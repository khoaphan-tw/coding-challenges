# ADR-001: Matching: nearest available driver via a Redis GEO projection + Haversine in Go

* **Status:** Accepted
* **Date:** 2026-11-01
* **Requirements:** AC-03 (latest location), AC-04 (nearest within radius, tie-break), AC-07 (exclusions)
* **Related:** [ADR-002 concurrency](ADR-002-concurrency.md) (the pick is confirmed atomically), [ADR-008 data stores](ADR-008-data-stores.md)

## Context

A ride must be offered to the **nearest `AVAILABLE` driver within a configurable radius** (default 2 km). Ties go to the lexicographically smallest `driverId`. Drivers who rejected or timed out on a ride are never offered that ride again. Matching must use the **latest known location**.

The problem has two independent parts:

1. **Algorithm:** which driver wins.
2. **Spatial index:** how to find candidates fast as the fleet grows (1k → 1M drivers, up to 250k location updates/s; see [scalability.md](../scalability.md)).

## Decision drivers

| Driver | Why it matters |
|---|---|
| Correct per the spec | Exact radius cut-off, deterministic tie-break, exclusions |
| Freshness | AC-03: "matching uses the latest known location" |
| Query cost independent of fleet size | Must not scan all drivers per request |
| Write cost of location updates | Location is the dominant write load at scale |
| Operability on a laptop | Everything in Docker on Colima; Redis is already a team constraint (T5) |
| Never a wrong assignment | A stale index may cost time, never correctness |

## Part 1: matching algorithm

| Option | How it works | Pros | Cons | Verdict |
|---|---|---|---|---|
| **Greedy nearest (per request)** | Each ride picks the nearest eligible driver at the moment it is processed | Exactly what AC-04 asks; simple; low latency; easy to test | Not globally optimal; hot zones cause conflicts | ✅ **Chosen** |
| Batch assignment (Hungarian / min-cost matching) | Collect requests for ~100–500 ms per area, solve one assignment | Better total pickup distance; fewer conflicts | Adds latency and complexity; not what the ACs specify | Future (S4 in [scalability](../scalability.md)) |
| ETA-based (routing engine: OSRM, Valhalla, Google Distance Matrix) | Rank by drive time instead of straight-line distance | Realistic for cities | Needs a routing engine or paid API; traffic/ETA is out of scope | ❌ Out of scope |
| Scoring model (distance + rating + acceptance rate + ML) | Weighted or learned score | Business-optimal | Not specified; untestable against the ACs | ❌ |
| Broadcast to N drivers, first to accept wins | Offer to many at once | Fast acceptance | Violates "one offer at a time"; driver UX churn | ❌ |

**Distance formula:** Haversine (great-circle, Earth radius 6371.0088 km), as the challenge suggests. Vincenty/geodesic (e.g. GeographicLib) is more precise (≤0.5 %), but irrelevant at a 2 km radius. Equirectangular approximation is faster, but drifts at high latitudes.

## Part 2: spatial index technology

