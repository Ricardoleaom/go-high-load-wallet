# 0013. Put every money movement behind an Engine port, in a hexagonal layout

- Status: accepted
- Date: 2026-10-18

## Context

Up to phase 2a the wager use case was one service of about 800 lines that
opened SQL transactions, called five repositories and built outbox rows
itself, and the HTTP handlers read repositories directly. Changing how a
bet is stored meant changing the use case, and nothing but the storage
code described what a bet must do.

Phase 2b rewrites the hot path as one statement per command (ADR 0014),
and phase 6 adds a single-writer engine (ADR 0007). Both need one
definition of what a command does that every implementation is held to.

## Decision

**Layout.** Four layers under `internal/`, and nothing else besides test
and benchmark support:

| Layer | Holds | May import |
| --- | --- | --- |
| `domain` | money, ids, wallet, ledger, wager: the rules, no I/O | the domain, `google/uuid` |
| `app` | ports and use cases: `engine`, `wagering`, `queries`, `relay`, `events` | the domain, the app, OpenTelemetry API, slog |
| `adapters` | `postgres`, `httpapi`, `sqsx` | the app, the domain, the platform |
| `platform` | config, observability, lifecycle, buildinfo | — |

Only `cmd/` knows Fx. An adapter never imports another adapter. A port
the app consumes is declared in the app; an interface only one adapter
consumes (the SQS inbox) is declared in that adapter.
`internal/archtest` checks every import of every non-test file against
these rules.

**The Engine port.** `engine.Engine` has one method,
`Execute(ctx, Command) (Outcome, error)`, and three commands: `OpenWallet`,
`SubmitWager` and `RetryReference`. A command takes effect completely
(transaction, ledger entry, balance, events) or not at all. Engines
validate a command before any lookup, so an invalid one fails the same way
whatever is recorded.

**One contract, three ways to check it.**

- `enginetest.Run` is the contract: 37 cases, concurrency included, that
  every engine passes. It runs against the reference model (`engine/model`)
  and against PostgreSQL.
- The reference model applies the domain rules to in-memory state under
  one lock. It is slow and obviously right; that is its job.
  `TestEngineMatchesTheModel` (`pgregory.net/rapid`) drives PostgreSQL and
  the model with the same random command sequences and requires the same
  answer at every step.
- `TestEngineIsLinearizable` (`porcupine`) checks histories of concurrent
  bets, wins, resends and reads on one wallet against a single-threaded
  wallet.

## Consequences

- A new engine (phase 6) is done when it passes the contract and matches
  the model; the use cases and transports do not change.
- The use case shrank to validation, identity and a call to the engine;
  HTTP and SQS share it unchanged.
- The rules now live in two places, the domain (through the model) and
  SQL (through the PostgreSQL engine), and the two can drift. The contract
  and the property test are what keep them together: in this phase they
  found two drifts, a replay racing its own commit answered as a key
  mismatch, and an invalid refund the SQL accepted.
- The layout costs a few more packages and one more indirection per
  request, which the benchmarks do not show.
