# Observability & Runbook

Stack: OpenTelemetry SDK → `grafana/otel-lgtm` (Grafana at http://localhost:3000, anonymous admin). Decision record: [ADR-005](adr/ADR-005-observability.md).

## Log format

Every service writes **plain-text `key=value` lines** to stdout (Go `log/slog` text format; set `LOG_FORMAT=json` for JSON lines) and exports the same records to Loki through OpenTelemetry, where every key is a queryable field. Fields present on every line of a ride's journey:

| Field | Meaning |
|---|---|
| `trace_id`, `span_id` | **The ride's trace**: one id for the whole journey, the same id as in Tempo |
| `ride_id`, `driver_id`, `offer_id` | Business ids |
| `event_id`, `causation_id`, `message_type` | The message being handled or produced, and the message or request that caused it |
| `saga_step`, `from`, `to`, `outcome` | State changes; `outcome` = `duplicate_skipped`, `invalid_transition`, `reservation_conflict`, … |

Three kinds of "traffic" lines carry full payloads (one-line, quoted, truncated at 4 KB):

**HTTP access log**: one line per request (`msg="http request"`):

```
time=2026-11-01T10:22:42.424Z level=INFO msg="http request" service=ride-service method=POST path=/rides route="POST /rides" status=201 duration_ms=5 http_trace_id=84101c67… idempotency_key=tx-841 request_body="{\"passengerId\":\"P001\",\"pickup\":{…},\"destination\":{…}}" response_body="{\"rideId\":\"01a196a2-…\",\"status\":\"REQUESTED\",…,\"traceId\":\"84101c67…\"}" ride_id=01a196a2-… trace_id=84101c67830cbb30061ad0a93f7bd686 span_id=4ffa6093b5db045c
```

When the request acts on a ride (accept, reject, cancel, complete), `trace_id` is the **ride's** trace, and `http_trace_id` is the request's own short trace. Health checks are not logged.

**Produced message**: one line per message written to the outbox (`msg="message produced"`), logged under the new message's `event_id`, with `topic`, `key` and the full envelope in `value`:

```
time=2026-11-01T10:22:42.434Z level=INFO msg="message produced" service=dispatch-service direction=produce via=outbox topic=ride.commands.v1 key=01a196a2-… value="{\"eventId\":\"01a196a2-0302-…\",\"type\":\"StartMatching\",…,\"causationId\":\"01a196a2-02f5-…\",\"rideId\":\"01a196a2-…\"}" ride_id=01a196a2-… event_id=01a196a2-0302-… message_type=StartMatching causation_id=01a196a2-02f5-… trace_id=84101c67… span_id=e43baa9e…
```

**Consumed message**: one line per message received (`msg="message consumed"`) with `consumer`, `topic`, `partition`, `offset`, `key` and the raw `value`. Then come the handler's own lines.

Other rules:
* Each state change logs exactly one `INFO` line with `from`, `to` and its cause.
* Duplicates, conflicts and rejections log at `INFO` with an `outcome`: they are expected, not errors.
* `ERROR` only for retries exhausted, DLQ, or infrastructure failures.
* Volume: every message is logged twice (produce + consume). Fine locally; in production you would sample the payload lines or drop them to `DEBUG`.

## All logs of one journey: query by trace id

The ride's trace id is returned by `POST /rides` and `GET /rides/{id}` (`"traceId"`), and printed by `make trace-ride`.

| Where | How |
|---|---|
| Terminal | `make trace-logs TRACE=<traceId>`: every line from all services in time order, with HTTP request/response bodies and message values (same as `docker compose logs --no-log-prefix \| grep trace_id=<traceId> \| sort`) |
| Grafana dashboard | *Dispatch overview* → paste it into the `trace_id` box → "Ride timeline" panel |
| Grafana Explore → Loki | `{service_name=~"ride-service\|driver-service\|dispatch-service"} \| trace_id = "<traceId>"` |
| Grafana Explore → Tempo | paste the trace id (see below); **Related logs** jumps back to Loki |

`make trace-logs` prints the raw log lines shown above, sorted by time.

## "What happened to ride R001?"

**1. From the terminal, with logs only (no Grafana needed):**

```bash
make trace-ride RIDE=<rideId>
```

[scripts/trace-ride.sh](../scripts/trace-ride.sh) merges `docker compose logs` of the three services, keeps this ride's lines (`ride_id=…`) without the HTTP and message-payload lines, and sorts them by time. It ends with the ride's trace id and a Tempo link. Excerpt:

```
time=…42.422Z level=INFO msg="ride created" service=ride-service status=REQUESTED passenger_id=P001 ride_id=01a196a2-… trace_id=84101c67…
time=…42.434Z level=INFO msg="saga started, asking ride-service to start matching" service=dispatch-service saga_step=STARTED ride_id=01a196a2-… trace_id=84101c67…
time=…42.443Z level=INFO msg="ride transitioned" service=ride-service from=REQUESTED to=MATCHING action=StartMatching ride_id=01a196a2-… trace_id=84101c67…
time=…42.483Z level=INFO msg="nearest eligible driver selected at 300 m, reserving" service=dispatch-service saga_step=RESERVING driver_id=TX-A-841 ride_id=01a196a2-… trace_id=84101c67…
time=…42.491Z level=INFO msg="driver status changed" service=driver-service from=AVAILABLE to=OFFERED driver_id=TX-A-841 ride_id=01a196a2-… trace_id=84101c67…
…
time=…43.452Z level=INFO msg="driver rejected the offer: excluded for this ride, released, ride back to MATCHING" service=dispatch-service saga_step=MATCHING driver_id=TX-A-841 …
```

For every line including payloads, use `make trace-logs TRACE=<traceId>`.

Plain `grep` works too: `docker compose logs | grep <rideId>`.

**2. Grafana → Dashboards → "Dispatch overview"**: type the ride id into the `ride_id` box at the top; the "Ride timeline" panel shows the same lines. Or in Explore → Loki:

```logql
{service_name=~"ride-service|driver-service|dispatch-service"} | ride_id = "<rideId>"
```

(`ride_id`, `driver_id`, `event_id` … are OpenTelemetry log attributes, stored by Loki as structured metadata, so no `| json` is needed.) All lines of one ride share a `trace_id`: open it in Tempo to see the whole flow across HTTP, Postgres and Redpanda as one trace.

**3. Follow one message:** `| event_id = "<eventId>" or causation_id = "<eventId>"` shows the message and everything it caused.

**4. One driver's history:** `| driver_id = "D001"`.

**5. Durable history (survives log rotation):**
* `GET /rides/{id}/history`: every ride status change from `ride_transitions` (from, to, action, driver, cause).
* `GET /rides/{id}/dispatch`: the saga, every offer with its outcome, the excluded drivers and every saga step.

## Tempo: the whole journey as one trace

**One ride = one trace.** A ride's journey lasts seconds to minutes and is driven by many separate triggers: the create request, the driver's accept/reject calls, the passenger's cancel/complete, the 30 s offer-timeout worker and the retry worker. Each would normally start its own trace. Instead:

* The trace of `POST /rides` becomes **the ride's trace**. Its `traceparent` is saved on the ride (`rides.trace_parent`) and the saga (`dispatch_sagas.trace_parent`), and travels in every Kafka message header.
* Every later step for that ride (`offer.accept`, `offer.reject`, `offer.timeout`, `saga.retry`, `match driver`, `ride.Cancel`, `ride.Complete`) **continues that trace** ([obs.ContinueTrace](../pkg/obs/trace.go)). The HTTP request that triggered the step keeps its own short trace and is attached as a **span link**.
* Every span carries `ride.id` (HTTP spans also carry `driver.id`), so you can search by ride.

### Find the trace id

| Where | How |
|---|---|
| API | `GET /rides/{id}` and `POST /rides` return `"traceId"`; `GET /rides/{id}/dispatch` → `saga.traceId` |
| Terminal | `make trace-ride RIDE=<rideId>` prints the trace id **and a ready-to-open Grafana link** |
| Logs | every log line of the ride has the same `trace_id` |

### Open it

1. **Direct link:** open the "Open in Tempo" URL printed by `make trace-ride`.
2. **By trace id:** Grafana http://localhost:3000 → **Explore** → data source **Tempo** → query type **TraceQL** → paste the trace id → **Run query**.
3. **By ride id (no trace id needed):** Explore → Tempo → TraceQL:
   ```
   { span.ride.id = "<rideId>" }
   ```
   The result lists the ride's journey trace (root `POST /rides`, long duration) plus the short HTTP-request traces of accept/reject/complete that link into it. Open the long one.
4. **From a log line:** in Explore → Loki (or the dashboard's "Ride timeline"), expand a line and click its `trace_id`.

### Read it

A request → reject → rematch → accept → complete journey (48 spans, 3 services):

```
0.000s ride-service      POST /rides
0.041s dispatch-service  consume RideRequested
0.083s ride-service      consume StartMatching
0.090s dispatch-service  match driver → geosearch
0.132s driver-service    consume ReserveDriver
0.182s ride-service      consume AssignDriver
1.087s dispatch-service  offer.reject        (linked to the driver's HTTP request)
1.089s dispatch-service  match driver
1.132s driver-service    consume ReleaseDriver / ReserveDriver
1.182s ride-service      consume AssignDriver
2.120s dispatch-service  offer.accept        (linked to the driver's HTTP request)
2.133s driver-service    consume MarkDriverBusy
2.133s ride-service      consume MarkAccepted
3.160s ride-service      ride.Complete       (linked to the HTTP request)
3.232s driver-service    consume ReleaseDriver
```

Tips in the trace view: **Service & Operation** shows the tree; filter by `span.ride.id`, a service or an operation name; each span's **Attributes** show `messaging.message.id` (the `event_id`), `ride.id` and `driver.id`; **Links** jump to the HTTP request that triggered a step; **Related logs** opens the matching Loki lines. Gaps in the timeline are waiting time, e.g. ~30 s before `offer.timeout`.

## Dashboard (provisioned automatically: "Dispatch overview")

| Panel | Metric | Healthy |
|---|---|---|
| HTTP p95 by service | `http_server_request_duration_seconds` | p95 < 100 ms |
| Time to first offer | `dispatch_match_duration_seconds` | p95 < 500 ms |
| Offer outcomes | `offers_total{outcome=accepted\|rejected\|expired\|cancelled}` | — |
| Reservation conflicts | `driver_reservation_conflicts_total` | low % of offers |
| Outbox backlog | `outbox_unpublished` | < 100 |
| Messages consumed | `messages_consumed_total{consumer, outcome}` | no `dlq` outcome |
| DLQ | `messages_consumed_total{outcome="dlq"}` / `dlq_messages_total` | 0 |

## Alerts (documented; not wired in the local stack)

| Alert | Condition | First action |
|---|---|---|
| DLQ not empty | `increase(dlq_messages_total[5m]) > 0` | Read DLQ headers, fix, `make replay-dlq` |
| Outbox stuck | `outbox_unpublished > 1000 for 2m` | Check Redpanda health, relay logs |
| Sagas stuck | saga non-terminal > 2 min without step | Check resumer logs; `GET /rides/{id}/dispatch` |
| Matching slow | `dispatch_match_duration_seconds p95 > 2s` | Redis health, consumer lag |
