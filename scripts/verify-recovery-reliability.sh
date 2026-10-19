#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

# All writes go to new, disposable instances. In particular, never use Compose
# down -v: the project's existing named data volumes must remain untouched.
run_id="$(date +%s)-$$"
mysql_container="huixiang-recovery-check-mysql-$run_id"
redis_container="huixiang-recovery-check-redis-$run_id"
mysql_created=0
redis_created=0
cleanup() {
  if [[ "$redis_created" == 1 ]]; then
    docker rm -f "$redis_container" >/dev/null || true
  fi
  if [[ "$mysql_created" == 1 ]]; then
    docker rm -f "$mysql_container" >/dev/null || true
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

docker compose config --format json | python3 -c '
import json, sys
mounts = json.load(sys.stdin)["services"]["mysql"].get("volumes", [])
assert not any(m["target"].startswith("/docker-entrypoint-initdb.d") for m in mounts), "MySQL must not auto-apply migration SQL"
'

echo 'Starting isolated MySQL and Redis with tmpfs data directories...'
docker run --rm --detach --name "$mysql_container" --tmpfs /var/lib/mysql \
  --publish 127.0.0.1::3306 \
  --env MYSQL_ROOT_PASSWORD=local_recovery_test_only \
  --env MYSQL_DATABASE=huixiang_recovery_test \
  --env MYSQL_USER=huixiang --env MYSQL_PASSWORD=local_recovery_test_only \
  mysql:8.4 >/dev/null
mysql_created=1
docker run --rm --detach --name "$redis_container" --tmpfs /data \
  --publish 127.0.0.1::6379 redis:7.4-alpine \
  redis-server --save '' --appendonly no >/dev/null
redis_created=1

mysql_mapping="$(docker port "$mysql_container" 3306/tcp)"
redis_mapping="$(docker port "$redis_container" 6379/tcp)"
export MYSQL_HOST=127.0.0.1 MYSQL_PORT="${mysql_mapping##*:}"
export MYSQL_DATABASE=huixiang_recovery_test MYSQL_USER=huixiang
export MYSQL_PASSWORD=local_recovery_test_only
export REDIS_HOST=127.0.0.1 REDIS_PORT="${redis_mapping##*:}"
export LOCAL_LIFE_REDIS_ADDR="127.0.0.1:$REDIS_PORT"
export LOCAL_LIFE_INTEGRATION=1
export LOCAL_LIFE_MIGRATION_TEST_DSN="root:local_recovery_test_only@tcp(127.0.0.1:$MYSQL_PORT)/?parseTime=true"

ready=0
for ((attempt=0; attempt<60; attempt++)); do
  if docker exec "$mysql_container" sh -c 'MYSQL_PWD="$MYSQL_PASSWORD" mysql --protocol=TCP -h127.0.0.1 -u"$MYSQL_USER" "$MYSQL_DATABASE" -e "SELECT 1"' >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done
if [[ "$ready" != 1 ]]; then
  echo 'Temporary MySQL did not become ready.' >&2
  docker logs --tail 20 "$mysql_container" >&2
  exit 1
fi

echo 'Verifying migration history, legacy-schema refusal, and safe replay...'
go test -race -count=1 -v ./internal/platform/migrate
go run ./cmd/migrate
go run ./cmd/migrate

echo 'Verifying recovery quarantine, lease ownership, and Redis crash replay...'
go test -race -count=1 -v ./internal/shop ./internal/coupon ./internal/order ./cmd/worker
echo 'Recovery reliability checks passed; removing only these temporary containers.'
