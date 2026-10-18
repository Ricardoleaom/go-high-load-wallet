"""Turn benchmark result directories into Markdown tables for the reports.

Usage: summarize.py RESULT_DIR...
Steady runs get one row each; knee runs get one row per load step, taken
from the server-side series over the step's hold window.
"""
import csv
import json
import os
import re
import sys


def load(path, default=None):
    try:
        with open(path) as f:
            return json.load(f)
    except (OSError, ValueError):
        return default


def ms(seconds):
    """Prometheus latencies are in seconds."""
    return "—" if seconds is None else f"{seconds * 1000:.1f}"


def k6ms(milliseconds):
    """k6's summary export already reports trends in milliseconds."""
    return "—" if milliseconds is None else f"{milliseconds:.1f}"


def mean(points, start, end):
    values = [v for t, v in points if start <= t <= end] if isinstance(points, list) else []
    return sum(values) / len(values) if values else None


def peak(points, start, end):
    values = [v for t, v in points if start <= t <= end] if isinstance(points, list) else []
    return max(values) if values else None


# Compose names the services' containers wallet-bench-<service>-<n> and
# one-off `compose run` containers wallet-bench-<service>-run-<id>; anchoring
# keeps postgres-exporter out of postgres.
CONTAINER_ROLES = {
    role: re.compile(rf"^wallet-bench-{role}-(\d+|run-[0-9a-f]+)$") for role in ("app", "postgres", "k6")
}


def container_cpu(directory):
    """Per-container CPU samples from docker-stats.csv: {role: [(t, percent)]}.
    100% is one full core."""
    samples = {role: [] for role in CONTAINER_ROLES}
    try:
        with open(os.path.join(directory, "docker-stats.csv")) as f:
            for line in f:
                parts = line.strip().split(",")
                if len(parts) < 3 or not parts[2].endswith("%"):
                    continue
                t, name, cpu = int(parts[0]), parts[1], float(parts[2][:-1])
                for role, pattern in CONTAINER_ROLES.items():
                    if pattern.match(name):
                        samples[role].append((t, cpu))
    except OSError:
        pass
    return samples


def cpu_cell(samples, start, end):
    cells = []
    for role in ("app", "postgres", "k6"):
        value = mean(samples[role], start, end)
        cells.append("—" if value is None else f"{value:.0f}")
    return "/".join(cells)


def read_csv(directory, name):
    try:
        with open(os.path.join(directory, name)) as f:
            return list(csv.DictReader(f))
    except OSError:
        return []


def pg_tables(directory):
    """PostgreSQL's counters per table at the end of the run (pg-tables.csv)."""
    return {
        row["relname"]: {k: int(v) for k, v in row.items() if k != "relname"}
        for row in read_csv(directory, "pg-tables.csv")
    }


def unused_indexes(directory):
    """Indexes the run never scanned (pg-indexes.csv)."""
    return [row["indexrelname"] for row in read_csv(directory, "pg-indexes.csv") if int(row["idx_scan"]) == 0]


def hot_percent(tables):
    """Share of wallet updates that were HOT: no index entry written."""
    wallets = tables.get("wallets")
    if not wallets or not wallets.get("n_tup_upd"):
        return "—"
    return f"{100 * wallets['n_tup_hot_upd'] / wallets['n_tup_upd']:.0f}"


def commits_per_request(series, start, end):
    """PostgreSQL commits per request served; a read is one commit too."""
    commits = mean(series.get("pg_commits_per_second", []), start, end)
    rps = mean(series.get("server_rps", []), start, end)
    return "—" if not commits or not rps else f"{commits / rps:.2f}"


def pg_ms_per_request(series, cpu, start, end):
    """Milliseconds of PostgreSQL CPU per request served."""
    pg = mean(cpu["postgres"], start, end)
    rps = mean(series.get("server_rps", []), start, end)
    return "—" if pg is None or not rps else f"{pg / 100 * 1000 / rps:.2f}"


def verify_ok(directory):
    report = load(os.path.join(directory, "verify.json"), {})
    return "pass" if report.get("ok") else "FAIL"


