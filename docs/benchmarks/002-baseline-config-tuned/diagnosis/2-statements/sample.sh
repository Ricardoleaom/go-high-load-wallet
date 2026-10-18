#!/usr/bin/env bash
# Which statements hold PostgreSQL's CPU, early and late in an overloaded run?
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
export POSTGRES_HOST_PORT=${POSTGRES_HOST_PORT:-5432}
OUT=bench/results/diagnosis-2-statements
rm -rf "$OUT"; mkdir -p "$OUT"
DB_MAX_OPEN_CONNS=40 DB_MAX_IDLE_CONNS=40 SCENARIO=uniform-mix MODE=steady RATE=140 DURATION=4m LABEL=diag2 \
  bench/run.sh > "$OUT/run.log" 2>&1 &
run_pid=$!
until ls bench/results/*-uniform-mix-steady-diag2/docker-stats.csv > /dev/null 2>&1; do
  kill -0 "$run_pid" 2> /dev/null || { echo "runner exited early"; exit 1; }
  sleep 1
done
echo "run phase started $(date -u +%T)"
sample() {
  local label=$1 n=$2
  for _ in $(seq "$n"); do
    docker exec wallet-bench-postgres-1 psql -U app -d wallet -At -F '|' -c "
      SELECT coalesce(wait_event_type, 'CPU'), left(regexp_replace(query, '\s+', ' ', 'g'), 160), count(*)
      FROM pg_stat_activity
      WHERE backend_type = 'client backend' AND state = 'active' AND pid <> pg_backend_pid()
      GROUP BY 1, 2" >> "$OUT/queries-$label.txt" 2>&1
    sleep 0.5
  done
}
sleep 25; sample early 30
docker exec wallet-bench-postgres-1 psql -U app -d wallet -At -c "
  SELECT relname, n_live_tup, n_dead_tup, seq_scan, seq_tup_read, idx_scan, n_tup_upd, n_tup_hot_upd
  FROM pg_stat_user_tables ORDER BY seq_tup_read DESC" > "$OUT/tables-early.txt" 2>&1
sleep 95; sample late 30
docker exec wallet-bench-postgres-1 psql -U app -d wallet -At -c "
  SELECT relname, n_live_tup, n_dead_tup, seq_scan, seq_tup_read, idx_scan, n_tup_upd, n_tup_hot_upd
  FROM pg_stat_user_tables ORDER BY seq_tup_read DESC" > "$OUT/tables-late.txt" 2>&1
docker exec wallet-bench-postgres-1 psql -U app -d wallet -At -c "
  SELECT indexrelname, idx_scan, idx_tup_read, idx_tup_fetch FROM pg_stat_user_indexes ORDER BY idx_tup_read DESC" > "$OUT/indexes-late.txt" 2>&1
echo "sampling done $(date -u +%T)"
wait "$run_pid"; echo "runner exit $?"; tail -2 "$OUT/run.log"; echo DIAG2-DONE
