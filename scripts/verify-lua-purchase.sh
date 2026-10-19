#!/usr/bin/env bash
set -euo pipefail

base_url="${BASE_URL:-http://127.0.0.1:18080}"
demo_password="${LOCAL_DEMO_PASSWORD:?LOCAL_DEMO_PASSWORD is required}"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

status="$(curl --silent --show-error --output "$work_dir/login.json" --write-out '%{http_code}' \
  --request POST "$base_url/auth/login" --header 'Content-Type: application/json' \
  --data "$(jq -nc --arg password "$demo_password" '{username:"demo_lua_customer",password:$password}')")"
[[ "$status" == "200" ]] || { echo "FAIL lua-customer-login: HTTP $status" >&2; exit 1; }
token="$(jq -er '.data.access_token' "$work_dir/login.json")"

status="$(curl --silent --show-error --output "$work_dir/accepted.json" --write-out '%{http_code}' \
  --request POST "$base_url/purchase-requests" \
  --header "Authorization: Bearer $token" \
  --header 'Idempotency-Key: demo-lua-purchase-v1' \
  --header 'Content-Type: application/json' --data '{"activity_id":4}')"
[[ "$status" == "202" ]] || { echo "FAIL lua-admission: HTTP $status" >&2; exit 1; }
request_id="$(jq -er '.data.request_id' "$work_dir/accepted.json")"
echo "PASS lua-admission: HTTP 202"

request_status=""
for _ in {1..40}; do
  status="$(curl --silent --show-error --output "$work_dir/status.json" --write-out '%{http_code}' \
    "$base_url/purchase-requests/$request_id" --header "Authorization: Bearer $token")"
  [[ "$status" == "200" ]] || { echo "FAIL lua-status: HTTP $status" >&2; exit 1; }
  request_status="$(jq -er '.data.status' "$work_dir/status.json")"
  [[ "$request_status" == "SUCCEEDED" || "$request_status" == "FAILED" ]] && break
  sleep 0.2
done
[[ "$request_status" == "SUCCEEDED" ]] || { echo "FAIL lua-terminal: status=$request_status" >&2; exit 1; }
echo "PASS lua-terminal: SUCCEEDED"

status="$(curl --silent --show-error --output "$work_dir/replay.json" --write-out '%{http_code}' \
  --request POST "$base_url/purchase-requests" \
  --header "Authorization: Bearer $token" \
  --header 'Idempotency-Key: demo-lua-purchase-v1' \
  --header 'Content-Type: application/json' --data '{"activity_id":4}')"
[[ "$status" == "202" ]] || { echo "FAIL lua-idempotent-replay: HTTP $status" >&2; exit 1; }
jq -e --arg request_id "$request_id" '.data.request_id == $request_id and .data.status == "SUCCEEDED"' "$work_dir/replay.json" >/dev/null
echo "PASS lua-idempotent-replay: same request"

rate_limited=0
for index in {1..8}; do
  status="$(curl --silent --show-error --output "$work_dir/rate-$index.json" --write-out '%{http_code}' \
    --request POST "$base_url/purchase-requests" \
    --header "Authorization: Bearer $token" \
    --header "Idempotency-Key: demo-rate-$index" \
    --header 'Content-Type: application/json' --data '{"activity_id":4}')"
  if [[ "$status" == "429" ]]; then
    rate_limited=$((rate_limited + 1))
  elif [[ "$status" != "409" ]]; then
    echo "FAIL rate-limit-probe-$index: HTTP $status" >&2
    exit 1
  fi
done
[[ "$rate_limited" -gt 0 ]] || { echo "FAIL rate-limit: no HTTP 429 observed" >&2; exit 1; }
echo "PASS rate-limit: observed $rate_limited HTTP 429 responses"

unset token demo_password
echo "Lua admission and rate-limit verification passed; tokens were not printed."
