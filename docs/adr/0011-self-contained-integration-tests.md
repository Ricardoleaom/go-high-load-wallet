# 0011. Make integration tests self-contained with testcontainers

- Status: accepted
- Date: 2026-10-16

## Context

Integration tests used to skip themselves unless `DATABASE_URL` and four
other variables pointed at a manually started compose stack. A suite that
passes by not running hides regressions.

## Decision

`internal/testsupport` provides `pgtest` (a migrated PostgreSQL from
`DATABASE_URL` or a testcontainers container per test binary, plus an
isolated database per test with `FreshURL`), `sqstest` (LocalStack),
`jwttest` (an in-process identity provider) and `hostport` (pins container
host ports to ports free in the test's network namespace, which Docker
Desktop on WSL does not guarantee). Integration tests run or fail; only
`-short` skips them.

## Consequences

`go test ./...` needs Docker and nothing else, locally and in CI. Each
test binary starts its own containers, which costs memory and a few
seconds; `DATABASE_URL` reuses one database for a faster local loop.