| Option | Query for "N nearest within R" | Location write cost | Freshness | Ops on a laptop | Scale path | Verdict |
|---|---|---|---|---|---|---|
| Plain Postgres + Haversine in SQL | Full table scan + sort, O(M) | Row update | Always fresh | Already there | Breaks at ~10⁵ drivers | ❌ No index |
| **PostGIS** (`geography`, GiST/SP-GiST, `ST_DWithin` + `<->` KNN) | Indexed KNN, O(log M) | Row update + index update (MVCC churn at high rates) | Always fresh | Extension in the same Postgres | Vertical, then Citus or partition by city | Strong runner-up |
| **Redis GEO** (sorted set + geohash: `GEOADD`, `GEOSEARCH … ASC`) | O(N + log M) in memory, sub-ms | O(log M), in memory, 100k+ ops/s per core | Eventually consistent if fed by events | One small container; Redis is a team constraint | Shard keys by city or H3 cell, Redis Cluster | ✅ **Chosen** |
| Tile38 (geospatial DB, Redis protocol) | Native nearby/within + geofences | In memory | Same | Extra, niche service | Leader/follower | ❌ Overkill; less familiar |
| Elasticsearch / OpenSearch (`geo_point`, `geo_distance` sort) | Indexed, rich filters | Near-real-time refresh (~1 s) | ~1 s stale | Heavy JVM container | Sharding built in | ❌ Too heavy, refresh lag |
| MongoDB `2dsphere` + `$near` | Indexed | Document update | Fresh | Extra database | Sharding built in | ❌ Second database, no gain |
| In-process grid (H3 / S2 / geohash buckets) | Fastest, no network | Memory only | Fresh per instance | Nothing to run | Must partition by area; lost on restart | Future, at extreme scale |
| Managed (DynamoDB + geo library, Google S2 services, AWS Location) | Varies | Varies | Varies | Not runnable locally | Managed | ❌ Must run offline in Docker |

Why Redis GEO over PostGIS, the closest alternative:

* **Separates the hot write path from the transactional database.** Location updates (the dominant load) never touch the rows used for reservation, so they cannot slow the `UPDATE … WHERE status='AVAILABLE'` that guarantees correctness.
* **Disposable read model.** The index can be rebuilt from driver-service at any time, so losing Redis costs matching time, never data.
* **Scale path is well known:** one GEO key per city or H3 cell, spread over a Redis Cluster.
* **Trade-off accepted:** the index is eventually consistent (milliseconds). PostGIS would always be fresh, but it couples reads, writes and reservations in one database.

## Decision

1. dispatch-service consumes `driver.events.v1` and maintains:
   * `geo:drivers:available`: a GEO set with **only** `AVAILABLE` drivers.
   * `driver:{id}` hash `{status, lat, lng, version}`. A **Lua script** applies an event only if `event.version > stored.version`, so duplicate and out-of-order events are ignored.
2. Candidate query:
   ```
   GEOSEARCH geo:drivers:available FROMLONLAT <lng> <lat> BYRADIUS <radius_m> m ASC WITHCOORD COUNT <20 + |excluded|>
   ```
   If the result is full, the cut-off may split a tie, so the query is widened until it is not.
3. Final choice in a **pure Go function**, so the rules are unit-tested without Redis:
   ```go
   func SelectNearest(pickup geo.Point, cands []Candidate, radiusM float64, excluded map[string]bool) (Ranked, bool)
   ```
   It drops excluded and non-`AVAILABLE` drivers, computes Haversine from exact coordinates, cuts at the radius (GEO hashes are ~0.6 m precise), sorts by `(distance, driverId)` and takes the first.
4. `MATCH_RADIUS_M` (default `2000`) configures the radius.
5. **Redis is only a hint.** driver-service confirms the pick atomically ([ADR-002](ADR-002-concurrency.md)). A stale candidate costs one retry, never a wrong assignment.

## Consequences

* ✅ Query cost depends on drivers inside the radius, not on fleet size.
* ✅ Exclusions are permanent per ride (`ride_exclusions` in `dispatch_db`), as AC-07 requires.
* ✅ Matching rules are 100 % unit-tested ([match_test.go](../../services/dispatch/internal/domain/match_test.go)); Lua versioning is integration-tested.
* ⚠️ A driver who just became `AVAILABLE` can be missed for a few ms (projection lag).
* ⚠️ After Redis loss, matching pauses until the rebuild (automatic on dispatch start, or `make rebuild-geo`).
* ⚠️ The Lua script touches two keys, which is fine on one Redis but not cluster-safe as written. Sharding needs hash tags (`{cell}`) in key names.

## When to revisit

* One GEO key above ~100k members or ~50k ops/s: shard by city or H3 cell.
* Reservation conflicts above ~20 % in an area: batch matching (S4).
* A product requirement for drive-time ranking: add a routing engine (OSRM or Valhalla, self-hosted).
