#!/usr/bin/env bash
set -euo pipefail

base_url="${BASE_URL:-http://127.0.0.1:18080}"
demo_password="${LOCAL_DEMO_PASSWORD:?LOCAL_DEMO_PASSWORD is required}"
work_dir="$(mktemp -d)"
redis_started=0
cleanup() {
  rm -rf "$work_dir"
  if [[ "$redis_started" == "0" ]]; then
    docker compose up -d redis >/dev/null
  fi
}
trap cleanup EXIT

status="$(curl --silent --show-error --output "$work_dir/login.json" --write-out '%{http_code}' \
  --request POST "$base_url/auth/login" --header 'Content-Type: application/json' \
  --data "$(jq -nc --arg password "$demo_password" '{username:"demo_lua_customer",password:$password}')")"
[[ "$status" == "200" ]] || { echo "FAIL redis-outage-login: HTTP $status" >&2; exit 1; }
token="$(jq -er '.data.access_token' "$work_dir/login.json")"

docker compose stop redis >/dev/null
status="$(curl --silent --show-error --output "$work_dir/outage.json" --write-out '%{http_code}' \
  --request POST "$base_url/purchase-requests" \
  --header "Authorization: Bearer $token" \
  --header 'Idempotency-Key: demo-redis-down-v1' \
  --header 'Content-Type: application/json' --data '{"activity_id":4}')"
[[ "$status" == "503" ]] || { echo "FAIL redis-outage-fail-closed: HTTP $status" >&2; exit 1; }
echo "PASS redis-outage-fail-closed: HTTP 503"

docker compose up -d --wait redis >/dev/null
redis_started=1
persisted="$(docker compose exec -T -e MYSQL_PWD="${MYSQL_ROOT_PASSWORD:-local_root_only}" mysql \
  mysql -uroot --database="${MYSQL_DATABASE:-huixiang_life}" --batch --skip-column-names \
  -e "SELECT COUNT(*) FROM purchase_requests WHERE idempotency_key = 'demo-redis-down-v1'")"
[[ "$persisted" == "0" ]] || { echo "FAIL redis-outage-persistence: requests=$persisted" >&2; exit 1; }
echo "PASS redis-outage-persistence: no request written before failed rate limit"

unset token demo_password
echo "Redis fail-closed verification passed; existing MySQL facts were untouched."
