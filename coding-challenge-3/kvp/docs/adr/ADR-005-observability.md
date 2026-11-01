# ADR-005: Observability with OpenTelemetry + Grafana LGTM

* **Status:** Accepted
* **Date:** 2026-11-01
* **Context:** challenge observability section; team constraint T3

## Context

A ride crosses 3 services and up to ~15 messages. We need to answer **"What happened to ride R001?"** from logs, using `rideId`, `driverId` and `eventId`, and to see latency and failure rates.

## Decision

* **Instrumentation:** OpenTelemetry Go SDK in all services (traces, metrics, logs) → OTLP → **`grafana/otel-lgtm`**, one container with Collector, Loki, Tempo, Prometheus and Grafana. It is light enough for Colima.
* **Logs:** `log/slog` JSON to **stdout** *and* the OTel log bridge (`otelslog`). Stdout keeps `docker compose logs` + `jq` working without Grafana.
* **Mandatory log fields** (set once in `context.Context` and added by a slog handler):
  `service`, `ride_id`, `driver_id`, `offer_id`, `event_id`, `causation_id`, `message_type`, `saga_step`, `trace_id`, `span_id`.
* **Trace propagation:** W3C `traceparent` stored in `outbox.headers` and copied to Kafka headers, so a single trace spans HTTP → DB → outbox → Redpanda → consumer. Spans are linked even across the 30 s timeout gap.
* **Audit tables** (`ride_transitions`, `saga_steps`) form a durable per-ride timeline. `GET /rides/{id}/dispatch` exposes it.
* **Metrics (RED + domain):** `http_server_duration` · `rides_created_total` · `dispatch_match_duration_seconds` (RideRequested → offer) · `offers_total{outcome}` · `driver_reservation_conflicts_total` · `saga_active{status}` · `outbox_unpublished` · `consumer_lag` · `dlq_messages_total`.
* **Health:** `/healthz` (liveness), `/readyz` (Postgres + Redpanda + Redis reachable).

Runbook and queries: [observability.md](../observability.md).

## Consequences

* ✅ One `docker compose up` gives Grafana with logs, traces and metrics linked by `trace_id`.
* ⚠️ The LGTM image is for development only. Production uses the same OTLP export pointed at a managed backend; no code change.
