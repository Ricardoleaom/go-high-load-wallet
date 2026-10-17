# 0009. Benchmark with an open model, pinned CPUs and invariant checks

- Status: accepted
- Date: 2026-10-16

## Context

Load numbers are easy to produce and easy to get wrong: closed-model tools
slow down with the server and hide latency (coordinated omission), a load
generator sharing CPUs with the system distorts both, and a fast run that
corrupted balances proves nothing.

## Decision

- k6 with arrival-rate executors (open model): `steady` holds a rate,
  `knee` climbs in steps until latency or errors break. The knee is the
  highest step with p99 under 50 ms and under 0.1% 5xx.
- `bench/run.sh` builds and seeds a fresh stack in its own compose project
  for every run, warms up, runs, captures a CPU and heap profile, container
  stats and a Prometheus snapshot of the window, then runs `cmd/verify`.
- PostgreSQL, the API and k6 are pinned to separate CPUs (`cpuset`) on the
  4-vCPU reference machine; the pinning is part of the recorded metadata.
- Business rejections (422) and pending reversals (202) are expected
  responses; only transport errors and 5xx count as failures.
- A run whose invariant check fails is reported as failed, whatever its
  throughput.
- Reports in `docs/benchmarks/` record the commit, environment, settings
  and raw summaries next to the analysis.

## Consequences

Numbers are comparable across phases on the same machine, not absolute:
WSL2 I/O and a shared 4-vCPU box are not production. Each run costs
minutes of setup, which is the price of never measuring a dirty state.
