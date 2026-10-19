#!/usr/bin/env bash
# Collect reproducible, process-bound pprof evidence for the two-level shop-read path.
# This is intentionally separate from run-shop-benchmark.sh: diagnostic sampling changes
# runtime behaviour and every profile type gets a fresh API process and load interval.
set -euo pipefail

project_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$project_dir"

run_id="${PPROF_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-shop-profiling}"
measurements_dir="${PPROF_MEASUREMENTS_DIR:-artifacts/measurements}"
run_dir="$measurements_dir/$run_id"
tool_dir="$(mktemp -d /private/tmp/huixiang-shop-profiling.XXXXXX)"
api_pid=""
load_pid=""
active_load_pid=""

# These constants are part of the evidence contract. Do not silently compare a
# differently shaped load to a profiling run.
readonly warmup_seconds=10
readonly load_seconds=45
readonly sample_seconds=30
readonly sample_lead_seconds=5
readonly sample_tail_seconds=10
readonly concurrency=100
readonly first_shop_id=1000001
readonly shop_count=10000
readonly hot_shop_count=100
readonly hot_percent=80
readonly timeout_milliseconds=2000

app_addr="${APP_ADDR:-127.0.0.1:18080}"
pprof_addr="${PPROF_ADDR:-127.0.0.1:18081}"
mutex_fraction="${PPROF_MUTEX_FRACTION:-1}"
block_rate="${PPROF_BLOCK_RATE:-1000000}"
profile_kinds="${PPROF_PROFILE_KINDS:-cpu mutex block allocs heap}"

die() {
  printf 'run-shop-profiling: %s\n' "$*" >&2
  exit 1
}

utc_now() {
  date -u +%Y-%m-%dT%H:%M:%SZ
}

is_positive_integer() {
  [[ "$1" =~ ^[1-9][0-9]*$ ]]
}

require_loopback_address() {
  local value="$1"
  [[ "$value" =~ ^127\.0\.0\.1:([1-9][0-9]{0,4})$ ]] || die "address must be 127.0.0.1:<port>, got $value"
  local port="${value##*:}"
  (( port <= 65535 )) || die "port out of range: $value"
}

timeline() {
  # Event names and values are script-controlled, so JSON escaping is not needed here.
  printf '{"at_utc":"%s","event":"%s","component":"%s","pid":"%s"}\n' \
    "$(utc_now)" "$1" "$2" "${3:-}" >>"$run_dir/timeline.jsonl"
}

stop_api() {
  if [[ -n "$api_pid" ]]; then
    if kill -0 "$api_pid" 2>/dev/null; then
      timeline "api_stop_requested" "api" "$api_pid"
      kill -TERM "$api_pid" 2>/dev/null || true
    fi
    wait "$api_pid" 2>/dev/null || true
    timeline "api_stopped" "api" "$api_pid"
    api_pid=""
  fi
}

stop_load() {
  if [[ -n "$active_load_pid" ]]; then
    if kill -0 "$active_load_pid" 2>/dev/null; then
      timeline "load_stop_requested" "shop-loadgen" "$active_load_pid"
      kill -TERM "$active_load_pid" 2>/dev/null || true
    fi
    wait "$active_load_pid" 2>/dev/null || true
    active_load_pid=""
  fi
}

cleanup() {
  stop_load
  stop_api
  rm -rf "$tool_dir"
}
trap cleanup EXIT INT TERM

require_loopback_address "$app_addr"
require_loopback_address "$pprof_addr"
is_positive_integer "$mutex_fraction" || die "PPROF_MUTEX_FRACTION must be a positive integer for this diagnostic run"
is_positive_integer "$block_rate" || die "PPROF_BLOCK_RATE must be a positive integer for this diagnostic run"
(( load_seconds >= sample_lead_seconds + sample_seconds + sample_tail_seconds )) || die "invalid sample timing constants"
command -v curl >/dev/null || die "curl is required"
command -v lsof >/dev/null || die "lsof is required to bind the profile to the API PID"
command -v shasum >/dev/null || die "shasum is required"
[[ ! -e "$run_dir" ]] || die "refusing to overwrite existing evidence directory: $run_dir"

mkdir -p "$run_dir" "$run_dir/profiles" "$run_dir/rounds"
touch "$run_dir/timeline.jsonl"

export GOCACHE="${GOCACHE:-/private/tmp/codex-local-life-gocache}"
export GOPROXY="${GOPROXY:-off}"

