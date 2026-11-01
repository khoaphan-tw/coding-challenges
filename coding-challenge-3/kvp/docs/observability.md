# Observability & Runbook

Stack: OpenTelemetry SDK → `grafana/otel-lgtm` (Grafana at http://localhost:3000, anonymous admin). Decision record: [ADR-005](adr/ADR-005-observability.md).

## Log format

Every service logs JSON to stdout (also exported to Loki):

```json
{"time":"2026-11-01T07:12:03.141Z","level":"INFO","service":"dispatch-service",
 "msg":"driver reserved, offer created",
 "ride_id":"0192f3c4-…","driver_id":"D001","offer_id":"0192f3c5-…",
 "event_id":"0192f3c4-9a1…","causation_id":"0192f3c4-77e…","message_type":"DriverReserved",
 "saga_step":"OFFERED","trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","span_id":"00f067aa0ba902b7"}
```

Rules:
* Each state change logs exactly one `INFO` line with `from`, `to` and the cause (`event_id` or HTTP request id).
* Duplicates, conflicts and rejections log at `INFO` with `outcome` (`duplicate_skipped`, `reservation_conflict`, `invalid_transition`). These are expected, not errors.
* `ERROR` only for retries exhausted, DLQ, or infrastructure failures.

## "What happened to ride R001?"

**1. From the terminal (no Grafana needed):**

```bash
make trace-ride RIDE=<rideId>
# = docker compose logs --no-log-prefix ride-service driver-service dispatch-service \
#     | jq -c 'select(.ride_id=="<rideId>") | [.time,.service,.msg,.saga_step,.driver_id,.event_id]' | sort
```

Example output for reject → rematch → accept:

```
07:12:03.090 ride-service     ride created                 REQUESTED   -     e1
07:12:03.120 dispatch-service saga started                 STARTED     -     e1
07:12:03.135 ride-service     ride transitioned            MATCHING    -     e2
07:12:03.141 dispatch-service candidate selected           RESERVING   D001  e3
07:12:03.152 driver-service   driver reserved              OFFERED     D001  e4
07:12:03.160 dispatch-service offer created (30s)          OFFERED     D001  e5
07:12:03.171 ride-service     driver assigned              DRIVER_ASSIGNED D001 e6
07:12:09.402 dispatch-service offer rejected, excluded     MATCHING    D001  http:req-7
07:12:09.420 driver-service   driver released              AVAILABLE   D001  e8
07:12:09.431 ride-service     driver unassigned            MATCHING    D001  e9
07:12:09.440 dispatch-service candidate selected           RESERVING   D002  e10
...
07:12:15.010 dispatch-service offer accepted (pivot)       ACCEPTED    D002  http:req-12
07:12:15.031 ride-service     ride transitioned            DRIVER_ACCEPTED D002 e14
```

**2. Grafana → Explore → Loki:**

```logql
{service_name=~"ride-service|driver-service|dispatch-service"} | json | ride_id = "<rideId>"
```
Click a `trace_id` to jump to the Tempo trace.

**3. Follow one message:** `| event_id = "<eventId>" or causation_id = "<eventId>"` shows the message and everything it caused.

**4. One driver's history:** `| driver_id = "D001"`.

**5. API:** `GET /rides/{id}` (ride state) and `GET /rides/{id}/dispatch` (saga + all offers with outcomes + exclusions).

## Dashboards (provisioned: "Dispatch overview")

| Panel | Metric | Healthy |
|---|---|---|
| Ride request rate / errors / p95 | `http_server_duration{route="/rides"}` | p95 < 100 ms |
| Time to first offer | `dispatch_match_duration_seconds` | p95 < 500 ms |
| Offer outcomes | `offers_total{outcome=accepted\|rejected\|expired\|cancelled}` | — |
| Reservation conflicts | `driver_reservation_conflicts_total` | low % of offers |
| Sagas by state | `saga_active{status}` | `WAITING_FOR_DRIVER` not growing |
| Outbox backlog | `outbox_unpublished` | < 100 |
| Consumer lag | Redpanda `kafka_consumer_group_lag` | ~0 |
| DLQ | `dlq_messages_total` | 0 |

## Alerts (documented; not wired in the local stack)

| Alert | Condition | First action |
|---|---|---|
| DLQ not empty | `increase(dlq_messages_total[5m]) > 0` | Read DLQ headers, fix, `make replay-dlq` |
| Outbox stuck | `outbox_unpublished > 1000 for 2m` | Check Redpanda health, relay logs |
| Sagas stuck | saga non-terminal > 2 min without step | Check resumer logs; `GET /rides/{id}/dispatch` |
| Matching slow | `dispatch_match_duration_seconds p95 > 2s` | Redis health, consumer lag |
