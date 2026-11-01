# ADR-001: Matching: nearest available driver via a Redis GEO projection + Haversine in Go

* **Status:** Accepted
* **Date:** 2026-11-01
* **Requirements:** AC-03 (latest location), AC-04 (nearest within radius, tie-break), AC-07 (exclusions)
* **Related:** [ADR-002 concurrency](ADR-002-concurrency.md) (the pick is confirmed atomically), [ADR-008 data stores](ADR-008-data-stores.md)

## Context

A ride must be offered to the **nearest `AVAILABLE` driver within a configurable radius** (default 2 km). Ties go to the lexicographically smallest `driverId`. Drivers who rejected or timed out on a ride are never offered that ride again. Matching must use the **latest known location**, and must stay fast as the fleet grows (1k → 1M drivers, up to 250k location updates/s; see [scalability.md](../scalability.md)).

## Part 1: matching algorithm

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Greedy nearest (per request)** | Exactly what AC-04 asks; simple; low latency; easy to test | Not globally optimal; hot zones cause conflicts | ✅ **Chosen** |
| Batch assignment (min-cost matching per area every few hundred ms) | Better total pickup distance; fewer conflicts | Adds latency and complexity; not what the ACs specify | Future (S5 in [scalability](../scalability.md)) |

**Distance:** Haversine (great-circle), as the challenge suggests. It is exact enough at a 2 km radius.

## Part 2: spatial index

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Redis GEO** (`GEOADD`, `GEOSEARCH … ASC`) | In memory, sub-ms queries; location writes never touch the reservation rows; disposable and rebuildable; a team constraint (T5) | Eventually consistent (fed by events) | ✅ **Chosen** |
| PostGIS (`ST_DWithin` + KNN index) | Always fresh; same database | Every location update churns the same Postgres that decides reservations | Strong runner-up |

**Why Redis GEO:** the location stream is the dominant write load. Keeping it out of the transactional database means it cannot slow the `UPDATE … WHERE status='AVAILABLE'` that guarantees correctness. If the index is stale, the reservation simply fails and the next driver is tried.

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
   It drops excluded and non-`AVAILABLE` drivers, computes Haversine from exact coordinates, cuts at the radius, sorts by `(distance, driverId)` and takes the first.
4. `MATCH_RADIUS_M` (default `2000`) configures the radius.
5. **Redis is only a hint.** driver-service confirms the pick atomically ([ADR-002](ADR-002-concurrency.md)). A stale candidate costs one retry, never a wrong assignment.

## Consequences

* ✅ Query cost depends on drivers inside the radius, not on fleet size.
* ✅ Exclusions are permanent per ride (`ride_exclusions`), as AC-07 requires.
* ✅ Matching rules are fully unit-tested ([match_test.go](../../services/dispatch/internal/domain/match_test.go)); Lua versioning is integration-tested.
* ⚠️ After a Redis loss, matching pauses until the index is rebuilt (automatic on dispatch start, or `make rebuild-geo`).

## When to revisit

* One GEO key above ~100k members: shard by city or H3 cell.
* Reservation conflicts above ~20 % in an area: batch matching.
