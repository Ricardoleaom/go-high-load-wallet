# 0010. Observe with Prometheus, OpenTelemetry and Jaeger, behind an admin port

- Status: accepted
- Date: 2026-10-16

## Context

Before phase 1 the service exposed counters only: no latency, no pool or
backlog visibility, no traces. Measuring and later optimizing needs all
three, without making the API depend on its observability backends.

## Decision

- Prometheus histograms for HTTP, submit, SQS processing and outbox
  rounds, with the request's trace ID attached as an exemplar; pool
  statistics, Go runtime and an outbox backlog collector that drops its
  series instead of failing a scrape.
- OpenTelemetry traces for HTTP, every SQL statement (`otelpgx`, only
  under a recording parent) and background rounds, exported over OTLP to
  Jaeger v2. Tracing is configured by the standard `OTEL_*` variables and
  is a no-op without an endpoint. Logs carry `trace_id` and `span_id`.
- Metrics and pprof live on an admin server bound to loopback by default
  (`ADMIN_ADDR`); pprof is opt-in (`PPROF_ENABLED`), never serves the
  command line and caps profiles at 60 s.
- Jaeger instead of Grafana Tempo: Tempo 3 changed its deployment model
  and configuration, while Jaeger's all-in-one image needs none, has its
  own UI and a native Grafana datasource, and uses less memory on the
  8 GB reference machine.

## Consequences

`make up-obs` gives a provisioned Grafana dashboard where a latency spike
links to its trace. Tracing costs CPU, so benchmarks sample 1% and one
control run measures the overhead with tracing off.
