# ADR-005: Observability: OpenTelemetry → Grafana LGTM, slog text logs, one trace per ride

* **Status:** Accepted
* **Date:** 2026-11-01
* **Requirements:** challenge §5 Observability ("What happened to ride R001?" from logs, using `rideId`, `driverId`, `eventId`); team constraint T3
* **Related:** [observability.md](../observability.md) (runbook)

## Context

A ride crosses 3 services and ~15–40 messages, over seconds to minutes (offer timeouts, retries). An engineer must be able to follow one ride from the **logs** alone, and ideally see it as a **trace** and as **metrics**. Everything must run locally in Docker on a laptop.

## Decision drivers

* Correlate by `ride_id`, `driver_id`, `event_id`, **and one id for the whole journey** (`trace_id`).
* Vendor-neutral instrumentation: switching backends must not change code.
* Light enough for Colima (8 GB VM shared with Postgres, Redpanda, Redis and the services).
* Readable plain logs in the terminal (`docker compose logs`, lazydocker) without any UI.

## Part 1: instrumentation API

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **OpenTelemetry SDK** (traces + metrics + logs, OTLP) | CNCF standard; one API for all three signals; any backend (Grafana, Jaeger, Datadog, Honeycomb, …); W3C trace context propagation | SDK setup is verbose; logs signal newest of the three | ✅ **Chosen** |
| Vendor SDKs (Datadog `dd-trace-go`, New Relic Go agent) | Rich auto-instrumentation | Lock-in; needs an account; not runnable offline | ❌ |
| Prometheus client + Jaeger client + separate logger | Mature, simple | Three APIs, no shared context; Jaeger clients are deprecated in favour of OTel | ❌ |
| OpenCensus / OpenTracing | — | Both archived and merged into OpenTelemetry | ❌ |

## Part 2: backend stack (local)

| Option | Containers / footprint | Logs | Traces | Metrics | Correlation (logs ↔ traces) | Verdict |
|---|---|---|---|---|---|---|
| **grafana/otel-lgtm** (Collector + Loki + Tempo + Prometheus + Grafana in one image) | 1 container, ~1 GB RAM | Loki | Tempo | Prometheus | Built in (trace_id link, "Related logs") | ✅ **Chosen** (dev only) |
| ELK / EFK (Elasticsearch + Logstash/Fluentd + Kibana) + APM | 3–4 containers, Elasticsearch JVM heap ≥ 1–2 GB | Excellent search | Elastic APM | Metricbeat | Good inside Elastic | ❌ Heavy for a laptop VM |
| Jaeger + Prometheus + Grafana + Loki (separate) | 4–5 containers | Loki | Jaeger | Prometheus | Manual wiring | ❌ Same result, more config |
| SigNoz (ClickHouse based) | Several containers incl. ClickHouse, ZooKeeper | Yes | Yes | Yes | Built in | ❌ Heavier |
| Uptrace (ClickHouse based) | ClickHouse + app | Yes | Yes | Yes | Built in | ❌ Less common |
| OpenObserve | 1 binary | Yes | Yes | Yes | Built in | Viable alternative; smaller ecosystem |
| SaaS: Datadog, New Relic, Honeycomb, Grafana Cloud | None locally | Yes | Yes | Yes | Excellent | ❌ Needs account and network; same OTLP export works later |

Production would export the same OTLP data to a managed backend (Grafana Cloud, Datadog, …) or a scaled LGTM deployment, with no code change.

## Part 3: logging library and format

| Library | Pros | Cons | Verdict |
|---|---|---|---|
| **`log/slog` (standard library)** | No dependency; structured; context-aware handlers; official OTel bridge (`otelslog`) | Slightly slower than zerolog/zap (irrelevant at our volume) | ✅ **Chosen** |
| `uber-go/zap` | Very fast, mature | Extra dependency; own API (`zap.String(...)`) | ❌ |
| `rs/zerolog` | Zero-allocation JSON | Extra dependency; JSON-first | ❌ |
| `sirupsen/logrus` | Familiar | In maintenance mode; slower | ❌ |

| Format on stdout | Pros | Cons | Verdict |
|---|---|---|---|
| **Plain text `key=value`** (slog `TextHandler`) | Readable in `docker compose logs`/lazydocker; `grep trace_id=…` works | Nested payloads are quoted strings | ✅ **Chosen** (team preference) |
| JSON lines | Machine-parsable with `jq`; nested bodies | Noisy to read by eye | Available: `LOG_FORMAT=json` |

Format does not affect Loki: records go there through OTLP with every key as a structured field.

## Part 4: trace model for a long-running journey

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| One trace per request/message (default) | Standard; short traces | A ride becomes 5–30 disconnected traces (accept, reject, timeout, retries) | ❌ |
| Per-request traces + `ride.id` attribute + span links | Standard; searchable with TraceQL `{ span.ride.id = "…" }` | Still many traces to open | ✅ Kept as well |
| **One trace per ride**: save the create request's `traceparent` with the ride and saga; later steps continue it, with a link to the triggering request | One `trace_id` shows the whole journey in Tempo and finds every log line | Minutes-long traces are unusual; relies on storing `traceparent` | ✅ **Chosen** |
| Correlation id only (no tracing) | Simple | No timing, no causality view | ❌ |

## Decision

* **Instrumentation:** OpenTelemetry Go SDK (`otelhttp`, franz-go header propagation, `redisotel`, `otelslog`) → OTLP gRPC → `grafana/otel-lgtm` (pinned `0.39.0`).
* **Logs:** `log/slog` text lines on stdout and OTLP to Loki. A custom handler adds `ride_id`, `driver_id`, `offer_id`, `event_id`, `causation_id`, `message_type`, `saga_step`, `trace_id`, `span_id` from the context.
* **Payloads:** HTTP request and response bodies on the access log; every produced and consumed message with its value (truncated at 4 KB).
* **One trace per ride** ([pkg/obs/trace.go](../../pkg/obs/trace.go)): `rides.trace_parent` and `dispatch_sagas.trace_parent`, `ContinueTrace`, span links, and the `ride.id` span attribute. API responses include `traceId`.
* **Metrics:** RED for HTTP plus domain metrics: `rides_created_total`, `offers_total{outcome}`, `dispatch_match_duration_seconds`, `driver_reservation_conflicts_total`, `outbox_unpublished`, `messages_consumed_total{outcome}`.
* **Durable audit:** `ride_transitions` and `saga_steps`, exposed by `GET /rides/{id}/history` and `/dispatch`.
* **Health:** `/healthz` (liveness), `/readyz` (Postgres + Redis + broker).

## Consequences

* ✅ `make trace-ride`, `make trace-logs TRACE=…`, Loki `| trace_id="…"` and Tempo by trace id all show the same journey.
* ✅ A provisioned Grafana dashboard ("Dispatch overview").
* ⚠️ Every message is logged twice (produce + consume) with payloads. Fine locally; production should sample those lines or log them at `DEBUG`.
* ⚠️ The LGTM image is for development only (no persistence, no HA).
* ⚠️ Alerts are documented, not wired.
