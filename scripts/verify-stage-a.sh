#!/usr/bin/env bash
set -euo pipefail

base_url="${BASE_URL:-http://127.0.0.1:18080}"
demo_password="${LOCAL_DEMO_PASSWORD:?LOCAL_DEMO_PASSWORD is required}"
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

status="$(request GET /health "$work_dir/health.json")"
expect_status "$status" 200 health
jq -e '.status == "ok"' "$work_dir/health.json" >/dev/null

status="$(request GET '/shops?page=1&page_size=10' "$work_dir/shops.json")"
expect_status "$status" 200 shop-list
jq -e '.data.total >= 2' "$work_dir/shops.json" >/dev/null

status="$(request GET /shops/999999 "$work_dir/missing.json")"
expect_status "$status" 404 shop-not-found
status="$(request GET '/shops?page=0' "$work_dir/invalid.json")"
expect_status "$status" 400 invalid-pagination

status="$(request POST /auth/login "$work_dir/wrong-login.json" \
  --header 'Content-Type: application/json' \
  --data '{"username":"demo_customer","password":"definitely-wrong"}')"
expect_status "$status" 401 wrong-password

status="$(request POST /auth/login "$work_dir/customer-login.json" \
  --header 'Content-Type: application/json' \
  --data "$(jq -nc --arg password "$demo_password" '{username:"demo_customer",password:$password}')")"
expect_status "$status" 200 customer-login
customer_token="$(jq -er '.data.access_token' "$work_dir/customer-login.json")"

status="$(request GET /me "$work_dir/me.json" --header "Authorization: Bearer $customer_token")"
expect_status "$status" 200 authenticated-me
jq -e '.data.username == "demo_customer"' "$work_dir/me.json" >/dev/null

update_payload='{"name":"翠湖书店·已更新","address":"五华区翠湖北路 1 号","status":"OPEN"}'
status="$(request PATCH /merchant/shops/1 "$work_dir/customer-update.json" \
  --header "Authorization: Bearer $customer_token" \
  --header 'Content-Type: application/json' --data "$update_payload")"
expect_status "$status" 403 customer-cannot-update-shop
unset customer_token

status="$(request POST /auth/login "$work_dir/merchant-login.json" \
  --header 'Content-Type: application/json' \
  --data "$(jq -nc --arg password "$demo_password" '{username:"demo_merchant",password:$password}')")"
expect_status "$status" 200 merchant-login
merchant_token="$(jq -er '.data.access_token' "$work_dir/merchant-login.json")"

status="$(request PATCH /merchant/shops/1 "$work_dir/merchant-update.json" \
  --header "Authorization: Bearer $merchant_token" \
  --header 'Content-Type: application/json' --data "$update_payload")"
expect_status "$status" 200 merchant-updates-owned-shop
jq -e '.data.name == "翠湖书店·已更新"' "$work_dir/merchant-update.json" >/dev/null

status="$(request PATCH /merchant/shops/2 "$work_dir/forbidden-shop.json" \
  --header "Authorization: Bearer $merchant_token" \
  --header 'Content-Type: application/json' \
  --data '{"name":"不应写入","address":"不应写入","status":"OPEN"}')"
expect_status "$status" 404 merchant-cannot-update-other-shop
unset merchant_token demo_password

echo "Stage A HTTP verification passed; bearer tokens were not printed."
