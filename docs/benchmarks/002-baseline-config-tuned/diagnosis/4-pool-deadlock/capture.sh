#!/usr/bin/env bash
# With usable indexes, what stops everything near 500 rps?
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
export POSTGRES_HOST_PORT=${POSTGRES_HOST_PORT:-5432}
OUT=bench/results/diagnosis-4-pool-deadlock
rm -rf "$OUT"; mkdir -p "$OUT"
SCENARIO=uniform-mix MODE=steady RATE=500 DURATION=4m LABEL=stall \
  bench/run.sh > "$OUT/run.log" 2>&1 &
run_pid=$!
until ls bench/results/*-uniform-mix-steady-stall/verify-before.json > /dev/null 2>&1; do
  kill -0 "$run_pid" 2> /dev/null || { echo "runner exited early"; exit 1; }
  sleep 0.5
done
pg() { docker exec wallet-bench-postgres-1 psql -U app -d wallet -At -F '|' -c "$1"; }
pg "CREATE INDEX idx_transactions_provider_idempotency ON transactions (provider_id, idempotency_key)" > /dev/null
pg "CREATE INDEX idx_transactions_provider_external ON transactions (provider_id, external_id)" > /dev/null
pg "ANALYZE transactions" > /dev/null
echo "indexes created $(date -u +%T)"
until ls bench/results/*-uniform-mix-steady-stall/docker-stats.csv > /dev/null 2>&1; do sleep 0.5; done
echo "run phase started $(date -u +%T)"
prev=$(pg "SELECT xact_commit FROM pg_stat_database WHERE datname = 'wallet'")
dumps=0
while kill -0 "$run_pid" 2> /dev/null; do
  now=$(pg "SELECT xact_commit FROM pg_stat_database WHERE datname = 'wallet'") || break
  rate=$((now - prev)); prev=$now
  ts=$(date -u +%T)
  echo "$ts commits/s=$rate" >> "$OUT/commits.txt"
  pg "SELECT '$ts', backend_type, state, coalesce(wait_event_type, 'CPU'), coalesce(wait_event, '-'),
             left(regexp_replace(query, '\s+', ' ', 'g'), 90), count(*)
      FROM pg_stat_activity WHERE pid <> pg_backend_pid() AND (state IS NULL OR state <> 'idle')
      GROUP BY 2, 3, 4, 5, 6" >> "$OUT/activity.txt" 2>&1
  if [ "$rate" -lt 300 ] && [ "$dumps" -lt 3 ] && [ "$(wc -l < "$OUT/commits.txt")" -gt 30 ]; then
    dumps=$((dumps + 1))
    curl -fsS "localhost:6060/debug/pprof/goroutine?debug=1" -o "$OUT/goroutines-$dumps.txt" || true
    pg "SELECT pid, state, wait_event_type, wait_event, now() - xact_start AS xact_age, now() - state_change AS in_state,
               pg_blocking_pids(pid), left(regexp_replace(query, '\s+', ' ', 'g'), 120)
        FROM pg_stat_activity WHERE pid <> pg_backend_pid() AND backend_type = 'client backend'" > "$OUT/backends-$dumps.txt" 2>&1
    pg "SELECT locktype, mode, granted, count(*) FROM pg_locks GROUP BY 1, 2, 3 ORDER BY 4 DESC" > "$OUT/locks-$dumps.txt" 2>&1
    pg "SELECT * FROM pg_stat_wal" > "$OUT/wal-$dumps.txt" 2>&1
    pg "SELECT * FROM pg_stat_checkpointer" > "$OUT/checkpointer-$dumps.txt" 2>&1
    echo "dump $dumps at $ts (commits/s=$rate)"
  fi
  sleep 1
done
wait "$run_pid"; echo "runner exit $?"; tail -2 "$OUT/run.log"; echo DIAG4-DONE
