#!/usr/bin/env bash
set -euo pipefail

project_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$project_dir"

duration_seconds="${BENCHMARK_DURATION_SECONDS:-60}"
warmup_seconds="${BENCHMARK_WARMUP_SECONDS:-10}"
rounds="${BENCHMARK_ROUNDS:-3}"
concurrency="${BENCHMARK_CONCURRENCY:-100}"
measurements_dir="${BENCHMARK_MEASUREMENTS_DIR:-artifacts/measurements}"
run_prefix="${BENCHMARK_RUN_PREFIX:-$(date -u +%Y%m%dT%H%M%SZ)-shop-full-body}"
manifest_dir="$measurements_dir/$run_prefix-manifest"
tool_dir="$(mktemp -d /private/tmp/huixiang-shop-benchmark.XXXXXX)"
api_pid=""

cleanup_api() {
  if [[ -n "$api_pid" ]]; then
    kill -TERM "$api_pid" 2>/dev/null || true
    wait "$api_pid" 2>/dev/null || true
    api_pid=""
  fi
}

cleanup() {
  cleanup_api
  rm -rf "$tool_dir"
}
trap cleanup EXIT INT TERM

mkdir -p "$manifest_dir"
export GOCACHE="${GOCACHE:-/private/tmp/codex-local-life-gocache}"
export GOPROXY="${GOPROXY:-off}"

go build -o "$tool_dir/benchmark-seed" ./cmd/benchmark-seed
go build -o "$tool_dir/api" ./cmd/api
go build -o "$tool_dir/shop-loadgen" ./cmd/shop-loadgen
"$tool_dir/benchmark-seed" >"$manifest_dir/seed.log" 2>&1

printf '{\n  "duration_seconds": %s,\n  "warmup_seconds": %s,\n  "rounds": %s,\n  "concurrency": %s,\n  "first_shop_id": 1000001,\n  "shop_count": 10000,\n  "hot_shop_count": 100,\n  "hot_percent": 80,\n  "timeout_milliseconds": 2000,\n  "latency_boundary": "before request until response body fully read and closed"\n}\n' \
  "$duration_seconds" "$warmup_seconds" "$rounds" "$concurrency" >"$manifest_dir/load-config.json"

run_mode() {
  local mode="$1"
  local mode_dir="$manifest_dir/$mode"
  mkdir -p "$mode_dir"
  HTTP_ACCESS_LOG=0 GORM_SILENT=1 GIN_MODE=release SHOP_CACHE_MODE="$mode" \
    "$tool_dir/api" >"$mode_dir/api.log" 2>&1 &
  api_pid=$!
  for _ in $(seq 1 100); do
    if curl -fsS http://127.0.0.1:18080/health >/dev/null; then break; fi
    sleep 0.1
  done
  curl -fsS http://127.0.0.1:18080/health >/dev/null

  RUN_ID="${mode}-warmup" DURATION_SECONDS="$warmup_seconds" CONCURRENCY="$concurrency" \
    FIRST_SHOP_ID=1000001 SHOP_COUNT=10000 HOT_SHOP_COUNT=100 HOT_PERCENT=80 TIMEOUT_MILLISECONDS=2000 \
    OUTPUT_DIR="$measurements_dir/$run_prefix-$mode-warmup" "$tool_dir/shop-loadgen" >"$mode_dir/warmup.stdout.json"
  curl -fsS http://127.0.0.1:18080/debug/cache-metrics >"$mode_dir/cache-metrics-after-warmup.json"

  for round in $(seq 1 "$rounds"); do
    run_id="${mode}-full-body-r${round}"
    RUN_ID="$run_id" DURATION_SECONDS="$duration_seconds" CONCURRENCY="$concurrency" \
      FIRST_SHOP_ID=1000001 SHOP_COUNT=10000 HOT_SHOP_COUNT=100 HOT_PERCENT=80 TIMEOUT_MILLISECONDS=2000 \
      OUTPUT_DIR="$measurements_dir/$run_prefix-$mode-r$round" "$tool_dir/shop-loadgen" >"$mode_dir/round-$round.stdout.json"
    curl -fsS http://127.0.0.1:18080/debug/cache-metrics >"$mode_dir/cache-metrics-after-round-$round.json"
  done
  cleanup_api
}

run_mode mysql
run_mode two-level

printf 'shop benchmark run prefix: %s\n' "$run_prefix"
