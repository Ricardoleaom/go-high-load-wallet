#!/usr/bin/env bash
# Baseline 002 diagnostic: where do PostgreSQL backends spend the overload?
# Runs uniform-mix steady at 140 rps with pool 40; samples wait events and
# per-process CPU with synchronous_commit on, then switches it off mid-run.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
export POSTGRES_HOST_PORT=${POSTGRES_HOST_PORT:-5432}

OUT=bench/results/diagnosis-1-synchronous-commit
rm -rf "$OUT"; mkdir -p "$OUT"

DB_MAX_OPEN_CONNS=40 DB_MAX_IDLE_CONNS=40 SCENARIO=uniform-mix MODE=steady RATE=140 DURATION=4m LABEL=diag \
  bench/run.sh > "$OUT/run.log" 2>&1 &
run_pid=$!

until ls bench/results/*-uniform-mix-steady-diag/docker-stats.csv > /dev/null 2>&1; do
  kill -0 "$run_pid" 2> /dev/null || { echo "runner exited early"; exit 1; }
  sleep 1
done
echo "run phase started $(date -u +%T)"

pg() { docker exec wallet-bench-postgres-1 psql -U app -d wallet -At -F '|' -c "$1"; }
served() { curl -fsS localhost:6060/metrics | awk '/^(wallet_)?wager_submit_duration_seconds_count/ {s += $NF} END {printf "%d\n", s}'; }
cpu() {
  docker exec wallet-bench-postgres-1 sh -c '
    for p in /proc/[0-9]*; do
      [ -r "$p/stat" ] || continue
      t=$(awk "{print \$14 + \$15}" "$p/stat" 2> /dev/null) || continue
      c=$(tr "\0" " " < "$p/cmdline" 2> /dev/null)
      echo "${p#/proc/}|$t|$c"
    done'
}
waits() {
  pg "SELECT backend_type, state, coalesce(wait_event_type, 'CPU'), coalesce(wait_event, '-'), count(*)
      FROM pg_stat_activity WHERE pid <> pg_backend_pid() AND (state IS NULL OR state <> 'idle')
      GROUP BY 1, 2, 3, 4"
}

window() {
  local label=$1 seconds=$2
  local s0 c0 w0
  sleep 20
  s0=$(served); c0=$(pg "SELECT xact_commit FROM pg_stat_database WHERE datname = 'wallet'")
  w0=$(pg "SELECT wal_records, wal_sync, wal_write FROM pg_stat_wal")
  cpu > "$OUT/cpu-$label-start.txt"
  : > "$OUT/waits-$label.txt"
  for _ in $(seq "$seconds"); do waits >> "$OUT/waits-$label.txt"; sleep 1; done
  cpu > "$OUT/cpu-$label-end.txt"
  local s1 c1 w1
  s1=$(served); c1=$(pg "SELECT xact_commit FROM pg_stat_database WHERE datname = 'wallet'")
  w1=$(pg "SELECT wal_records, wal_sync, wal_write FROM pg_stat_wal")
  echo "$label seconds=$seconds served=$((s1 - s0)) commits=$((c1 - c0)) wal_before=$w0 wal_after=$w1" | tee -a "$OUT/windows.txt"
}

window sync-on 60
pg "ALTER SYSTEM SET synchronous_commit = off" > /dev/null
pg "SELECT pg_reload_conf()" > /dev/null
echo "synchronous_commit off at $(date -u +%T)"
window sync-off 60

wait "$run_pid"
echo "runner exit $?"
tail -3 "$OUT/run.log"
echo DIAG-DONE
