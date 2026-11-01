# ADR-005: Observability: OpenTelemetry → Grafana LGTM, slog text logs, one trace per ride

* **Status:** Accepted
* **Date:** 2026-11-01
* **Requirements:** challenge §5 Observability ("What happened to ride R001?" from logs, using `rideId`, `driverId`, `eventId`); team constraint T3
* **Related:** [observability.md](../observability.md) (runbook)

## Context

A ride crosses 3 services and ~15–40 messages, over seconds to minutes (offer timeouts, retries). An engineer must be able to follow one ride from the **logs** alone, and ideally see it as a **trace** and as **metrics**. Everything must run locally in Docker on a laptop.

## Part 1: instrumentation and backend

**Instrumentation: OpenTelemetry.** It is the CNCF standard, has one API for traces, metrics and logs, and exports to any backend over OTLP, so the code does not change if the backend does. No comparison needed.

**Local backend:**

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **grafana/otel-lgtm** (Collector + Loki + Tempo + Prometheus + Grafana in one image) | 1 container; logs ↔ traces links built in | Development only (no persistence, no HA) | ✅ **Chosen** |
| ELK (Elasticsearch + Logstash + Kibana) | Excellent log search | 3–4 containers and a large JVM heap; too heavy for a laptop VM | ❌ |

Production would send the same OTLP data to a managed backend (Grafana Cloud, Datadog, …) with no code change.

## Part 2: logs

**Library: `log/slog`** from the standard library: structured, context-aware, no dependency, and an official OpenTelemetry bridge (`otelslog`).

**Format: plain `key=value` text** (team preference): readable in `docker compose logs` and lazydocker, and `grep trace_id=…` works. `LOG_FORMAT=json` switches to JSON lines. The format does not matter for Loki, which receives every key as a structured field through OTLP.

## Part 3: trace model for a long-running journey

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| One trace per request/message (the default) | Standard; short traces | A ride becomes 5–30 disconnected traces (accept, reject, timeout, retries) | ❌ |
| **One trace per ride**: save the create request's `traceparent` with the ride and saga; later steps continue it, with a link to the triggering request | One `trace_id` shows the whole journey in Tempo and finds every log line | Minutes-long traces are unusual | ✅ **Chosen** |

## Decision

* **Instrumentation:** OpenTelemetry Go SDK (`otelhttp`, `otelgrpc` for the gRPC API v2, franz-go header propagation, `redisotel`, `otelslog`) → OTLP gRPC → `grafana/otel-lgtm` (pinned `0.39.0`).
* **Logs:** `log/slog` text lines on stdout and OTLP to Loki. A custom handler adds `ride_id`, `driver_id`, `offer_id`, `event_id`, `causation_id`, `message_type`, `saga_step`, `trace_id`, `span_id` from the context.
* **Payloads:** HTTP request and response bodies on the access log; every produced and consumed message with its value (truncated at 4 KB).
* **One trace per ride** ([pkg/obs/trace.go](../../pkg/obs/trace.go)): `rides.trace_parent` and `dispatch_sagas.trace_parent`, `ContinueTrace`, span links, and the `ride.id` span attribute. API responses include `traceId`.
* **Metrics:** RED for HTTP plus domain metrics: `rides_created_total`, `offers_total{outcome}`, `dispatch_match_duration_seconds`, `driver_reservation_conflicts_total`, `outbox_unpublished`, `messages_consumed_total{outcome}`.
* **Durable audit:** `ride_transitions` and `saga_steps`, exposed by `GET /rides/{id}/history` and `/dispatch`.
* **Health:** `/healthz` (liveness), `/readyz` (Postgres + Redis + broker).

## Consequences

* ✅ `make trace-ride`, `make trace-logs TRACE=…`, Loki `| trace_id="…"` and Tempo by trace id all show the same journey.
* ✅ A provisioned Grafana dashboard ("Dispatch overview").
* ⚠️ Every message is logged twice (produce + consume) with payloads. Fine locally; production should sample those lines.