go version >"$run_dir/go-version.txt"
go build -o "$tool_dir/benchmark-seed" ./cmd/benchmark-seed
go build -o "$tool_dir/api" ./cmd/api
go build -o "$tool_dir/shop-loadgen" ./cmd/shop-loadgen
shasum -a 256 "$tool_dir/api" >"$run_dir/api-binary.sha256"
go version -m "$tool_dir/api" >"$run_dir/api-go-version-m.txt"

# Deliberately do not serialize DSNs, passwords, or arbitrary environment values.
cat >"$run_dir/profiling-config.json" <<EOF
{
  "scenario": "two-level-hot-shop-read",
  "app_address": "$app_addr",
  "pprof_address": "$pprof_addr",
  "shop_cache_mode": "two-level",
  "http_access_log": false,
  "gorm_silent": true,
  "gin_mode": "release",
  "pprof_mutex_fraction": $mutex_fraction,
  "pprof_block_rate": $block_rate,
  "profile_kinds": "$profile_kinds",
  "warmup_seconds": $warmup_seconds,
  "load_seconds": $load_seconds,
  "sample_seconds": $sample_seconds,
  "sample_lead_seconds": $sample_lead_seconds,
  "concurrency": $concurrency,
  "first_shop_id": $first_shop_id,
  "shop_count": $shop_count,
  "hot_shop_count": $hot_shop_count,
  "hot_percent": $hot_percent,
  "timeout_milliseconds": $timeout_milliseconds,
  "latency_boundary": "before request until response body fully read and closed"
}
EOF

timeline "run_started" "harness"
"$tool_dir/benchmark-seed" >"$run_dir/seed.log" 2>&1
timeline "seed_finished" "benchmark-seed"

wait_for_api() {
  local port="${app_addr##*:}"
  local pprof_port="${pprof_addr##*:}"
  local process_file="$1"
  for _ in $(seq 1 100); do
    kill -0 "$api_pid" 2>/dev/null || die "API exited before becoming ready; see api.log"
    if curl -fsS "http://$app_addr/health" >"$2" 2>/dev/null && \
      lsof -nP -a -p "$api_pid" -iTCP:"$port" -sTCP:LISTEN >"$3" 2>/dev/null && \
      lsof -nP -a -p "$api_pid" -iTCP:"$pprof_port" -sTCP:LISTEN >"$4" 2>/dev/null; then
      ps -p "$api_pid" -o pid=,ppid=,lstart=,command= >"$process_file"
      curl -fsS "http://$pprof_addr/debug/pprof/" >"$5"
      return
    fi
    sleep 0.1
  done
  die "API PID $api_pid did not own both HTTP and pprof listeners"
}

start_api() {
  local round_dir="$1"
  {
    printf '%s\n' 'HTTP_ACCESS_LOG=0'
    printf '%s\n' 'GORM_SILENT=1'
    printf '%s\n' 'GIN_MODE=release'
    printf '%s\n' 'SHOP_CACHE_MODE=two-level'
    printf 'APP_ADDR=%s\n' "$app_addr"
    printf 'PPROF_ADDR=%s\n' "$pprof_addr"
    printf 'PPROF_MUTEX_FRACTION=%s\n' "$mutex_fraction"
    printf 'PPROF_BLOCK_RATE=%s\n' "$block_rate"
    printf '%s\n' 'MYSQL_PASSWORD=[redacted]'
    printf '%s\n' 'RABBITMQ_PASSWORD=[redacted]'
    printf '%s\n' 'API_EXECUTABLE=api (SHA-256 recorded at ../../api-binary.sha256)'
  } >"$round_dir/startup-config.redacted.txt"
  HTTP_ACCESS_LOG=0 GORM_SILENT=1 GIN_MODE=release SHOP_CACHE_MODE=two-level \
    APP_ADDR="$app_addr" PPROF_ADDR="$pprof_addr" \
    PPROF_MUTEX_FRACTION="$mutex_fraction" PPROF_BLOCK_RATE="$block_rate" \
    "$tool_dir/api" >"$round_dir/api.log" 2>&1 &
  api_pid=$!
  timeline "api_started" "api" "$api_pid"
  wait_for_api "$round_dir/process.txt" "$round_dir/health.json" "$round_dir/api-listener.txt" "$round_dir/pprof-listener.txt" "$round_dir/pprof-index.html"
  timeline "api_pprof_verified" "api" "$api_pid"
}

run_load() {
  local label="$1"
  local output_dir="$2"
  local log_dir="$3"
  local duration_seconds="$4"
  RUN_ID="$label" DURATION_SECONDS="$duration_seconds" CONCURRENCY="$concurrency" \
    FIRST_SHOP_ID="$first_shop_id" SHOP_COUNT="$shop_count" HOT_SHOP_COUNT="$hot_shop_count" HOT_PERCENT="$hot_percent" \
    TIMEOUT_MILLISECONDS="$timeout_milliseconds" OUTPUT_DIR="$output_dir" \
    "$tool_dir/shop-loadgen" >"$log_dir/load.stdout.json" 2>"$log_dir/load.stderr.log" &
  load_pid=$!
  active_load_pid="$load_pid"
}

