# go-high-load-wallet

[![ci](https://github.com/Ricardoleaom/go-high-load-wallet/actions/workflows/ci.yml/badge.svg)](https://github.com/Ricardoleaom/go-high-load-wallet/actions/workflows/ci.yml)

A wallet service for online gaming written in Go: players' balances move
through bets, wins, refunds and rollbacks sent by game providers over HTTP
and a message queue. The point of the project is to take it to **high
volume without giving up a wallet's guarantees** — no negative balance,
every operation applied at most once, an append-only ledger that always
explains the balance, an event for every change — and to prove each step
with measurements.

It started as a hiring-challenge delivery
([backend-challenge-go](https://github.com/Ricardoleaom/backend-challenge-go),
tag `v0.1.0-challenge`) and grows phase by phase along the
[roadmap](ROADMAP.md).

| Read | For |
| --- | --- |
| [ARCHITECTURE.md](ARCHITECTURE.md) | how it works: concurrency, idempotency, ledger, outbox, contract |
| [docs/adr](docs/adr/README.md) | why: the decisions and the alternatives rejected |
| [docs/benchmarks](docs/benchmarks/README.md) | how it performs, measured and reproducible |
| [ROADMAP.md](ROADMAP.md) / [CHANGELOG.md](CHANGELOG.md) | where it is going and what changed |

## What it guarantees

- **Exact money**: `int64` minor units, never floats, never rounded.
- **No lost updates**: every command is one call to a PostgreSQL function
  that locks the wallet, decides and writes in one statement and one
  commit; concurrent bets on one wallet serialize on its row, different
  wallets run in parallel, with one instance or many. A reference model,
  a 37-case contract, a property test and a linearizability check hold it
  to the rules.
- **Idempotency** by `(provider, idempotency key)` with a canonical payload
  hash, enforced by unique indexes: resends, races and HTTP plus queue
  duplicates move money once.
- **Append-only ledger** enforced by a trigger, with reconciliation.
- **Transactional outbox**: an event exists if and only if its change
  committed; delivery at least once.
- **Out-of-order reversals** wait as `PENDING_REFERENCE` with a durable,
  multi-instance retry.
- **OAuth2** client credentials with Keycloak; providers only see their own
  transactions.

## Quick start

Requires Docker with Compose, and Go 1.27.2 to run tests or the load
harness from the host. The API examples below also use `jq` to pull a
field out of a JSON response.

```sh
make up        # PostgreSQL, Keycloak, LocalStack (SQS), migrations, API
make up-obs    # the same plus Prometheus, Grafana, Jaeger; tracing on
make down      # stop (make clean also drops the data)
```

If port 5432 is taken on your machine, prefix the commands with
`POSTGRES_HOST_PORT=55432`.

| Service | Address |
| --- | --- |
| API | <http://localhost:8080> (`/health/ready` says when it is up) |
| Admin (metrics, pprof when `PPROF_ENABLED=true`) | <http://127.0.0.1:6060/metrics> |
| Keycloak | <http://localhost:8081> (admin/admin) |
| Grafana, dashboard "Wallet / Overview" | <http://localhost:3000> (`make up-obs`) |
| Prometheus | <http://localhost:9090> (`make up-obs`) |
| Jaeger | <http://localhost:16686> (`make up-obs`) |

Configuration is environment variables, listed with their defaults in
[ARCHITECTURE.md §16](ARCHITECTURE.md#16-configuration) and
[`.env.example`](.env.example).

## Using the API

The `wagering` realm has `client_credentials` clients: `provider-a` and
`provider-b` (wagers), `internal-service` (wallets), and `bench-provider`
and `bench-internal` with two-hour tokens for load tests. Secrets are the
client ID plus `-secret`.

```sh
token() {
  curl -s -X POST http://localhost:8081/realms/wagering/protocol/openid-connect/token \
    -d grant_type=client_credentials -d client_id="$1" -d client_secret="$1-secret" | jq -r .access_token
}
INTERNAL_TOKEN=$(token internal-service)
PROVIDER_TOKEN=$(token provider-a)

# open a wallet
WALLET_ID=$(curl -s -X POST localhost:8080/wallets \
  -H "Authorization: Bearer $INTERNAL_TOKEN" -H "Content-Type: application/json" \
  -d '{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"1000.00","currency":"BRL"}}' | jq -r .id)

# place a bet; sending it again replays the same result with idempotentReplay: true
curl -s -X POST localhost:8080/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: provider-a:transaction-123" \
  -d '{"providerId":"provider-a","externalTransactionId":"transaction-123",
       "playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"'"$WALLET_ID"'",
       "roundId":"round-987","gameId":"fortune-chimp","kind":"BET",
       "money":{"amount":"25.00","currency":"BRL"}}'

# balance, ledger and reconciliation
curl -s -H "Authorization: Bearer $INTERNAL_TOKEN" localhost:8080/wallets/$WALLET_ID
curl -s -H "Authorization: Bearer $INTERNAL_TOKEN" "localhost:8080/wallets/$WALLET_ID/ledger?limit=50"
curl -s -X POST -H "Authorization: Bearer $INTERNAL_TOKEN" localhost:8080/wallets/$WALLET_ID/reconciliation
```

`REFUND` and `ROLLBACK` add `"referenceExternalTransactionId"`. The same
operation can arrive through the `wager-transactions.fifo` queue
(`MessageGroupId` = wallet) with the envelope documented in
[ARCHITECTURE.md §10](ARCHITECTURE.md#10-sqs-consumer); events leave
through `wager-events.fifo`. Every endpoint, status code and
`failureCode` is in [§12](ARCHITECTURE.md#12-http-contract) and in the
OpenAPI contract, [`docs/openapi.yaml`](docs/openapi.yaml).

### For a browser console

[go-high-load-wallet-web](https://github.com/Ricardoleaom/go-high-load-wallet-web)
is an operator console (React) and a load lab (Go, open-model, verifies
every wallet's money after each run) that live in their own repository
and run against this API. It signs people in through the public client
`wallet-console` (authorization code with PKCE) and calls the API as an
`operator`: it opens, reads and reconciles wallets, reads every
provider's transactions and reads `GET /ops/summary`, the last five
minutes of the service's own numbers, but never submits a wager; opening
a wallet with an initial balance is as trusted as for the internal
service. Compose
allows the console's local origins through `CORS_ALLOWED_ORIGINS`. The
realm's demo user is `operator` with password `operator`, for local use
only ([ADR 0018](docs/adr/0018-browser-console-surface.md)).

```sh
# the live summary, with any internal or operator token
curl -s -H "Authorization: Bearer $INTERNAL_TOKEN" localhost:8080/ops/summary | jq '.points[-1]'
```

## Testing

```sh
make test         # everything, integration included (needs Docker)
make test-short   # unit tests only, no Docker
make lint lint-ci vuln
```

Integration tests start what they need with testcontainers — a migrated
PostgreSQL, LocalStack and an in-process identity provider — and fail
instead of skipping when Docker is missing. Setting `DATABASE_URL` reuses a
running PostgreSQL. The engine contract runs against both the PostgreSQL engine
and an in-memory reference model; a property test (rapid) drives the two
with the same random command sequences, a linearizability check
(porcupine) replays concurrent histories, and the concurrency scenarios
(bets racing for one balance, fifty duplicates across three instances,
out-of-order reversals racing the retry worker) run against real
PostgreSQL with the race detector. The list is in
[ARCHITECTURE.md §18](ARCHITECTURE.md#18-testing).

## Benchmarks

Measured on a 4-vCPU laptop under WSL2 with PostgreSQL pinned to two
cores, `uniform-mix` traffic on 100,000 wallets; the method is in
[ADR 0009](docs/adr/0009-benchmark-methodology.md) and every number comes
from a report in [docs/benchmarks](docs/benchmarks/README.md).

| | knee (p99 under 50 ms) | p50 at the knee | highest served rate | events published |
| --- | --- | --- | --- | --- |
| [001](docs/benchmarks/001-baseline-as-delivered.md), as delivered | 40 rps | 18 ms | 82 rps, then falling | 10 a second, whatever the load |
| [002](docs/benchmarks/002-baseline-config-tuned.md), configuration only | no better | 18 ms | 86 rps | up to 57 a second; one publisher tops out near 240 |
| [002](docs/benchmarks/002-baseline-config-tuned.md#where-postgresqls-cpu-goes), same code with two indexes | 100 rps | 4.7 ms | 443 rps | |
| [003](docs/benchmarks/003-fixes-measured.md), defects fixed (0.2.1) | 150 rps | 4.5 ms | 495 rps; 568 at 750 offered, no stall | 10 a second |
| [004](docs/benchmarks/004-single-statement-hot-path.md), one statement per command | 400 rps | 3.2 ms | 970 rps; 1,021 at 1,500 offered, no stall, no 5xx | 10 a second, without stalling |
| phase 2 target | ≥ 1,000 rps | ≤ 5 ms | | |
| phase 3 target | | | | ≥ 2,000 a second, p99 age under 1 s |

What the measurements found:

- The first ceiling was not the design but two idempotency lookups that
  could not use their partial unique indexes: they read the whole
  `transactions` table on every bet and took 98% of PostgreSQL's CPU.
- Under overload, reversals could deadlock the connection pool until
  clients timed out.
- The outbox publishes exactly `batch / interval` events a second, one
  event at a time.

Version 0.2.1 fixes the first two, bounds every request in time and size
([ADR 0012](docs/adr/0012-time-budgets-and-input-limits.md)), and
[benchmark 003](docs/benchmarks/003-fixes-measured.md) measures the
result: six times the as-delivered throughput, and no stall under 1.5
times the peak.

Phase 2b makes every command one call to a PL/pgSQL function
([ADR 0014](docs/adr/0014-one-statement-per-command.md)), and
[benchmark 004](docs/benchmarks/004-single-statement-hot-path.md) measures
one commit and 1.5 to 2.1 ms of PostgreSQL CPU per bet, against about four
and 3.9 ms. The knee moved to 400 rps, the service serves about 1,000 bets
a second at saturation, and one wallet takes about 450 a second instead of
240. Phase 2's target, 1,000 rps within a 50 ms p99, is still ahead: the
tail breaks first, on pool waits and checkpoints, which is phase 2c's
work. Every run, the overloaded ones included, passed all eight invariant
checks.

## Layout

A hexagonal layout ([ADR 0013](docs/adr/0013-hexagonal-layout-and-engine-port.md)),
its dependency rule checked by a test:

```
cmd/api                 the service (Fx composition)
cmd/seed, verify        load-test seeding and invariant checks
internal/domain         money, ids, wallet, wager transaction, ledger: the rules
internal/app            ports and use cases: engine (+ reference model and
                        contract), wagering, queries, relay, events
internal/adapters       postgres (the engine as SQL functions), httpapi, sqsx
internal/platform       config, observability, lifecycle, buildinfo
bench/                  k6 scenarios and the benchmark runner
deploy/                 Keycloak realm, LocalStack queues, Prometheus, Grafana
migrations/             SQL schema and the engine's functions
```
