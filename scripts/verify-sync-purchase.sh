#!/usr/bin/env bash
set -euo pipefail

base_url="${BASE_URL:-http://127.0.0.1:18080}"
demo_password="${LOCAL_DEMO_PASSWORD:?LOCAL_DEMO_PASSWORD is required}"
idempotency_key="demo-sync-purchase-v1"
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
  --data "$(jq -nc --arg password "$demo_password" '{username:"demo_customer",password:$password}')")"
expect_status "$status" 200 customer-login
token="$(jq -er '.data.access_token' "$work_dir/login.json")"

status="$(request POST /sync/purchases "$work_dir/purchase.json" \
  --header "Authorization: Bearer $token" \
  --header "Idempotency-Key: $idempotency_key" \
  --header 'Content-Type: application/json' --data '{"activity_id":1}')"
expect_status "$status" 201 synchronous-purchase
order_id="$(jq -er '.data.id' "$work_dir/purchase.json")"

status="$(request POST /sync/purchases "$work_dir/replay.json" \
  --header "Authorization: Bearer $token" \
  --header "Idempotency-Key: $idempotency_key" \
  --header 'Content-Type: application/json' --data '{"activity_id":1}')"
expect_status "$status" 201 idempotent-replay
jq -e --argjson order_id "$order_id" '.data.id == $order_id' "$work_dir/replay.json" >/dev/null

status="$(request POST /sync/purchases "$work_dir/repeat.json" \
  --header "Authorization: Bearer $token" \
  --header 'Idempotency-Key: demo-sync-purchase-other-key' \
  --header 'Content-Type: application/json' --data '{"activity_id":1}')"
expect_status "$status" 409 repeat-purchase-blocked

status="$(request GET "/orders/$order_id" "$work_dir/order.json" --header "Authorization: Bearer $token")"
expect_status "$status" 200 owned-order-readable
jq -e --argjson order_id "$order_id" '.data.id == $order_id and .data.status == "PENDING_PAYMENT"' "$work_dir/order.json" >/dev/null

unset token demo_password
echo "Synchronous purchase verification passed; tokens were not printed."
