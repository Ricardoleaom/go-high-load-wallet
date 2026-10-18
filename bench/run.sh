#!/usr/bin/env bash
# Runs one load scenario against a freshly built, freshly seeded stack in a
# separate compose project (wallet-bench), so development data is never
# touched, and collects everything a benchmark report needs.
#
#   SCENARIO=uniform-mix MODE=steady RATE=200 DURATION=5m REPEAT=3 bench/run.sh
#
# Knee mode reads KNEE_START, KNEE_STEP, KNEE_MAX and KNEE_STEP_DURATION.
# Any app setting (DB_MAX_OPEN_CONNS, OUTBOX_BATCH_SIZE, ...) passes through.
# Exits 1 when any repeat's k6 run or invariant check failed.
set -euo pipefail

cd "$(dirname "$0")/.."

SCENARIO=${SCENARIO:?set SCENARIO, e.g. uniform-mix}
MODE=${MODE:-steady}
RATE=${RATE:-50}
DURATION=${DURATION:-5m}
REPEAT=${REPEAT:-1}
WARMUP=${WARMUP:-60s}
WALLETS=${WALLETS:-100000}
KNEE_START=${KNEE_START:-50}
KNEE_STEP=${KNEE_STEP:-50}
KNEE_MAX=${KNEE_MAX:-600}
KNEE_STEP_DURATION=${KNEE_STEP_DURATION:-60s}
POSTGRES_HOST_PORT=${POSTGRES_HOST_PORT:-5432}
LABEL=${LABEL:-}

export SCENARIO MODE RATE DURATION REPEAT WARMUP WALLETS KNEE_START KNEE_STEP KNEE_MAX KNEE_STEP_DURATION LABEL
export POSTGRES_HOST_PORT
export HOST_UID HOST_GID
HOST_UID=$(id -u)
HOST_GID=$(id -g)
export OTEL_EXPORTER_OTLP_ENDPOINT=${OTEL_EXPORTER_OTLP_ENDPOINT-http://jaeger:4318}

DATABASE_URL="postgres://app:local_dev_password@localhost:${POSTGRES_HOST_PORT}/wallet?sslmode=disable"
PROJECT=wallet-bench

[ -f "bench/k6/scenarios/${SCENARIO}.js" ] || { echo "unknown scenario ${SCENARIO}" >&2; exit 2; }

compose() {
  docker compose -p "$PROJECT" -f compose.yaml -f compose.bench.yaml --profile obs --profile bench "$@"
}

# seconds 90s|5m|1h -> integer seconds
seconds() {
  local value=$1
  case "$value" in
    *h) echo $(( ${value%h} * 3600 )) ;;
    *m) echo $(( ${value%m} * 60 )) ;;
    *s) echo "${value%s}" ;;
    *) echo "$value" ;;
  esac
}

for port in 8080 8081 4566 6060 9090 16686 "$POSTGRES_HOST_PORT"; do
  if ss -ltn "( sport = :${port} )" | grep -q LISTEN; then
    echo "port ${port} is in use; stop the development stack (make down) or set POSTGRES_HOST_PORT" >&2
    exit 2
  fi
done

cleanup() { compose down -v --remove-orphans > /dev/null 2>&1 || true; }
trap cleanup EXIT

# k6 copies whatever setup() returns into the summary, and setup() hands
# the VUs their access tokens: drop it so no result file carries a token.
scrub_summary() {
  [ -s "$1" ] || return 0
  python3 -c '
import json, sys
path = sys.argv[1]
with open(path) as f:
    summary = json.load(f)
summary.pop("setup_data", None)
with open(path, "w") as f:
    json.dump(summary, f, indent=2)
' "$1"
}

if [ "$MODE" = knee ]; then
  steps=$(( (KNEE_MAX - KNEE_START) / KNEE_STEP + 1 ))
  run_seconds=$(( steps * ($(seconds "$KNEE_STEP_DURATION") + 5) ))
  warm_rate=$(( KNEE_START / 10 ))
  # Profile the last full step, where the load is highest.
  profile_at=$(( run_seconds - $(seconds "$KNEE_STEP_DURATION") + 10 ))
else
  run_seconds=$(seconds "$DURATION")
  warm_rate=$(( RATE / 10 ))
  profile_at=$(( run_seconds / 2 - 15 ))
fi
[ "$warm_rate" -ge 5 ] || warm_rate=5
[ "$profile_at" -ge 5 ] || profile_at=5
profile_seconds=30
[ "$run_seconds" -ge 60 ] || profile_seconds=$(( run_seconds / 3 ))

