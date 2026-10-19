#!/usr/bin/env bash
set -euo pipefail

project_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$project_dir"

activity_id="${BENCHMARK_ACTIVITY_ID:-200}"
user_count="${BENCHMARK_USER_COUNT:-500}"
request_count="${BENCHMARK_REQUESTS:-$user_count}"
concurrency="${BENCHMARK_CONCURRENCY:-100}"
benchmark_timeout="${BENCHMARK_TIMEOUT:-5m}"
output_dir="${BENCHMARK_OUTPUT_DIR:-artifacts/measurements/$(date -u +%Y%m%dT%H%M%SZ)-async}"
tool_dir="$(mktemp -d /private/tmp/huixiang-async-benchmark.XXXXXX)"
benchmark_password="$(openssl rand -hex 24)"
api_pid=""
worker_pid=""

cleanup() {
  if [[ -n "$worker_pid" ]]; then kill -TERM "$worker_pid" 2>/dev/null || true; fi
  if [[ -n "$api_pid" ]]; then kill -TERM "$api_pid" 2>/dev/null || true; fi
  if [[ -n "$worker_pid" ]]; then wait "$worker_pid" 2>/dev/null || true; fi
  if [[ -n "$api_pid" ]]; then wait "$api_pid" 2>/dev/null || true; fi
  rm -rf "$tool_dir"
}
trap cleanup EXIT INT TERM

mkdir -p "$output_dir"
export GOCACHE="${GOCACHE:-/private/tmp/codex-local-life-gocache}"
export GOPROXY="${GOPROXY:-off}"

go build -o "$tool_dir/benchmark-seed" ./cmd/benchmark-seed
go build -o "$tool_dir/admission-init" ./cmd/admission-init
go build -o "$tool_dir/api" ./cmd/api
go build -o "$tool_dir/worker" ./cmd/worker
go build -o "$tool_dir/async-loadgen" ./cmd/async-loadgen

BENCHMARK_PASSWORD="$benchmark_password" BENCHMARK_USER_COUNT="$user_count" BENCHMARK_ACTIVITY_ID="$activity_id" \
  "$tool_dir/benchmark-seed" >"$output_dir/seed.log" 2>&1
ACTIVITY_ID="$activity_id" "$tool_dir/admission-init" >"$output_dir/admission-init.log" 2>&1

HTTP_ACCESS_LOG=0 GORM_SILENT=1 GIN_MODE=release RATE_LIMIT_TOTAL_CAPACITY=1024 \
  RATE_LIMIT_TOTAL_REFILL_PER_SECOND=1024 RATE_LIMIT_USER_CAPACITY=5 RATE_LIMIT_USER_REFILL_PER_SECOND=5 \
  "$tool_dir/api" >"$output_dir/api.log" 2>&1 &
api_pid=$!
HTTP_ACCESS_LOG=0 GORM_SILENT=1 "$tool_dir/worker" >"$output_dir/worker.log" 2>&1 &
worker_pid=$!

for _ in $(seq 1 100); do
  if curl -fsS http://127.0.0.1:18080/health >/dev/null; then break; fi
  sleep 0.1
done
curl -fsS http://127.0.0.1:18080/health >/dev/null

BENCHMARK_ACTIVITY_ID="$activity_id" BENCHMARK_USER_START=1 BENCHMARK_USER_COUNT="$user_count" \
  BENCHMARK_REQUESTS="$request_count" BENCHMARK_CONCURRENCY="$concurrency" BENCHMARK_TIMEOUT="$benchmark_timeout" \
  BENCHMARK_OUTPUT_DIR="$output_dir" BENCHMARK_PASSWORD="$benchmark_password" \
  "$tool_dir/async-loadgen"

printf 'async benchmark artifacts: %s\n' "$output_dir"
