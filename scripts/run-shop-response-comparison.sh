#!/usr/bin/env bash
# Compare two API binaries under the same cache-hot HTTP workload. Diagnostic
# profiling is explicitly disabled so the result measures normal operation.
set -euo pipefail

project_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$project_dir"

baseline_api="${BASELINE_API_BINARY:?BASELINE_API_BINARY is required}"
optimized_api="${OPTIMIZED_API_BINARY:?OPTIMIZED_API_BINARY is required}"
run_id="${COMPARISON_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-shop-response-comparison}"
run_dir="${COMPARISON_MEASUREMENTS_DIR:-artifacts/measurements}/$run_id"
duration_seconds="${COMPARISON_DURATION_SECONDS:-60}"
warmup_seconds="${COMPARISON_WARMUP_SECONDS:-10}"
rounds="${COMPARISON_ROUNDS:-3}"
concurrency="${COMPARISON_CONCURRENCY:-100}"
tool_dir="$(mktemp -d /private/tmp/huixiang-shop-comparison.XXXXXX)"
api_pid=""

die() {
  printf 'run-shop-response-comparison: %s\n' "$*" >&2
  exit 1
}

stop_api() {
  if [[ -n "$api_pid" ]]; then
    kill -TERM "$api_pid" 2>/dev/null || true
    wait "$api_pid" 2>/dev/null || true
    api_pid=""
  fi
}

cleanup() {
  stop_api
  rm -rf "$tool_dir"
}
trap cleanup EXIT INT TERM

[[ -x "$baseline_api" ]] || die "baseline API is not executable: $baseline_api"
[[ -x "$optimized_api" ]] || die "optimized API is not executable: $optimized_api"
[[ ! -e "$run_dir" ]] || die "refusing to overwrite existing evidence directory: $run_dir"
[[ "$duration_seconds" =~ ^[1-9][0-9]*$ ]] || die "duration must be positive"
[[ "$warmup_seconds" =~ ^[1-9][0-9]*$ ]] || die "warmup must be positive"
[[ "$rounds" =~ ^[1-9][0-9]*$ ]] || die "rounds must be positive"
[[ "$concurrency" =~ ^[1-9][0-9]*$ ]] || die "concurrency must be positive"

mkdir -p "$run_dir"
export GOCACHE="${GOCACHE:-/private/tmp/codex-local-life-gocache}"
export GOPROXY="${GOPROXY:-off}"
go build -o "$tool_dir/benchmark-seed" ./cmd/benchmark-seed
go build -o "$tool_dir/shop-loadgen" ./cmd/shop-loadgen
"$tool_dir/benchmark-seed" >"$run_dir/seed.log" 2>&1

shasum -a 256 "$baseline_api" >"$run_dir/baseline-api.sha256"
shasum -a 256 "$optimized_api" >"$run_dir/optimized-api.sha256"
go version -m "$baseline_api" >"$run_dir/baseline-api-go-version-m.txt"
go version -m "$optimized_api" >"$run_dir/optimized-api-go-version-m.txt"
printf '{\n  "scenario": "typed-detail-response",\n  "duration_seconds": %s,\n  "warmup_seconds": %s,\n  "rounds": %s,\n  "concurrency": %s,\n  "shop_cache_mode": "two-level",\n  "pprof_mutex_fraction": 0,\n  "pprof_block_rate": 0,\n  "first_shop_id": 1000001,\n  "shop_count": 10000,\n  "hot_shop_count": 100,\n  "hot_percent": 80,\n  "timeout_milliseconds": 2000,\n  "latency_boundary": "before request until response body fully read and closed"\n}\n' \
  "$duration_seconds" "$warmup_seconds" "$rounds" "$concurrency" >"$run_dir/config.json"

wait_for_api() {
  for _ in $(seq 1 100); do
    kill -0 "$api_pid" 2>/dev/null || die "API exited before health check"
    if curl -fsS http://127.0.0.1:18080/health >/dev/null 2>&1; then
      return
    fi
    sleep 0.1
  done
  die "API did not become healthy"
}

run_variant() {
  local variant="$1"
  local binary="$2"
  local round="$3"
  local variant_dir="$run_dir/$variant-r$round"
  mkdir -p "$variant_dir/warmup" "$variant_dir/load"

  HTTP_ACCESS_LOG=0 GORM_SILENT=1 GIN_MODE=release SHOP_CACHE_MODE=two-level \
    PPROF_MUTEX_FRACTION=0 PPROF_BLOCK_RATE=0 \
    "$binary" >"$variant_dir/api.log" 2>&1 &
  api_pid=$!
  wait_for_api
  ps -p "$api_pid" -o pid=,ppid=,lstart=,command= >"$variant_dir/process.txt"

  RUN_ID="$variant-r$round-warmup" DURATION_SECONDS="$warmup_seconds" CONCURRENCY="$concurrency" \
    FIRST_SHOP_ID=1000001 SHOP_COUNT=10000 HOT_SHOP_COUNT=100 HOT_PERCENT=80 TIMEOUT_MILLISECONDS=2000 \
    OUTPUT_DIR="$variant_dir/warmup" "$tool_dir/shop-loadgen" >"$variant_dir/warmup.stdout.json"
  curl -fsS http://127.0.0.1:18080/debug/cache-metrics >"$variant_dir/cache-metrics.after-warmup.json"

  RUN_ID="$variant-r$round-load" DURATION_SECONDS="$duration_seconds" CONCURRENCY="$concurrency" \
    FIRST_SHOP_ID=1000001 SHOP_COUNT=10000 HOT_SHOP_COUNT=100 HOT_PERCENT=80 TIMEOUT_MILLISECONDS=2000 \
    OUTPUT_DIR="$variant_dir/load" "$tool_dir/shop-loadgen" >"$variant_dir/load.stdout.json"
  curl -fsS http://127.0.0.1:18080/debug/cache-metrics >"$variant_dir/cache-metrics.after-load.json"
  stop_api
}

for round in $(seq 1 "$rounds"); do
  if (( round % 2 == 1 )); then
    run_variant baseline "$baseline_api" "$round"
    run_variant optimized "$optimized_api" "$round"
  else
    run_variant optimized "$optimized_api" "$round"
    run_variant baseline "$baseline_api" "$round"
  fi
done

(
  cd "$run_dir"
  find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 shasum -a 256 >SHA256SUMS
)
printf 'shop response comparison artifacts: %s\n' "$run_dir"
