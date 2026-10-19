# 0018. Serve a browser console through an operator role, explicit CORS and an in-process summary

- Status: accepted
- Date: 2026-10-18

## Context

A web console, in its own repository, shows the service at work: wallets,
transactions, ledgers, reconciliation and live numbers, and starts load
against it. Until now every caller was a service with client
credentials: providers, with a `provider_id` claim, and the internal
service, with the `internal` realm role. A person in a browser fits
neither, and the browser adds three constraints: it cannot keep a client
secret, it calls the API from another origin, and it should not reach
Prometheus or the admin port, which are on the internal network.

## Decision

- **An `operator` realm role for people**, signed in through the public
  client `wallet-console` with the authorization code flow and S256 PKCE
  only (no password, implicit or client-credentials grant), redirecting
  only to the console's listed origins, with the `wallet-api` audience. An
  operator may do what the internal service does on wallets and read
  every provider's transactions. Submitting a wager still requires a
  provider identity, so the console can watch money move but never move
  it. A test pins the client's settings in the realm export.
- **CORS only for listed origins** (`CORS_ALLOWED_ORIGINS`, off when
  unset). Their preflights are answered before authentication, other
  origins' preflights get 403, and responses to a listed origin, errors
  included, are readable by it. No credentials are allowed, since the
  token travels in a header, and a wildcard fails the boot.
- **A live summary computed in the process**: `GET /ops/summary` returns
  five minutes of request, bet, error, transaction and event rates, bet
  latency quantiles, the outbox backlog and the pool's state, sampled
  from the process's own Prometheus registry every `OPS_SAMPLE_INTERVAL`.
  The browser asks the API, with the same token, instead of Prometheus.
- **An OpenAPI 3.1 contract**, `docs/openapi.yaml`, which the console
  generates its client from; Go tests hold it to the registered routes
  and to the error codes the handlers answer.

## Consequences

- The console needs no secret and no network path beyond the API and
  Keycloak; a stolen operator token reads but cannot move money.
- The demo `operator`/`operator` user is for local use only; any shared
  deployment must remove it and manage people in the IdP.
- The summary is per instance: behind several instances each answers
  for itself, which is enough for a local console and not a replacement
  for Prometheus. Sampling costs one gather every five seconds, including
  the outbox backlog query a scrape already runs.
- Load generation stays outside the browser: a browser opens a handful of
  connections per host and cannot honestly offer thousands of requests a
  second, and the wager API is for providers. The console drives a
  server-side load generator instead, in its own repository.
