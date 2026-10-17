# 0002. Represent money as int64 minor units

- Status: accepted
- Date: 2026-10-16

## Context

A wallet that rounds is a wallet that loses or creates money. Binary
floating point cannot represent most decimal amounts, and arbitrary
precision decimals cost allocations on the hottest path of the service.

## Decision

`internal/domain/money.Money` is immutable and holds an `int64` amount in
minor units (cents) plus an ISO 4217 currency. Input is accepted only as a
decimal string with exactly two decimal places (`"25.00"`): no sign,
exponent, `NaN` or other scale, and nothing is ever rounded. Arithmetic
checks currency equality and `int64` overflow explicitly. Columns are
`BIGINT`; the API speaks decimal strings.

## Consequences

Amounts are exact end to end and cheap to compute. The maximum balance is
92,233,720,368,547,758.07 in the currency unit. Currencies with other
scales (JPY, BHD) would need a per-currency scale before being accepted.
