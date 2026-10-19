# Changelog

All notable changes to this project. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [semantic versioning](https://semver.org/).

## [Unreleased]

Phase 2b: the hot path rewritten as one statement per command, behind an
`Engine` port, in a hexagonal layout, measured in
[benchmark 004](docs/benchmarks/004-single-statement-hot-path.md): one
commit and 1.5 to 2.1 ms of PostgreSQL CPU per bet (from about four and
3.9 ms), the knee from 150 to 400 rps, about 1,000 bets a second at
saturation, and one wallet from about 240 to 450 bets a second.

### Changed (breaking)

- `DB_MAX_IDLE_CONNS` is replaced by `DB_MIN_CONNS` (default 2): the pool
  is now pgx's own, which has no idle maximum. `DB_MAX_OPEN_CONNS` still
  sets the maximum.
- The pool metrics are `wallet_db_pool_*` instead of `go_sql_*`.
- A submission is validated completely before anything is looked up. A
  request that breaks a rule of its kind now answers 400
  `VALIDATION_ERROR` even under an idempotency key already used (it
  answered 409 `IDEMPOTENCY_CONFLICT`) or for a wallet that does not exist
  (it answered 404).
- Events' `occurredAt` carries microseconds with six digits, and a
  transaction, its ledger entry and its events share one instant, taken
  after the wallet lock.
- New rows get UUIDv7 keys. Identifiers sent by clients may still be any
  UUID.
- Opening a wallet answers the `playerId` in lower case, as it is stored.
- Migrations: `000012` leaves room on wallet pages for HOT updates and
  drops `idx_wallets_player_id`; `000013` adds the engine's functions;
  `000014` rebuilds the idempotency and external-ID unique indexes, each
  led by its own column; `000015` indexes pending outbox events by age.
  `000014` and `000015` build indexes without `CONCURRENTLY`.

### Added

- `engine.Engine`, a reference model of the rules and a 37-case contract
  every engine passes; a property test (rapid) comparing the PostgreSQL
  engine with the model and a linearizability check (porcupine)
  ([ADR 0013](docs/adr/0013-hexagonal-layout-and-engine-port.md)).
- Fuzz targets for HTTP and SQS command decoding, and a test enforcing
  the layout's dependency rule.
- `auth_token_cache_total{result}` and the `wallet_db_pool_*` metrics.
- Benchmark runs record PostgreSQL's table and index counters and its
  log; the summaries report PostgreSQL CPU and commits per bet served
  and the wallet HOT ratio.
- For a browser console ([ADR 0018](docs/adr/0018-browser-console-surface.md)):
  the `operator` realm role, which opens, reads and reconciles wallets
  and reads every provider's transactions but never submits a wager; the
  public `wallet-console` client (authorization code with S256 PKCE) and
  a demo `operator` user; CORS for `CORS_ALLOWED_ORIGINS`, off when unset;
  `GET /ops/summary`, five minutes of live numbers sampled every
  `OPS_SAMPLE_INTERVAL`; and the OpenAPI contract `docs/openapi.yaml`,
  held to the routes and error codes by tests.

### Changed

- Every command (open a wallet, submit, retry a pending reversal) is one
  call to a PL/pgSQL function: one round trip and one commit, where a bet
  took about eleven and four
  ([ADR 0014](docs/adr/0014-one-statement-per-command.md)).
- pgx's pool replaces `database/sql`; a request that runs out of time has
  its statement cancelled on the server and keeps its connection
  ([ADR 0015](docs/adr/0015-pgxpool-and-server-side-cancellation.md)).
- `wallets` pages keep a fifth free so balance updates are HOT
  ([ADR 0016](docs/adr/0016-uuidv7-keys-and-wallet-fillfactor.md)).
- Verified tokens are cached until their expiry, one JWKS refresh
  interval, or their key leaving the JWKS, whichever comes first
  ([ADR 0017](docs/adr/0017-cache-verified-tokens.md)).
- The outbox relay moves to the app layer over a store and a sink, and
  counts failed sends.

### Fixed

- The idempotency lookup could be planned on the external-ID index with
  the provider alone, reading every transaction of the provider, while
  the statistics did not know that provider's rows yet.
- Claiming due outbox events sorted the whole backlog on every round;
  with a large backlog the claim hit the statement timeout and the relay
  published nothing.

## [0.2.1] - 2026-10-18

Phase 2a: the defects benchmark 002 found, fixed and measured in
[benchmark 003](docs/benchmarks/003-fixes-measured.md), plus the hardening
from the post-phase-1 audit.

### Changed (breaking)

- `JWT_ISSUER` and `JWT_AUDIENCE` are required, and tokens must carry a
  matching issuer, the audience and an expiry. The bundled Keycloak realm
  adds the `wallet-api` audience to every client and runs with a fixed
  hostname.
- Migration `000011` adds `transactions.currency`, filled from the wallets
  for existing rows.
- Malformed identifiers, oversized fields and bodies now answer 400
  (`INVALID_ID`, `VALIDATION_ERROR`) or 413 (`BODY_TOO_LARGE`) instead of
  500, and a request out of time answers 503 `TIMEOUT`.
- The local stack publishes its ports on `127.0.0.1` only.

### Added

- `REQUEST_TIMEOUT`, `DB_STATEMENT_TIMEOUT`,
  `DB_IDLE_IN_TRANSACTION_TIMEOUT` and `DB_LOCK_TIMEOUT`; HTTP servers
  bound reads, writes and idle connections
  ([ADR 0012](docs/adr/0012-time-budgets-and-input-limits.md)).
- Dependabot for Go modules, Actions and the Dockerfile; Actions pinned by
  commit SHA.
- Tests for readiness, the submit error mapping and the outbox backoff,
  and a fuzz test for money parsing.

### Fixed

- The idempotency and external-ID lookups use their unique indexes
  instead of scanning the `transactions` table on every submission.
- Reversals and the pending-reference retry no longer need a second pool
  connection while holding one, which deadlocked the pool under overload.
- Concurrent submissions with the same external ID and different
  idempotency keys answer 409 `IDEMPOTENCY_KEY_MISMATCH` instead of 500.
- One pending reference that keeps failing no longer stops the retry of
  the others claimed with it.
- A transaction rejected for its currency reads back in the currency it
  was requested in, not the wallet's.
- Malformed IDs arriving over SQS are counted as invalid messages.
- A wallet or player ID sent in upper case is the same ID: it no longer
  fails the wallet ownership check with `WALLET_NOT_FOUND`, and the same
  operation sent again in another case replays.

## [0.2.0] - 2026-10-17

Phase 1: foundation and baseline. See [ROADMAP.md](ROADMAP.md).

### Changed (breaking)

- The module is `github.com/Ricardoleaom/go-high-load-wallet` and the local
  database is `wallet`; run `docker compose down -v` once after upgrading.
- `/metrics` left the public port. Metrics, and pprof when
  `PPROF_ENABLED=true`, are served on the admin server at `ADMIN_ADDR`
  (`127.0.0.1:6060` by default).
- Integration tests no longer skip without a manual environment: they
  start their own containers and fail when Docker is unavailable. `-short`
  runs unit tests only.
- Invalid configuration values fail the boot naming the variable instead
  of silently falling back to defaults.

### Added

- Latency histograms for HTTP, submit, SQS processing and outbox rounds,
  with trace exemplars; connection pool, Go runtime and outbox backlog
  metrics; `wallet_build_info`.
- OpenTelemetry tracing for HTTP, SQL statements and background work,
  logs correlated by `trace_id`, and a Prometheus, Grafana and Jaeger
  profile (`make up-obs`) with a provisioned dashboard.
- Settings for the connection pool, outbox batch size, admin address,
  pprof, log level and JWKS refresh interval.
- Load harness: `cmd/seed`, `cmd/verify`, five k6 scenarios and
  `make bench`, plus benchmark reports 001 and 002.
- Self-contained test support (`pgtest`, `sqstest`, `jwttest`, `hostport`),
  end-to-end HTTP tests and the first SQS consumer and publisher tests.
- GitHub Actions CI (lint, actionlint, tests with containers, govulncheck,
  image build), pinned developer tools in `tools/go.mod`, a Makefile and a
  distroless non-root image.
- Architecture decision records, a roadmap and English documentation.

### Fixed

- The JWKS refresh stopped right after boot, so keys revoked at the IdP
  stayed trusted until a restart.
- A failed first JWKS fetch was ignored and the API reported ready without
  keys; it now retries for up to 2 minutes and fails the boot otherwise.
- A token with an unknown key could hold a request for up to a minute
  while the IdP was slow; the re-fetch is now bounded to 2 seconds.
- Go 1.27.2 and updated dependencies remove known vulnerabilities.

### Known issues

Found by the benchmarks of this release and fixed in 0.2.1; the hot path
is unchanged from 0.1.0-challenge on purpose, so the baselines measure it
as delivered.

- The idempotency and external-ID lookups cannot use their partial unique
  indexes and scan the `transactions` table on every submission.
- Under overload, concurrent reversals can deadlock the connection pool
  until clients time out.

## [0.1.0-challenge] - 2026-10-15

The service as delivered for the hiring challenge: wallets moved over HTTP
and SQS with persistent idempotency, an append-only ledger, a
transactional outbox, pending-reference handling for early reversals and
OAuth2 authentication with Keycloak.

[Unreleased]: https://github.com/Ricardoleaom/go-high-load-wallet/compare/v0.2.1...HEAD
[0.2.1]: https://github.com/Ricardoleaom/go-high-load-wallet/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/Ricardoleaom/go-high-load-wallet/compare/v0.1.0-challenge...v0.2.0
[0.1.0-challenge]: https://github.com/Ricardoleaom/go-high-load-wallet/releases/tag/v0.1.0-challenge
