# ADR-002: Concurrency: atomic conditional updates in PostgreSQL, no distributed locks

* **Status:** Accepted
* **Date:** 2026-11-01
* **Requirements:** AC-02, AC-05, AC-09 ("a simple database-based approach is sufficient")
* **Related:** [ADR-001 matching](ADR-001-matching.md), [ADR-003 saga](ADR-003-saga-orchestration.md), [ADR-008 data stores](ADR-008-data-stores.md)

## Context: the races

Matching reads a projection (Redis) and then writes to the driver. In between, another saga can pick the same driver:

```
Saga R1: GEOSEARCH → D001 ┐
Saga R2: GEOSEARCH → D001 ┼─> both send ReserveDriver(D001)
```

"Read status, check `AVAILABLE`, write `OFFERED`" lets both succeed (a lost update), so D001 would hold two offers. The same check-then-act race exists for:

| Race | Contenders |
|---|---|
| Two rides → one driver | two dispatch sagas (AC-09) |
| One offer | driver accept vs. driver reject vs. 30 s timeout vs. passenger cancel |
| One ride | cancel vs. assign vs. accept |
| Duplicate HTTP calls | double-click accept, retry after a timeout |

## Options compared

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Atomic conditional `UPDATE … WHERE status = 'AVAILABLE'`** | One statement; the row count is the answer; correct across replicas and crashes; no extra infrastructure | The driver row is a serialisation point | ✅ **Chosen** |
| Distributed lock (Redis `SET NX` / Redlock) | Familiar | A lease can expire during a pause, so two holders are possible unless the write re-checks in the database anyway | ❌ |

`SELECT … FOR UPDATE` is used too, where the domain rule runs in Go before the write (rides, sagas).

## Decision

| Race | Statement (owner service) | The loser sees |
|---|---|---|
| Two rides → one driver | `UPDATE drivers SET status='OFFERED', current_ride_id=$ride, version=version+1 WHERE id=$d AND status='AVAILABLE'` (driver-service) | 0 rows → `DriverReservationFailed` → saga tries the next candidate |
| Accept vs reject vs timeout | `UPDATE offers SET status=$new … WHERE id=$o AND driver_id=$d AND status='PENDING' [AND expires_at > now()]` (dispatch) | 0 rows → re-read: same result → `200` (idempotent), else `409` |
| Ride transitions | `SELECT … FROM rides WHERE id=$r FOR UPDATE`, run the state machine, `UPDATE … version=version+1` (ride-service) | Waits for the lock, then sees the new status; a repeat is a no-op, an invalid one is rejected |
| Saga steps | `SELECT … FROM dispatch_sagas WHERE ride_id=$r FOR UPDATE` before touching offers (dispatch) | Waits; **lock order is always saga → offer**, so no deadlocks |
| Late release / busy | `… WHERE id=$d AND current_ride_id=$ride AND status IN (…)` | 0 rows → no-op (a stale command never frees a driver who moved on) |

**Defence in depth:**

* `UNIQUE (driver_id) WHERE status='PENDING'` and `UNIQUE (ride_id) WHERE status='PENDING'` on `offers`.
* `CHECK ((status IN ('OFFERED','BUSY')) = (current_ride_id IS NOT NULL))` on `drivers`.
* `driver.commands.v1` is keyed by `driverId`, so commands for one driver are usually handled in order, which lowers contention. Correctness does not depend on it.

## Consequences

* ✅ No lock service; correct with any number of replicas and after crashes.
* ✅ Proven by tests:
  * [postgres_integration_test.go](../../services/driver/internal/adapter/postgres/postgres_integration_test.go): 50 rounds of two goroutines behind a barrier on real Postgres; exactly one wins.
  * [orchestrator_integration_test.go](../../services/dispatch/internal/usecase/orchestrator_integration_test.go): two sagas, one driver; accept racing the timeout.
  * e2e `TestConcurrentRidesCannotShareADriver`: two rides through the gateway.
* ⚠️ Popular drivers (airport) cause wasted reservation attempts (B3 in [scalability](../scalability.md)).

## When to revisit

* Reservation conflict rate above ~20 % in an area: area-owned dispatchers (single writer per area).
