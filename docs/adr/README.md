# Architecture Decision Records

Decisions that shape the service, newest last. Each record follows
[`template.md`](template.md); accepted records are never edited, only
superseded.

| ADR | Decision | Status |
| --- | --- | --- |
| [0001](0001-record-architecture-decisions.md) | Record architecture decisions | accepted |
| [0002](0002-money-as-int64-minor-units.md) | Represent money as int64 minor units | accepted |
| [0003](0003-atomic-conditional-wallet-update.md) | Move balances with one conditional UPDATE | superseded by 0014 |
| [0004](0004-idempotency-unique-index-and-payload-hash.md) | Enforce idempotency with a unique index and a canonical payload hash | accepted |
| [0005](0005-transactional-outbox-with-polling.md) | Publish events through a transactional outbox, polled | accepted |
| [0006](0006-pending-reference-durable-retry.md) | Hold early reversals as PENDING_REFERENCE with a durable retry | accepted |
| [0007](0007-high-load-target-architecture.md) | Grow a PostgreSQL-centric engine first, a single-writer engine later | accepted |
| [0008](0008-kafka-via-redpanda-replaces-sqs.md) | Replace SQS with Kafka, run locally as Redpanda | accepted, phase 3 |
| [0009](0009-benchmark-methodology.md) | Benchmark with an open model, pinned CPUs and invariant checks | accepted |
| [0010](0010-observability-stack.md) | Observe with Prometheus, OpenTelemetry and Jaeger, behind an admin port | accepted |
| [0011](0011-self-contained-integration-tests.md) | Make integration tests self-contained with testcontainers | accepted |
| [0012](0012-time-budgets-and-input-limits.md) | Bound every request in time and size, and answer the client's mistakes with 4xx | accepted |
| [0013](0013-hexagonal-layout-and-engine-port.md) | Put every money movement behind an Engine port, in a hexagonal layout | accepted |
| [0014](0014-one-statement-per-command.md) | Run every command as one call to a PL/pgSQL function | accepted |
| [0015](0015-pgxpool-and-server-side-cancellation.md) | Use pgx's own pool and cancel timed-out statements on the server | accepted |
| [0016](0016-uuidv7-keys-and-wallet-fillfactor.md) | Key new rows with UUIDv7 and leave room for HOT wallet updates | accepted |
| [0017](0017-cache-verified-tokens.md) | Cache verified tokens until their key or their expiry ends them | accepted |
| [0018](0018-browser-console-surface.md) | Serve a browser console through an operator role, explicit CORS and an in-process summary | accepted |
