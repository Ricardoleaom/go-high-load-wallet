# 0008. Replace SQS with Kafka, run locally as Redpanda

- Status: accepted (implemented in phase 3)
- Date: 2026-10-16

## Context

SQS FIFO orders messages per group and LocalStack emulates its API, but not
its throughput limits, so local measurements of the messaging path say
little. The phase 6 single-writer engine needs partition ownership with
ordered, replayable logs, which SQS does not offer.

## Decision

Phase 3 moves inbound commands and outbound events to Kafka topics keyed
by `walletId`, using the `franz-go` client. Locally the broker is Redpanda
(Kafka API compatible, no JVM, fits in about 1 GB). Payloads move to
Protobuf with `buf` breaking-change checks; trace context travels in
record headers.

## Consequences

Per-wallet ordering and cross-wallet parallelism come from partitions;
consumers can batch and store offsets in the database for effectively-once
processing. The SQS adapter and LocalStack leave the project in phase 3.
