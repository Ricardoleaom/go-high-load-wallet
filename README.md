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
- **No lost updates**: every movement is one conditional `UPDATE`;
  concurrent bets on one wallet serialize on its row, different wallets run
  in parallel, with one instance or many.
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
harness from the host.

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
`failureCode` is in [§12](ARCHITECTURE.md#12-http-contract).

## Testing

```sh
make test         # everything, integration included (needs Docker)
make test-short   # unit tests only, no Docker
make lint lint-ci vuln
```

Integration tests start what they need with testcontainers — a migrated
PostgreSQL, LocalStack and an in-process identity provider — and fail
instead of skipping when Docker is missing. Setting `DATABASE_URL` reuses a
running PostgreSQL. The concurrency scenarios (two bets racing for the same
balance, fifty duplicates across three instances, out-of-order reversals)
run against real PostgreSQL with the race detector; the list is in
[ARCHITECTURE.md §18](ARCHITECTURE.md#18-testing).

## Benchmarks

BENCHMARKS_SUMMARY

## Layout

```
cmd/api            the service (Fx composition)
cmd/seed, verify   load-test seeding and invariant checks
internal/domain    money, wallet, wager transaction, ledger
internal/application  use cases
internal/infrastructure  PostgreSQL and SQS adapters
internal/httpapi   HTTP handlers and authentication
internal/observability  metrics, tracing, admin endpoints
bench/             k6 scenarios and the benchmark runner
deploy/            Keycloak realm, LocalStack queues, Prometheus, Grafana
migrations/        SQL schema
```
