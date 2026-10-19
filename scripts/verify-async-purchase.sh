#!/usr/bin/env bash
set -euo pipefail

base_url="${BASE_URL:-http://127.0.0.1:18080}"
demo_password="${LOCAL_DEMO_PASSWORD:?LOCAL_DEMO_PASSWORD is required}"
idempotency_key="demo-async-purchase-v1"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

request() {
  local method="$1"
  local path="$2"
  local output="$3"
  shift 3
  curl --silent --show-error --output "$output" --write-out '%{http_code}' \
    --request "$method" "$base_url$path" "$@"
}

expect_status() {
  local actual="$1"
  local expected="$2"
  local label="$3"
  if [[ "$actual" != "$expected" ]]; then
    echo "FAIL $label: HTTP $actual, want $expected" >&2
    exit 1
  fi
  echo "PASS $label: HTTP $actual"
}

status="$(request POST /auth/login "$work_dir/login.json" \
  --header 'Content-Type: application/json' \
  --data "$(jq -nc --arg password "$demo_password" '{username:"demo_async_customer",password:$password}')")"
expect_status "$status" 200 async-customer-login
token="$(jq -er '.data.access_token' "$work_dir/login.json")"

status="$(request POST /purchase-requests "$work_dir/accepted.json" \
  --header "Authorization: Bearer $token" \
  --header "Idempotency-Key: $idempotency_key" \
  --header 'Content-Type: application/json' --data '{"activity_id":2}')"
expect_status "$status" 202 async-request-persisted
request_id="$(jq -er '.data.request_id' "$work_dir/accepted.json")"
jq -e '.data.status == "ADMITTED" or .data.status == "SUCCEEDED"' "$work_dir/accepted.json" >/dev/null

terminal=""
for _ in {1..30}; do
  status="$(request GET "/purchase-requests/$request_id" "$work_dir/status.json" \
    --header "Authorization: Bearer $token")"
  expect_status "$status" 200 async-status-readable >/dev/null
  terminal="$(jq -er '.data.status' "$work_dir/status.json")"
  if [[ "$terminal" == "SUCCEEDED" || "$terminal" == "FAILED" ]]; then
    break
  fi
  sleep 0.2
done
if [[ "$terminal" != "SUCCEEDED" ]]; then
  echo "FAIL async-request-terminal: status=$terminal" >&2
  exit 1
fi
order_id="$(jq -er '.data.order_id' "$work_dir/status.json")"
echo "PASS async-request-terminal: SUCCEEDED"

status="$(request POST /purchase-requests "$work_dir/replay.json" \
  --header "Authorization: Bearer $token" \
  --header "Idempotency-Key: $idempotency_key" \
  --header 'Content-Type: application/json' --data '{"activity_id":2}')"
expect_status "$status" 202 async-idempotent-replay
jq -e --arg request_id "$request_id" --argjson order_id "$order_id" \
  '.data.request_id == $request_id and .data.order_id == $order_id and .data.status == "SUCCEEDED"' \
  "$work_dir/replay.json" >/dev/null

status="$(request GET "/orders/$order_id" "$work_dir/order.json" \
  --header "Authorization: Bearer $token")"
expect_status "$status" 200 async-order-readable
jq -e --argjson order_id "$order_id" \
  '.data.id == $order_id and .data.status == "PENDING_PAYMENT"' "$work_dir/order.json" >/dev/null

unset token demo_password
echo "Asynchronous purchase verification passed; tokens were not printed."
