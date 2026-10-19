"""Dump the Prometheus series a benchmark report needs for one run window.

Usage: prometheus_snapshot.py START_UNIX END_UNIX > prometheus.json
The stack is torn down after every run, so this is the only copy.
"""
import json
import sys
import urllib.parse
import urllib.request

PROMETHEUS = "http://localhost:9090"
ROUTE = 'route="POST /wagering/transactions"'

QUERIES = {
    "server_rps": 'sum(rate(http_requests_total{path="POST /wagering/transactions"}[30s]))',
    "server_p50": f"histogram_quantile(0.50, sum by (le) (rate(http_request_duration_seconds_bucket{{{ROUTE}}}[30s])))",
    "server_p95": f"histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{{{ROUTE}}}[30s])))",
    "server_p99": f"histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{{{ROUTE}}}[30s])))",
    "server_5xx_rps": 'sum(rate(http_requests_total{status=~"5.."}[30s]))',
    "transactions_rps": "sum(rate(wager_transactions_total[30s]))",
    "outbox_published_rps": "sum(rate(outbox_events_published_total[30s]))",
    "outbox_pending": "max(outbox_pending_events)",
    "outbox_oldest_seconds": "max(outbox_oldest_pending_age_seconds)",
    "pool_in_use": 'max(wallet_db_pool_connections{state="acquired"})',
    "pool_wait_seconds_per_second": 'sum(rate(wallet_db_pool_acquire_wait_seconds_total[30s]))',
    "pool_waits_per_second": 'sum(rate(wallet_db_pool_acquire_waits_total[30s]))',
    "pg_commits_per_second": 'sum(rate(pg_stat_database_xact_commit{datname="wallet"}[30s]))',
    "app_cpu_cores": 'sum(rate(process_cpu_seconds_total{job="wallet"}[30s]))',
    "k6_rps": "sum(rate(k6_http_reqs_total[30s]))",
    "k6_dropped_per_second": "sum(rate(k6_dropped_iterations_total[30s]))",
}


def query_range(expr, start, end):
    params = urllib.parse.urlencode({"query": expr, "start": start, "end": end, "step": "5s"})
    with urllib.request.urlopen(f"{PROMETHEUS}/api/v1/query_range?{params}", timeout=30) as resp:
        result = json.load(resp)["data"]["result"]
    if not result:
        return []
    return [[int(float(t)), float(v)] for t, v in result[0]["values"] if v not in ("NaN", "+Inf", "-Inf")]


def main():
    start, end = int(sys.argv[1]), int(sys.argv[2])
    snapshot = {"start": start, "end": end, "series": {}}
    for name, expr in QUERIES.items():
        try:
            snapshot["series"][name] = query_range(expr, start, end)
        except OSError as err:
            snapshot["series"][name] = {"error": str(err)}
    json.dump(snapshot, sys.stdout, indent=1)


if __name__ == "__main__":
    main()