status=0
for run in $(seq 1 "$REPEAT"); do
  suffix=""
  [ "$REPEAT" -gt 1 ] && suffix="-r${run}"
  name="$(date -u +%Y%m%dT%H%M%SZ)-${SCENARIO}-${MODE}${LABEL:+-$LABEL}${suffix}"
  dir="bench/results/${name}"
  mkdir -p "$dir" bench/data
  echo "== ${name}"

  cleanup
  compose up -d --build --scale grafana=0 > "$dir/compose-up.log" 2>&1
  for _ in $(seq 1 120); do
    curl -fsS localhost:8080/health/ready > /dev/null 2>&1 && break
    sleep 2
  done
  curl -fsS localhost:8080/health/ready > /dev/null

  go run ./cmd/seed -database-url "$DATABASE_URL" -wallets "$WALLETS" -truncate | tee "$dir/seed.txt"
  go run ./cmd/verify -database-url "$DATABASE_URL" > "$dir/verify-before.json"

  k6_args=(-e "MODE=$MODE" -e "RATE=$RATE" -e "DURATION=$DURATION"
    -e "KNEE_START=$KNEE_START" -e "KNEE_STEP=$KNEE_STEP" -e "KNEE_MAX=$KNEE_MAX"
    -e "KNEE_STEP_DURATION=$KNEE_STEP_DURATION")
  for passthrough in MIX ZIPF_S REPLAY_RATIO AMOUNT_MIN AMOUNT_MAX; do
    [ -n "${!passthrough:-}" ] && k6_args+=(-e "${passthrough}=${!passthrough}")
  done

  echo "   warmup ${WARMUP} at ${warm_rate}/s"
  compose run --rm k6 run -q --no-thresholds -e MODE=steady -e "RATE=$warm_rate" -e "DURATION=$WARMUP" \
    "/scripts/scenarios/${SCENARIO}.js" > "$dir/warmup.log" 2>&1 || true

  started=$(date -u +%s)
  (
    while sleep 5; do
      docker stats --no-stream --format "{{.Name}},{{.CPUPerc}},{{.MemUsage}}" 2> /dev/null \
        | sed "s/^/$(date -u +%s),/"
    done
  ) > "$dir/docker-stats.csv" &
  stats_pid=$!
  (
    sleep "$profile_at"
    curl -fsS "localhost:6060/debug/pprof/profile?seconds=${profile_seconds}" -o "$dir/cpu.pprof" || true
    curl -fsS localhost:6060/debug/pprof/heap -o "$dir/heap.pprof" || true
  ) &
  profile_pid=$!

  echo "   ${MODE} run (${run_seconds}s)"
  set +e
  compose run --rm k6 run -o experimental-prometheus-rw \
    --summary-export "/results/${name}/summary.json" "${k6_args[@]}" \
    "/scripts/scenarios/${SCENARIO}.js" > "$dir/k6.log" 2>&1
  k6_exit=$?
  set -e
  finished=$(date -u +%s)
  scrub_summary "$dir/summary.json"

  wait "$profile_pid" || true
  kill "$stats_pid" 2> /dev/null || true
  wait "$stats_pid" 2> /dev/null || true

  if [ -s "$dir/cpu.pprof" ]; then
    go tool pprof -top -nodecount=15 "$dir/cpu.pprof" > "$dir/pprof-top.txt" 2> /dev/null || true
  fi
  # Let the outbox publisher work through what the run left behind for a
  # moment, then sample the app's own view of the backlog.
  sleep 10
  curl -fsS localhost:6060/metrics | grep -E '^(outbox_|wager_transactions_total|outbox_events_published_total)' > "$dir/metrics-after.txt" || true
  python3 bench/prometheus_snapshot.py "$started" "$finished" > "$dir/prometheus.json" || true

  set +e
  go run ./cmd/verify -database-url "$DATABASE_URL" > "$dir/verify.json"
  verify_exit=$?
  set -e

  python3 - "$dir" "$k6_exit" "$verify_exit" "$started" "$finished" <<'PY'
import json, os, platform, subprocess, sys
dir, k6_exit, verify_exit, started, finished = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4]), int(sys.argv[5])
def out(cmd):
    try:
        return subprocess.run(cmd, capture_output=True, text=True, check=False).stdout.strip()
    except OSError:
        return ""
cpu = next((line.split(":", 1)[1].strip() for line in out(["lscpu"]).splitlines() if line.startswith("Model name")), "")
keys = ["SCENARIO", "MODE", "RATE", "DURATION", "REPEAT", "WARMUP", "WALLETS", "KNEE_START", "KNEE_STEP", "KNEE_MAX",
        "KNEE_STEP_DURATION", "MIX", "ZIPF_S", "REPLAY_RATIO", "LABEL", "DB_MAX_OPEN_CONNS", "DB_MIN_CONNS",
        "OUTBOX_BATCH_SIZE", "OUTBOX_PUBLISH_INTERVAL", "OTEL_TRACES_SAMPLER_ARG", "BENCH_CPUS_POSTGRES", "BENCH_CPUS_APP", "BENCH_CPUS_K6"]
meta = {
    "git_sha": out(["git", "rev-parse", "HEAD"]),
    "git_dirty": out(["git", "status", "--porcelain", "--untracked-files=no"]) != "",
    "go_version": out(["go", "env", "GOVERSION"]),
    "images": out(["docker", "compose", "-p", "wallet-bench", "images", "--format", "json"]),
    "cpu_model": cpu,
    "cpus": os.cpu_count(),
    "memory": out(["free", "-m"]).splitlines()[1:2],
    "kernel": platform.release(),
    "settings": {k: os.environ[k] for k in keys if os.environ.get(k)},
    "started_unix": started,
    "finished_unix": finished,
    "k6_exit": k6_exit,
    "verify_exit": verify_exit,
}
json.dump(meta, open(os.path.join(dir, "meta.json"), "w"), indent=2)
PY

  echo "   k6 exit ${k6_exit}, verify exit ${verify_exit} -> ${dir}"
  if [ "$k6_exit" -ne 0 ] || [ "$verify_exit" -ne 0 ]; then
    status=1
  fi
done

exit "$status"
