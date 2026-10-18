# Roadmap

The service evolves in phases. Each phase ends with a benchmark report
compared against the previous one, decision records for the choices it
made, updated documentation, a green CI and a release tag. The reasoning
behind the order is in [ADR 0007](docs/adr/0007-high-load-target-architecture.md).

| Phase | Release | Delivers | Proof | Status |
| --- | --- | --- | --- | --- |
| 1. Foundation and baseline | v0.2.0 | self-contained tests, CI, distroless image, metrics, tracing, admin port, load harness, as-delivered and configuration-only baselines | [benchmarks 001 and 002](docs/benchmarks/README.md) | done |
| 2. PostgreSQL hot path | v0.3.0 | fixes for the two defects benchmark 002 found (lookups that bypass their indexes, a pool deadlock in reversals), then the `Engine` port and hexagonal layout, native pgx pool, a bet in one statement, UUIDv7 keys, fillfactor and HOT updates, admission control, PgBouncer, PostgreSQL tuning | [002's targets](docs/benchmarks/002-baseline-config-tuned.md#targets-for-phases-2-and-3): knee of at least 1,000 rps within a 50 ms p99, graceful overload; linearizability checked with porcupine | next |
| 3. Kafka | v0.4.0 | SQS replaced by Kafka (Redpanda locally), Protobuf with `buf`, batched consumer storing offsets in the database, sharded outbox relay, DLQ, trace propagation | at least 2,000 events a second with a p99 lag under 1 s; Kafka ingestion measured; `kill -9` without loss | planned |
| 4. Data at scale | v0.5.0 | time partitioning, a separate operation-keys table for global uniqueness, ledger keyed by `(wallet_id, version)`, read replica, continuous reconciliation | stable p99 with tens of millions of rows; query plans documented | planned |
| 5. Kubernetes and resilience | v0.6.0 | Helm chart on k3d, HPA and KEDA, PDB, per-provider rate limits, fault injection with toxiproxy | rolling update under load with zero 5xx; killed pods never break an invariant | planned |
| 6. Advanced engines | v0.7.0+ | single-writer-per-partition engine, CDC outbox over logical replication, double-entry ledger without a hot house account, PGO | engine A against engine B and polling against CDC in the same harness | planned |

## Invariants no phase may break

1. A wallet balance is never negative.
2. Every wallet's balance equals the sum of its ledger.
3. An operation moves money at most once, over any transport, on any instance.
4. Every processed movement has exactly one ledger entry; rejections and `LOSS` have none.
5. An original transaction has at most one successful reversal.
6. Every committed change has its event and no event exists without its change.
7. The ledger is append-only.

`cmd/verify` checks them after every benchmark run.
