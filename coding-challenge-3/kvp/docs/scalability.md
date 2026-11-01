# Scalability & Performance

## Load model (back-of-the-envelope)

Assumptions: a driver app sends a location update every **4 s** while online. At peak, each driver completes about **0.6 rides/hour**, and a ride needs on average **1.5 offers**. One saga is about **12 messages**.

| | 1,000 drivers | 100,000 drivers | 1,000,000 drivers |
|---|---|---|---|
| Location writes | 250 /s | 25,000 /s | **250,000 /s** |
| Ride requests (peak) | 0.2 /s | 17 /s | ~170 /s (×3 burst ≈ 500 /s) |
| Offers | 0.3 /s | 25 /s | ~250 /s (burst ~750) |
| Saga messages | ~3 /s | ~200 /s | ~2,000 /s (burst ~6,000) |
| Location bandwidth (~100 B) | 25 KB/s | 2.5 MB/s | 25 MB/s |
| Redis GEO memory (~120 B/member) | 120 KB | 12 MB | ~120 MB |
| Ride data/day (~1 KB/ride, daily avg ≈ ⅓ of peak) | ~5 MB | ~500 MB | ~5 GB |

**Key conclusion:** the location stream grows to ~250k writes/s. Dispatch traffic stays below ~1k/s. So scaling is mostly about **location ingestion and the geo index**, not about rides.

## Bottlenecks (in the order they hit)

1. **`driver_db` location writes.** Every location update is a Postgres `UPDATE` plus an outbox row. A single primary handles about 10–20k such writes/s, so this breaks between 100k and 1M drivers.
2. **Single Redis GEO key.** Redis is single-threaded per shard. `GEOADD` at 250k/s plus `GEOSEARCH` on one sorted set saturates one core, and one key cannot be split across a Redis cluster.
3. **Outbox polling relay.** Polling + `UPDATE published_at` doubles DB write load for location events.
4. **Hot zones (airport, stadium).** Many sagas pick the same few nearest drivers, so many reservations fail (wasted round-trips) and the same driver rows are contended.
5. **Partition count.** `driver.events.v1` with 12 partitions caps consumer parallelism of the geo projector.

## Evolution

### Stage 1: 1,000 drivers (this implementation)
One instance of each service, one Postgres (3 databases), one Redis, one Redpanda broker. Everything fits on a laptop. Location goes through Postgres for simplicity and auditability.

### Stage 2: 100,000 drivers
* **Location fast path:** driver-service writes location **only to Redpanda** (`driver.locations.v1`, compacted, keyed by `driverId`) and snapshots to Postgres every 30 s in batches. Status changes stay transactional (outbox). Matching still uses the latest location because the projector reads the stream. *(strategy 1)*
* **Scale stateless services horizontally:** 3–6 replicas each. Partitions: 24–48 per topic. Dispatch sagas spread by `rideId`. *(strategy 2)*
* **Postgres:** a separate cluster per service; read replicas for `GET` endpoints; `rides` range-partitioned by `created_at` (monthly) with old partitions archived.
* **Outbox → CDC:** Redpanda Connect / Debezium reads the WAL instead of polling.
* **Caching:** cache `GET /rides/{id}` for 1 s (polling clients). Passenger lookups are cached in-process (static seed).

### Stage 3: 1,000,000 drivers
* **Geo partitioning:** shard the index by **city → H3 cell (res 7, ~5 km²)**. Keys `geo:{city}:{h3}`; a search queries the pickup cell plus its ring-1 neighbours. Redis Cluster spreads cells across nodes. *(strategy 3)*
* **Cell-affine dispatch:** partition `ride.events` and dispatch replicas **by city/region** instead of `rideId`. Each dispatcher owns a set of cells, so most reservation conflicts disappear (one owner per area). This gives "sharded orchestrators". *(strategy 4)*
* **Batch matching in hot cells:** collect requests for ~200 ms per cell and solve the assignment together (Hungarian / greedy on distance). This beats first-come-nearest and stops reservation storms. *(strategy 5)*
* **Shard `driver_db` by city** (Citus or app-level). Reservation stays a single-row atomic update inside one shard.
* **Multi-broker Redpanda** (3+ nodes, RF=3), 100+ partitions for location topics.

## Trade-offs

| Trade-off | We chose | Cost | Alternative |
|---|---|---|---|
| **Consistency vs latency/availability** of the geo index | Eventually consistent projection | Stale candidates mean wasted reservations | Synchronous write-through by driver-service (fresher, couples services) |
| **Microservices vs monolith** | Microservices | Network hops, eventual consistency, ops cost | Modular monolith: one transaction for ride+driver+offer; simpler up to ~100k drivers |
| **Greedy nearest vs global optimal matching** | Greedy per request (spec) | Sub-optimal fleet-wide; contention in hot zones | Batch assignment per cell (adds ~200 ms latency) |
| **Durability vs throughput for location** | Postgres per update (stage 1) | Write amplification | Stream-only location + periodic snapshot (loses ≤ 30 s of history on a crash) |
| **Postgres sweeper vs timer service** | Sweeper (`SKIP LOCKED`) | ≤ 1 s late; polling | Redis ZSET delay queue / Temporal timers |

## Signals to evolve (alerts)

* `driver_db` write latency p99 > 50 ms, or CPU > 70% → stage 2 location path
* Redis CPU > 60% or `GEOSEARCH` p99 > 5 ms → geo sharding
* `driver_reservation_conflicts_total / offers_total` > 20% in a cell → cell-affine dispatch / batch matching
* `consumer_lag` growing for > 1 min → more partitions + replicas
