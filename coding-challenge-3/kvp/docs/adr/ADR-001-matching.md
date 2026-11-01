# ADR-001: Matching — nearest available driver via Redis GEO + Haversine

* **Status:** Accepted
* **Date:** 2026-11-01
* **Context ACs:** AC-03, AC-04, AC-07

## Context

We must select the **nearest `AVAILABLE` driver within a radius** (default 2 km). Ties go to the lexicographically smallest `driverId`. Drivers who rejected or timed out on a ride are excluded for that ride. Matching must use the latest known location.

Options considered:

| Option | Pros | Cons |
|---|---|---|
| A. `SELECT … ORDER BY haversine(...)` over `drivers` in Postgres | Simple, always fresh | Full scan per request; contends with 25k+ location writes/s at scale |
| B. PostGIS + GiST index | Correct, indexed, fresh | Extra extension; the driver DB still takes all location writes *and* reads |
| **C. Redis GEO index as a projection, Go Haversine for the final ordering** | `GEOSEARCH` is O(N+log M) in memory; keeps location reads off the transactional DB; Redis is required by T5 | Eventually consistent (ms); must be rebuildable |
| D. In-memory grid inside dispatch | Fastest | Lost on restart; hard to share between replicas |

## Decision

**Option C.**

1. dispatch-service consumes `driver.events.v1` and maintains:
   * `geo:drivers:available`: a GEO set containing **only** `AVAILABLE` drivers.
   * `driver:{id}` hash `{status, lat, lng, version}`. A Lua script applies an event only if `event.version > stored.version`, so out-of-order and duplicate events are harmless.
2. For a ride with pickup `p`:
   ```
   GEOSEARCH geo:drivers:available FROMLONLAT p.lng p.lat BYRADIUS <radius_m> m ASC WITHCOORD COUNT <20 + |excluded| + |round_skips|>
   ```
3. A **pure Go function** makes the final choice, so it is unit-testable without Redis:
   ```go
   // match.SelectNearest returns the best candidate or ok=false.
   func SelectNearest(pickup geo.Point, cands []Candidate, radiusM float64,
                      excluded map[string]struct{}) (Candidate, bool)
   ```
   * drop excluded drivers (`ride_exclusions` + this round's `round_skips`)
   * compute Haversine distance from exact coordinates (earth radius 6371.0088 km)
   * drop candidates with distance > radius (Redis GEO hashes coordinates to about 0.6 m, so we re-check)
   * sort by `(distance, driverId)` and take the first
   * if the `COUNT` limit cut the list at a tie, repeat with a larger `COUNT`
4. Radius is configurable: `MATCH_RADIUS_M` (default `2000`).
5. Redis is **only a hint**. The pick is confirmed by the atomic reservation in driver-service ([ADR-002](ADR-002-concurrency.md)). A stale candidate costs one extra round-trip; it can never cause a wrong assignment.

## Consequences

* ✅ Matching cost does not depend on total driver count, only on the drivers inside the radius.
* ✅ Exclusions are permanent per ride (stored in `dispatch_db`), as AC-07 requires.
* ⚠️ A driver who just became `AVAILABLE` may be missed for a few ms (projection lag). Acceptable.
* ⚠️ Redis loss means no matches until rebuilt (`make rebuild-geo`). Matching never produces a wrong result in that case; it only pauses.
* 🔁 Revisit when one GEO key gets too hot (≫100k members, or ≫50k ops/s). Then shard keys by city / H3 cell ([scalability.md](../scalability.md)).

## Tests

`internal/match/select_test.go`: nearest selected · outside radius excluded · offline excluded · busy/offered excluded (not in the GEO set, and filtered by status if present) · no driver available · equidistant → smallest `driverId` · rejected driver excluded.
