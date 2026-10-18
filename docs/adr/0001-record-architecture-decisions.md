# 0001. Record architecture decisions

- Status: accepted
- Date: 2026-10-16

## Context

The service grew from a hiring challenge into a project whose point is to
show, with evidence, how a wallet behaves under load. Most of its value is
in decisions and trade-offs — why the balance update is shaped the way it
is, why a queue was replaced, what a benchmark proves — and those decisions
used to live in one long ARCHITECTURE.md written in Portuguese.

## Decision

Every decision that constrains future work gets an Architecture Decision
Record in `docs/adr/`, numbered, in English, using `template.md`. A record
is never rewritten after it is accepted; a later record supersedes it.
ARCHITECTURE.md describes how the system works today and links to the
records that explain why.

## Consequences

Reviewers and future contributors can follow the reasoning without reading
the whole history. Writing a record is now part of finishing a phase.