def steady_row(directory, meta, summary, prom):
    m = summary.get("metrics", {})
    duration = m.get("http_req_duration", {})
    reqs = m.get("http_reqs", {})
    failed = m.get("http_req_failed", {})
    dropped = m.get("dropped_iterations", {}).get("count", 0)
    series = prom.get("series", {})
    start, end = prom.get("start", 0), prom.get("end", 0)
    produced = mean(series.get("transactions_rps", []), start + 30, end)
    published = mean(series.get("outbox_published_rps", []), start + 30, end)
    settings = meta.get("settings", {})
    cpu = container_cpu(directory)
    return (
        f"| {os.path.basename(directory)} | {settings.get('RATE', '')} | {reqs.get('rate', 0):.1f} | "
        f"{k6ms(duration.get('med'))} | {k6ms(duration.get('p(95)'))} | {k6ms(duration.get('p(99)'))} | "
        f"{failed.get('value', 0) * 100:.2f}% | {dropped} | "
        f"{'—' if produced is None else f'{produced:.1f}'} | {'—' if published is None else f'{published:.1f}'} | "
        f"{peak(series.get('outbox_pending', []), start, end) or 0:.0f} | "
        f"{peak(series.get('outbox_oldest_seconds', []), start, end) or 0:.0f} | "
        f"{mean(series.get('pool_in_use', []), start + 30, end) or 0:.1f} | "
        f"{cpu_cell(cpu, start + 30, end)} | {pg_ms_per_request(series, cpu, start + 30, end)} | "
        f"{commits_per_request(series, start + 30, end)} | {hot_percent(pg_tables(directory))} | {verify_ok(directory)} |"
    )


STEADY_HEADER = (
    "| run | target rps | achieved rps | p50 ms | p95 ms | p99 ms | failed | dropped | "
    "transactions/s | outbox published/s | outbox backlog (peak) | oldest pending s (peak) | pool in use (avg) | CPU % app/pg/k6 | "
    "PG CPU ms/req | commits/req | wallet HOT % | verify |\n"
    "| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |"
)


def knee_rows(directory, meta, prom):
    settings = meta.get("settings", {})
    start_rate, step, top = int(settings.get("KNEE_START", 50)), int(settings.get("KNEE_STEP", 50)), int(settings.get("KNEE_MAX", 600))
    hold = settings.get("KNEE_STEP_DURATION", "60s")
    hold_seconds = int(hold[:-1]) * (60 if hold.endswith("m") else 1)
    series = prom.get("series", {})
    cpu = container_cpu(directory)
    rows = []
    t = prom.get("start", 0)
    for rate in range(start_rate, top + 1, step):
        window_start, window_end = t + 5 + 15, t + 5 + hold_seconds  # skip the ramp and let the 30s rates settle
        rows.append(
            f"| {rate} | {mean(series.get('server_rps', []), window_start, window_end) or 0:.1f} | "
            f"{ms(mean(series.get('server_p50', []), window_start, window_end))} | "
            f"{ms(mean(series.get('server_p99', []), window_start, window_end))} | "
            f"{mean(series.get('server_5xx_rps', []), window_start, window_end) or 0:.2f} | "
            f"{mean(series.get('k6_dropped_per_second', []), window_start, window_end) or 0:.1f} | "
            f"{mean(series.get('pool_waits_per_second', []), window_start, window_end) or 0:.1f} | "
            f"{mean(series.get('app_cpu_cores', []), window_start, window_end) or 0:.2f} | "
            f"{cpu_cell(cpu, window_start, window_end)} | {pg_ms_per_request(series, cpu, window_start, window_end)} | "
            f"{commits_per_request(series, window_start, window_end)} |"
        )
        t += 5 + hold_seconds
    return rows


KNEE_HEADER = (
    "| target rps | server rps | p50 ms | p99 ms | 5xx/s | dropped/s | pool waits/s | API CPU cores | CPU % app/pg/k6 | "
    "PG CPU ms/req | commits/req |\n"
    "| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |"
)


def main():
    steady, knees = [], []
    for directory in map(os.path.normpath, sys.argv[1:]):
        meta = load(os.path.join(directory, "meta.json"), {})
        prom = load(os.path.join(directory, "prometheus.json"), {})
        if meta.get("settings", {}).get("MODE") == "knee":
            knees.append((directory, knee_rows(directory, meta, prom)))
        else:
            steady.append(steady_row(directory, meta, load(os.path.join(directory, "summary.json"), {}), prom))
    if steady:
        print(STEADY_HEADER)
        print("\n".join(steady))
    for directory, rows in knees:
        print(f"\n{os.path.basename(directory)} (verify: {verify_ok(directory)})\n")
        print(KNEE_HEADER)
        print("\n".join(rows))
    stats = [d for d in map(os.path.normpath, sys.argv[1:]) if pg_tables(d)]
    if stats:
        print("\n| run | wallet updates | HOT % | transactions seq scans | indexes never scanned |")
        print("| --- | --- | --- | --- | --- |")
        for directory in stats:
            tables = pg_tables(directory)
            print(
                f"| {os.path.basename(directory)} | {tables.get('wallets', {}).get('n_tup_upd', 0)} | {hot_percent(tables)} | "
                f"{tables.get('transactions', {}).get('seq_scan', 0)} | {', '.join(unused_indexes(directory)) or '—'} |"
            )


if __name__ == "__main__":
    main()
