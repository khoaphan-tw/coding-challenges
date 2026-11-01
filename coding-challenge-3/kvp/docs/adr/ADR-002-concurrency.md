# ADR-002: Concurrency — atomic conditional update on the driver row

* **Status:** Accepted
* **Date:** 2026-11-01
* **Context ACs:** AC-02, AC-05, AC-09

## The race

Matching reads from a projection (Redis), then writes to the driver. Between that read and the write, another saga can pick the same driver:

```
Saga R1: GEOSEARCH → D001 ┐
Saga R2: GEOSEARCH → D001 ┼─> both send ReserveDriver(D001)
```

A naive "read status, check AVAILABLE, then write OFFERED" lets both succeed. That is a lost update, and D001 would hold two offers. The same check-then-act race exists for:

* accept vs. timeout on one offer
* accept vs. reject (double click, two devices)
* cancel vs. assign on one ride

## Options

| Option | Verdict |
|---|---|
| Distributed lock (Redis `SET NX PX`) | Rejected. Lease expiry plus GC pauses still allow two holders unless every write is fenced, and fencing means a DB check anyway |
| `SELECT … FOR UPDATE` then `UPDATE` | Works, but holds a lock across a round-trip; more code |
| Optimistic concurrency (`version`) | Works; the caller must retry on conflict |
| **Atomic conditional update** | **Chosen.** One statement both checks and writes; Postgres row lock serialises it |

## Decision

The **owner of the state decides atomically, in one SQL statement**, and the number of affected rows is the answer.

| Race | Statement (owner) | Loser sees |
|---|---|---|
| Two rides → one driver | `UPDATE drivers SET status='OFFERED', current_ride_id=$ride, version=version+1 WHERE id=$d AND status='AVAILABLE'` (driver-service) | 0 rows → `DriverReservationFailed` → next candidate |
| Accept vs timeout vs reject | `UPDATE offers SET status=$new, responded_at=now() WHERE id=$o AND driver_id=$d AND status='PENDING' [AND expires_at > now()]` (dispatch) | 0 rows → re-read; same result → `200` (idempotent), else `409` |
| Concurrent ride transitions | `UPDATE rides SET … , version=version+1 WHERE id=$r AND version=$v` (ride-service) | 0 rows → reload, re-run state machine (repeat is a no-op) |
| Driver release/busy | `… WHERE id=$d AND current_ride_id=$ride AND status IN (…)` | 0 rows → no-op (stale command) |

**Defence in depth** (in case of a bug elsewhere):

* `CREATE UNIQUE INDEX one_pending_offer_per_driver ON offers(driver_id) WHERE status='PENDING'`
* `CREATE UNIQUE INDEX one_pending_offer_per_ride ON offers(ride_id) WHERE status='PENDING'`
* `driver.commands.v1` keyed by `driverId`: commands for one driver are consumed in order. This reduces contention but is **not** relied on for correctness, because a consumer-group rebalance can briefly run two consumers on one partition.

## Consequences

* ✅ No distributed lock service; correct across any number of replicas and after crashes.
* ✅ Simple to test: run the statements from two goroutines released by a barrier.
* ⚠️ Popular drivers (airport queue) cause wasted reservation attempts. Mitigation at scale: batch matching per geo cell ([scalability.md](../scalability.md)).
* ⚠️ The driver row is the serialization point, so `driver_db` must stay on one primary per shard.

## Tests

* `internal/driver/store_concurrency_test.go` (testcontainers Postgres): 50 iterations; two goroutines call `Reserve(D1, R1)` and `Reserve(D1, R2)` behind a `sync.WaitGroup` barrier. Assert exactly one success, the driver is `OFFERED` with one `current_ride_id`, and the loser gets `ErrNotAvailable`.
* `test/e2e/concurrency_test.go` (full compose stack): one available driver, two rides created in parallel. Assert exactly one ride reaches `DRIVER_ASSIGNED`, the other stays `MATCHING`, and the driver has one pending offer.
* `internal/dispatch/offer_race_test.go`: accept and timeout fire together. Exactly one wins.
