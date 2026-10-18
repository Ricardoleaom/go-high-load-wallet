#!/usr/bin/env bash
# Same code, two plain indexes the lookups can use: where is the knee now?
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
export POSTGRES_HOST_PORT=${POSTGRES_HOST_PORT:-5432}
OUT=bench/results/diagnosis-3-composite-lookup-indexes
rm -rf "$OUT"; mkdir -p "$OUT"
SCENARIO=uniform-mix MODE=knee KNEE_START=50 KNEE_STEP=50 KNEE_MAX=600 KNEE_STEP_DURATION=60s LABEL=lookup-index \
  bench/run.sh > "$OUT/run.log" 2>&1 &
run_pid=$!
until ls bench/results/*-uniform-mix-knee-lookup-index/verify-before.json > /dev/null 2>&1; do
  kill -0 "$run_pid" 2> /dev/null || { echo "runner exited early"; exit 1; }
  sleep 0.5
done
pg() { docker exec wallet-bench-postgres-1 psql -U app -d wallet -c "$1"; }
explain() {
  pg "EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) SELECT id FROM transactions WHERE provider_id = 'bench-provider' AND idempotency_key = 'bench-provider:missing'"
  pg "EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) SELECT id FROM transactions WHERE provider_id = 'bench-provider' AND external_id = 'missing'"
}
explain > "$OUT/explain-before.txt" 2>&1
pg "CREATE INDEX idx_transactions_provider_idempotency ON transactions (provider_id, idempotency_key)" > "$OUT/ddl.txt" 2>&1
pg "CREATE INDEX idx_transactions_provider_external ON transactions (provider_id, external_id)" >> "$OUT/ddl.txt" 2>&1
pg "ANALYZE transactions" >> "$OUT/ddl.txt" 2>&1
explain > "$OUT/explain-after.txt" 2>&1
echo "indexes created $(date -u +%T)"
wait "$run_pid"; echo "runner exit $?"; tail -2 "$OUT/run.log"; echo DIAG3-DONE
