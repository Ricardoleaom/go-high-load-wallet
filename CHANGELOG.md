# Changelog

All notable changes to this project. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [semantic versioning](https://semver.org/).

## [0.2.1] - unreleased

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

[0.2.1]: https://github.com/Ricardoleaom/go-high-load-wallet/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/Ricardoleaom/go-high-load-wallet/compare/v0.1.0-challenge...v0.2.0
[0.1.0-challenge]: https://github.com/Ricardoleaom/go-high-load-wallet/releases/tag/v0.1.0-challenge
