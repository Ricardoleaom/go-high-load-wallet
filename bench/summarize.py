"""Turn benchmark result directories into Markdown tables for the reports.

Usage: summarize.py RESULT_DIR...
Steady runs get one row each; knee runs get one row per load step, taken
from the server-side series over the step's hold window.
"""
import json
import os
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
    return (
        f"| {os.path.basename(directory)} | {settings.get('RATE', '')} | {reqs.get('rate', 0):.1f} | "
        f"{k6ms(duration.get('med'))} | {k6ms(duration.get('p(95)'))} | {k6ms(duration.get('p(99)'))} | "
        f"{failed.get('value', 0) * 100:.2f}% | {dropped} | "
        f"{'—' if produced is None else f'{produced:.1f}'} | {'—' if published is None else f'{published:.1f}'} | "
        f"{peak(series.get('outbox_pending', []), start, end) or 0:.0f} | "
        f"{peak(series.get('outbox_oldest_seconds', []), start, end) or 0:.0f} | "
        f"{mean(series.get('pool_in_use', []), start + 30, end) or 0:.1f} | {verify_ok(directory)} |"
    )


STEADY_HEADER = (
    "| run | target rps | achieved rps | p50 ms | p95 ms | p99 ms | failed | dropped | "
    "transactions/s | outbox published/s | outbox backlog (peak) | oldest pending s (peak) | pool in use (avg) | verify |\n"
    "| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |"
)


def knee_rows(directory, meta, prom):
    settings = meta.get("settings", {})
    start_rate, step, top = int(settings.get("KNEE_START", 50)), int(settings.get("KNEE_STEP", 50)), int(settings.get("KNEE_MAX", 600))
    hold = settings.get("KNEE_STEP_DURATION", "60s")
    hold_seconds = int(hold[:-1]) * (60 if hold.endswith("m") else 1)
    series = prom.get("series", {})
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
            f"{mean(series.get('app_cpu_cores', []), window_start, window_end) or 0:.2f} |"
        )
        t += 5 + hold_seconds
    return rows


KNEE_HEADER = (
    "| target rps | server rps | p50 ms | p99 ms | 5xx/s | dropped/s | pool waits/s | API CPU cores |\n"
    "| --- | --- | --- | --- | --- | --- | --- | --- |"
)


def main():
    steady, knees = [], []
    for directory in sys.argv[1:]:
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


if __name__ == "__main__":
    main()
