# Benchmarks

Every phase ends with a measured comparison against the previous one, run
with the harness in [`bench/`](../../bench) and the method in
[ADR 0009](../adr/0009-benchmark-methodology.md).

| Report | What it measures |
| --- | --- |
| [001](001-baseline-as-delivered.md) | The service as delivered for the challenge |
| [002](002-baseline-config-tuned.md) | The same code with only configuration changed, the diagnosis of its ceiling, and the targets for phases 2 and 3 |
| [003](003-fixes-measured.md) | The same hot path with the defects fixed and every request bounded (0.2.1) |
| [004](004-single-statement-hot-path.md) | Every command as one statement behind the `Engine` port (phase 2b) |

## Running one

```sh
make down                                   # the dev stack must be stopped
make bench SCENARIO=uniform-mix MODE=steady RATE=100 DURATION=5m
make bench SCENARIO=uniform-mix MODE=knee KNEE_START=50 KNEE_STEP=50 KNEE_MAX=600
python3 bench/summarize.py bench/results/<run>...
```

Each run builds and seeds a fresh stack in the `wallet-bench` compose
project, warms up, runs the scenario, profiles the API, snapshots
Prometheus and checks the financial invariants. Raw results land in
`bench/results/` (not versioned). Reports copy into
`docs/benchmarks/NNN-name/` the files their tables come from (k6 summary,
Prometheus snapshot, container CPU, invariant report, run metadata,
profile top), so `bench/summarize.py` regenerates every table from the
repository.

| Variable | Default | Meaning |
| --- | --- | --- |
| `SCENARIO` | — | `uniform-mix`, `zipf-mix`, `hot-wallet`, `replay-storm`, `read-mix` |
| `MODE` | `steady` | `steady` holds `RATE` for `DURATION`; `knee` climbs from `KNEE_START` to `KNEE_MAX` by `KNEE_STEP`, holding each step `KNEE_STEP_DURATION` |
| `REPEAT` | `1` | independent runs, each on a fresh stack |
| `WALLETS` | `100000` | seeded wallets |
| `WARMUP` | `60s` | warmup at a tenth of the rate |
| `BENCH_CPUS_POSTGRES`, `BENCH_CPUS_APP`, `BENCH_CPUS_K6` | `0-1`, `2`, `3` | CPU pinning |
| `POSTGRES_HOST_PORT` | `5432` | host port for seeding and verifying |
| `DB_MAX_OPEN_CONNS`, `DB_MIN_CONNS`, ... | the app's | any app setting passes through to the API |

App settings such as `DB_MAX_OPEN_CONNS`, `OUTBOX_BATCH_SIZE` or
`OUTBOX_PUBLISH_INTERVAL` pass straight through to the API container.
