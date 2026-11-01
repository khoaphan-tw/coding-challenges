# Bottlenecks, Scaling Strategies & Trade-offs

This page answers the challenge requirement directly: **at least 3 potential bottlenecks, 3 scaling strategies and 2 architectural trade-offs.** The strategies are design only; none are built in this implementation, which targets 1,000 drivers. The numbers and the full growth path are in [scalability.md](scalability.md).

## Load assumptions

| | 1,000 drivers | 100,000 drivers | 1,000,000 drivers |
|---|---|---|---|
| Location updates (one per driver every 4 s) | 250 /s | 25,000 /s | **250,000 /s** |
| Ride requests at peak | 0.2 /s | 17 /s | ~170 /s (burst ~500 /s) |
| Offers | 0.3 /s | 25 /s | ~250 /s |

Location traffic grows about 1,000× faster than ride traffic. The bottlenecks below are mostly about locations, not rides.

---

## Bottlenecks

### B1. Location writes to the driver database
* **Where:** `PUT /drivers/{id}/location` → `UPDATE drivers` + an outbox row in `driver_db`.
* **Why it breaks:** one Postgres primary handles roughly 10–20k of these writes per second. 1M drivers need 250k/s.
* **Breaks at:** about 50–100k online drivers.
* **Signal:** `driver_db` write latency p99 > 50 ms, CPU > 70%, outbox backlog growing.

### B2. One Redis GEO key for all drivers
* **Where:** `geo:drivers:available`, written by every location/status event and read by every match.
* **Why it breaks:** Redis runs commands on one CPU core per shard, and one key cannot be split across a Redis Cluster. At 250k `GEOADD`/s plus searches, that core saturates.
* **Breaks at:** a few hundred thousand drivers.
* **Signal:** Redis CPU > 60%, `GEOSEARCH` p99 > 5 ms, projector consumer lag.

### B3. Hot zones (airport, stadium, end of a concert)
* **Where:** driver reservation (`UPDATE drivers … WHERE status='AVAILABLE'`).
* **Why it breaks:** many rides in a small area pick the same nearest drivers at the same moment. Most reservations fail and retry. This wastes round-trips, raises time-to-offer and locks the same driver rows. More servers do not help, because the contention is on the data.
* **Breaks at:** any scale, during local spikes.
* **Signal:** `driver_reservation_conflicts_total / offers_total` > 20% for an area.

### B4. Outbox polling (secondary)
Every change is written twice (the row plus its outbox row), and the relay polls and updates `published_at`. For location events this doubles the B1 load.

### B5. Topic partition count (secondary)
Consumers in a group cannot outnumber partitions. `driver.events.v1` with 12 partitions caps the geo projector at 12 parallel consumers.

---

## Scaling strategies

### S1. Take location off the transactional path → fixes B1, B4
* Driver-service publishes location **straight to Redpanda** (`driver.locations.v1`, compacted, keyed by `driverId`). No Postgres row per update.
* A background job saves the latest position to Postgres in **batches every 30 s** for history and recovery.
* Status changes (`AVAILABLE`, `OFFERED`, `BUSY`) stay transactional with the outbox. They are rare and must never be lost.
* **Result:** `driver_db` writes drop from 250k/s to ~35k rows per 30 s batch window. Matching still sees the latest location because the projector reads the stream.

### S2. Partition the geo index and dispatch by area → fixes B2, B3, B5
* Shard the GEO index by **city → H3 cell (resolution 7, ~5 km²)**: keys `geo:{city}:{cell}`. A search queries the pickup cell plus its neighbours. Redis Cluster spreads cells over many nodes.
* Route rides to dispatch workers **by area** (partition key = city/cell instead of `rideId`). Each worker owns its areas, so two workers rarely compete for the same driver.
* Raise partition counts (100+ for location topics) so consumers can scale with them.

### S3. Scale out stateless services and split databases → general capacity
* Run 3–N replicas of each Go service behind the gateway. Kafka partitions spread the saga work; the timeout sweeper is already safe with many replicas (`FOR UPDATE SKIP LOCKED`).
* Give each service **its own Postgres cluster** (today: one container, three databases). Add read replicas for `GET` endpoints. Partition `rides` by month and archive old partitions. Shard `driver_db` by city when one primary is not enough.
* Replace the outbox poller with **CDC** (Debezium / Redpanda Connect reading the Postgres WAL).
* **Cache** `GET /rides/{id}` for ~1 s, because clients poll it.

### S4. Batch matching in hot zones → fixes B3
Instead of "nearest driver per request, first come first served", collect requests in a busy cell for ~200 ms and assign them **together** (greedy on distance, or the Hungarian algorithm). There are fewer conflicts and better total pickup distance. The cost is ~200 ms of extra latency, so use it only in cells above a conflict threshold.

---

## Architectural trade-offs

### T1. Microservices + saga vs. a modular monolith
| | Our choice: 3 microservices + orchestrated saga | Alternative: modular monolith |
|---|---|---|
| Gains | Location-heavy driver path scales separately from rides; clear ownership; failures isolated | One database transaction can update ride + driver + offer, so it is consistent immediately |
| Costs | Ride/driver/offer are consistent only after a few ms; outbox/inbox, more containers, harder debugging (hence tracing) | Everything scales together; one bad deploy stops everything |
| Switch when | — | The team is small and scale stays below ~100k drivers |

### T2. Redis GEO projection (fast, slightly stale) vs. always-fresh reads
| | Our choice: dispatch keeps its own Redis index fed by driver events | Alternative: query driver-service / Postgres on every match |
|---|---|---|
| Gains | Matching is in-memory and fast; dispatch keeps working if driver-service is slow | Never stale |
| Costs | A candidate may already be busy or offline, which costs one failed reservation and a retry; the index must be rebuildable | Every match hits the busiest database (B1), plus a synchronous dependency between services |
| Why it's safe | Redis only *suggests* drivers. Postgres decides with an atomic update ([ADR-002](adr/ADR-002-concurrency.md)), so staleness costs time, never correctness | — |

### T3. Nearest driver per request (greedy) vs. global optimisation
Greedy is what the challenge asks for: simple, predictable, low latency. It is not optimal fleet-wide and causes B3 in hot zones. Batch matching (S4) gives better results but adds latency and complexity.

### T4. At-least-once delivery + idempotency vs. exactly-once
Exactly-once across Postgres and Kafka needs distributed transactions, which we avoid. We accept duplicates and make every handler safe to repeat (inbox table, state machines, `rideId` guards). The cost is two extra tables per service and cleanup jobs.

---

## Summary

| Requirement | Minimum | Provided |
|---|---|---|
| Potential bottlenecks | 3 | 5 (B1–B5) |
| Scaling strategies | 3 | 4 (S1–S4) |
| Architectural trade-offs | 2 | 4 (T1–T4) |
