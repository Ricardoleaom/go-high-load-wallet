# 0017. Cache verified tokens until their key or their expiry ends them

- Status: accepted
- Date: 2026-10-18

## Context

Every request carries a bearer token (ADR 0012's issuer, audience and
expiry checks included), and verifying its RS256 signature costs about
78 µs of CPU on the benchmark machine (`BenchmarkMiddleware`): at 1,000
requests a second, 8% of a core spent re-checking the same few tokens,
since a provider reuses its token until it expires.

## Decision

The authenticator keeps what it verified, keyed by the SHA-256 of the
token (the tokens themselves are not kept), with the identity it carries
and the ID of the key that signed it. An entry is served while all of
these hold:

- the token has not expired: an entry lives until the earlier of the
  token's `exp` and one JWKS refresh interval;
- the signing key is still in the JWKS the authenticator holds, checked
  on every hit, so a key the IdP revokes stops being trusted at the next
  refresh, as it did before the cache;
- it was verified in full first: a rejected token is never cached.

The cache holds at most 10,000 entries; when full it drops expired ones,
at most once a second, and otherwise stops adding. Tokens without a key ID
are not cached. `auth_token_cache_total{result}` counts hits and misses.

## Consequences

- A request with a cached token costs about 10 µs in the middleware
  instead of 78 µs.
- The first version capped entries at one refresh interval from when they
  were cached but did not follow the JWKS itself, so a token verified just
  before the refresh that removed its key stayed trusted for up to another
  interval. An automated security review caught it; binding entries to
  their key fixed it, and `TestCachedTokenStopsWhenItsKeyLeavesTheJWKS`,
  on a clock that never moves, pins it.
- A token can only be cached if it is valid, so filling the cache takes
  valid tokens from the IdP, and the bound caps memory at about 1 MB.
- A token revoked individually at the IdP (rather than its key) was never
  checked by this service, and still is not: tokens are short-lived for
  that.
