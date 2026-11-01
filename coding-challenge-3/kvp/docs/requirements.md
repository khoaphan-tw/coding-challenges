# Requirements & Scope

Source: [coding-challenge-3.md](../../coding-challenge-3.md). This page turns the challenge into a checklist the design must satisfy, plus the extra constraints the team chose.

## Team constraints (beyond the challenge)

| # | Constraint | Consequence in the design |
|---|---|---|
| T1 | Microservices + saga | 3 Go services, each owns its database. The dispatch flow spans services, so it runs as an **orchestrated saga** ([ADR-003](adr/ADR-003-saga-orchestration.md)). |
| T2 | Go | Go 1.27, standard `net/http` router, `pgx`, `franz-go`, `go-redis`, OpenTelemetry SDK. |
| T3 | Observability | Structured logs + traces + metrics through OpenTelemetry into Grafana LGTM ([ADR-005](adr/ADR-005-observability.md)). |
| T4 | Everything runs in Docker | One `docker compose up`. Docker engine = **Colima** (no Docker Desktop); **lazydocker** for inspection ([local-development.md](local-development.md)). |
| T5 | Redis GEO + Redpanda + PostgreSQL | Redis GEO = matching index, Redpanda = event/command bus, PostgreSQL = source of truth ([ADR-004](adr/ADR-004-messaging-outbox-inbox.md)). |
| T6 | Docs in `docs/` | This folder. |

## Functional requirements → where they are met

| AC | Requirement (short) | Owner | Mechanism |
|---|---|---|---|
| AC-01 | `POST /rides`, starts `REQUESTED`, coordinate + passenger validation, `Idempotency-Key` | ride-service | `idempotency_keys` table written in the same transaction as the ride |
| AC-02 | Driver `OFFLINE ⇄ AVAILABLE`; driver status machine | driver-service | Conditional `UPDATE … WHERE status = …` per transition |
| AC-03 | Driver location update, latest stored, used by matching | driver-service → dispatch-service | Postgres row + `DriverLocationUpdated` event → Redis GEO projection |
| AC-04 | Nearest available driver within radius (default 2 km), tie-break by `driverId` | dispatch-service | Redis `GEOSEARCH` for candidates, pure-Go Haversine + sort ([ADR-001](adr/ADR-001-matching.md)) |
| AC-05 | Offer, accept/reject, 30 s timeout, rejected driver excluded for that ride | dispatch-service | `offers` table, `ride_exclusions` table, timeout sweeper |
| AC-06 | Explicit ride state machine, terminal states, duplicate transitions safe | ride-service | `ride.Transition()` table-driven state machine |
| AC-07 | Rematch after reject/timeout; stay `MATCHING` if nobody left | dispatch-service | Saga loops back to the `MATCHING` step |
| AC-08 | Idempotent ride creation, acceptance, transitions | all | Idempotency key + inbox dedup + idempotent state machines ([ADR-004](adr/ADR-004-messaging-outbox-inbox.md)) |
| AC-09 | One driver can never hold two offers/rides | driver-service (+ dispatch) | Atomic conditional update on the driver row; partial unique index on pending offers ([ADR-002](adr/ADR-002-concurrency.md)) |
| AC-10 | Async dispatch: RideCreated → event → matching | all | Transactional outbox → Redpanda → consumer |

## Non-functional requirements

| Concern | Target for this implementation | Why |
|---|---|---|
| Correctness | No driver double-booked, ever, including under duplicate delivery and crashes | Core of AC-08/AC-09 |
| Delivery | At-least-once messaging + idempotent consumers ("effectively once") | Exactly-once across Postgres + Kafka is not available without 2PC |
| Latency | Ride request → offer created in < 500 ms p95 on a laptop | Async but must feel instant |
| Consistency | Strong inside one service (Postgres transaction); eventual across services (milliseconds) | Microservice boundaries rule out a shared transaction |
| Traceability | "What happened to ride X?" answerable from logs alone, keyed by `ride_id`, `driver_id`, `event_id` | Observability requirement |
| Scale | Designed for 1k drivers; documented path to 1M ([scalability.md](scalability.md)) | Challenge requirement |

## Out of scope

Payments, pricing, auth, registration, real-time push to driver apps (drivers "receive" offers by polling `GET /drivers/{id}/offer` or reading logs), ETA/traffic, multi-region, Kubernetes. Listed again with future options in [scalability.md](scalability.md).

## Assumptions

1. Passengers `P001–P010` and drivers `D001–D020` are seeded (around District 1, Ho Chi Minh City).
2. "Driver receives an offer" = an `offers` row in `PENDING` + a `RideOfferCreated` event. No push channel.
3. If no driver is eligible, the ride stays `MATCHING`. The saga retries every 10 s (configurable) until the passenger cancels. Retrying is an addition; the ACs only require that the ride stays `MATCHING`.
4. Generated IDs are UUIDv7 (time-sortable). Seeded records use readable IDs (`P001`, `D001`).
5. Clocks: offer expiry uses the dispatch database clock (`now()`) so service clock skew does not matter.
