#!/usr/bin/env bash
set -euo pipefail

base_url="${BASE_URL:-http://127.0.0.1:18080}"
demo_password="${LOCAL_DEMO_PASSWORD:?LOCAL_DEMO_PASSWORD is required}"
work_dir="$(mktemp -d)"
rabbit_started=0
cleanup() {
  rm -rf "$work_dir"
  if [[ "$rabbit_started" == "0" ]]; then
    docker compose up -d rabbitmq >/dev/null
  fi
}
trap cleanup EXIT

status="$(curl --silent --show-error --output "$work_dir/login.json" --write-out '%{http_code}' \
  --request POST "$base_url/auth/login" --header 'Content-Type: application/json' \
  --data "$(jq -nc --arg password "$demo_password" '{username:"demo_recovery_customer",password:$password}')")"
[[ "$status" == "200" ]] || { echo "FAIL recovery-login: HTTP $status" >&2; exit 1; }
token="$(jq -er '.data.access_token' "$work_dir/login.json")"

docker compose stop rabbitmq >/dev/null
status="$(curl --silent --show-error --output "$work_dir/accepted.json" --write-out '%{http_code}' \
  --request POST "$base_url/purchase-requests" \
  --header "Authorization: Bearer $token" \
  --header 'Idempotency-Key: demo-rabbit-restart-v1' \
  --header 'Content-Type: application/json' --data '{"activity_id":3}')"
[[ "$status" == "202" ]] || { echo "FAIL durable-accept-during-rabbit-outage: HTTP $status" >&2; exit 1; }
request_id="$(jq -er '.data.request_id' "$work_dir/accepted.json")"
echo "PASS durable-accept-during-rabbit-outage: HTTP 202"

docker compose up -d --wait rabbitmq >/dev/null
rabbit_started=1

request_status=""
for _ in {1..50}; do
  status="$(curl --silent --show-error --output "$work_dir/status.json" --write-out '%{http_code}' \
    "$base_url/purchase-requests/$request_id" --header "Authorization: Bearer $token")"
  [[ "$status" == "200" ]] || { echo "FAIL recovery-status: HTTP $status" >&2; exit 1; }
  request_status="$(jq -er '.data.status' "$work_dir/status.json")"
  [[ "$request_status" == "SUCCEEDED" || "$request_status" == "FAILED" ]] && break
  sleep 0.2
done
[[ "$request_status" == "SUCCEEDED" ]] || { echo "FAIL recovered-request: status=$request_status" >&2; exit 1; }
echo "PASS recovered-request: SUCCEEDED"

outbox_status=""
for _ in {1..50}; do
  outbox_status="$(docker compose exec -T \
    -e MYSQL_PWD="${MYSQL_ROOT_PASSWORD:-local_root_only}" mysql \
    mysql -uroot --database="${MYSQL_DATABASE:-huixiang_life}" --batch --skip-column-names \
    -e "SELECT status FROM outbox_events WHERE aggregate_id = '$request_id'")"
  [[ "$outbox_status" == "SENT" ]] && break
  sleep 0.2
done
[[ "$outbox_status" == "SENT" ]] || { echo "FAIL recovered-outbox: status=$outbox_status" >&2; exit 1; }
echo "PASS recovered-outbox: SENT"

unset token demo_password
echo "RabbitMQ restart verification passed; the durable request remained queryable and no token was printed."