run_warmup() {
  local round="$1"
  local round_dir="$run_dir/rounds/$round"
  local warmup_dir="$round_dir/warmup"
  mkdir -p "$warmup_dir"
  local warmup_pid
  run_load "$round-warmup" "$warmup_dir" "$round_dir" "$warmup_seconds"
  warmup_pid="$load_pid"
  timeline "warmup_started" "shop-loadgen" "$warmup_pid"
  wait "$warmup_pid"
  active_load_pid=""
  timeline "warmup_finished" "shop-loadgen" "$warmup_pid"
  [[ -s "$warmup_dir/summary.json" && -s "$warmup_dir/raw-results.jsonl" ]] || die "warmup artifacts missing for $round"
}

capture_profile() {
  local kind="$1"
  local output="$2"
  local url
  case "$kind" in
    cpu) url="http://$pprof_addr/debug/pprof/profile?seconds=$sample_seconds" ;;
    mutex|block|allocs|heap) url="http://$pprof_addr/debug/pprof/$kind?seconds=$sample_seconds" ;;
    *) die "unknown profile kind: $kind" ;;
  esac
  timeline "profile_started_$kind" "api" "$api_pid"
  curl -fsS "$url" -o "$output"
  timeline "profile_finished_$kind" "api" "$api_pid"
  [[ -s "$output" ]] || die "empty $kind profile"
}

run_round() {
  local kind="$1"
  local round_dir="$run_dir/rounds/$kind"
  local profile_dir="$run_dir/profiles/$kind"
  local load_dir="$round_dir/load"
  mkdir -p "$round_dir" "$profile_dir" "$load_dir"

  start_api "$round_dir"
  run_warmup "$kind"

  # Heap snapshots are explicit baseline/final snapshots. Both force the same
  # GC condition; the separate heap delta is retained only as time-window context.
  if [[ "$kind" == "heap" ]]; then
    timeline "heap_before_started" "api" "$api_pid"
    curl -fsS "http://$pprof_addr/debug/pprof/heap?gc=1" -o "$profile_dir/heap.before.gc1.pb.gz"
    curl -fsS "http://$pprof_addr/debug/pprof/heap?debug=1" -o "$profile_dir/heap.before.debug.txt"
    ps -p "$api_pid" -o pid=,rss=,vsz=,etime= >"$profile_dir/process-memory.before.txt"
    timeline "heap_before_finished" "api" "$api_pid"
  fi

  run_load "$kind-load" "$load_dir" "$round_dir" "$load_seconds"
  local round_load_pid="$load_pid"
  timeline "load_started" "shop-loadgen" "$round_load_pid"
  sleep "$sample_lead_seconds"
  kill -0 "$round_load_pid" 2>/dev/null || die "$kind load ended before profile sampling started"
  capture_profile "$kind" "$profile_dir/$kind.window-${sample_seconds}s.pb.gz"
  kill -0 "$round_load_pid" 2>/dev/null || die "$kind load ended before profile sampling finished"
  wait "$round_load_pid"
  active_load_pid=""
  timeline "load_finished" "shop-loadgen" "$round_load_pid"
  [[ -s "$load_dir/summary.json" && -s "$load_dir/raw-results.jsonl" ]] || die "load artifacts missing for $kind"

  if [[ "$kind" == "heap" ]]; then
    timeline "heap_after_started" "api" "$api_pid"
    curl -fsS "http://$pprof_addr/debug/pprof/heap?gc=1" -o "$profile_dir/heap.after.gc1.pb.gz"
    curl -fsS "http://$pprof_addr/debug/pprof/heap?debug=1" -o "$profile_dir/heap.after.debug.txt"
    ps -p "$api_pid" -o pid=,rss=,vsz=,etime= >"$profile_dir/process-memory.after.txt"
    timeline "heap_after_finished" "api" "$api_pid"
  fi

  stop_api
}

for kind in $profile_kinds; do
  case "$kind" in
    cpu|mutex|block|allocs|heap) ;;
    *) die "PPROF_PROFILE_KINDS contains unsupported kind: $kind" ;;
  esac
  run_round "$kind"
done

timeline "run_finished" "harness"
(
  cd "$run_dir"
  find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 shasum -a 256 >SHA256SUMS
)
printf 'shop profiling artifacts: %s\n' "$run_dir"
