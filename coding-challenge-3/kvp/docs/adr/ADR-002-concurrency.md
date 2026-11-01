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

## Decision drivers

* **Correct across processes and replicas**, not just goroutines (`sync.Mutex` is useless across services).
* **Survives crashes** with no orphaned locks and no lease expiry problems.
* **Simple to explain and to test** (the challenge asks for the strategy to be documented and proven by a test).
* No extra infrastructure just for locking.

## Options compared

| Option | Mechanism | Correct under crash / GC pause? | Extra infra | Latency | Complexity | Verdict |
|---|---|---|---|---|---|---|
| In-process mutex / channel per driver | `sync.Mutex` map | Only inside one process | None | Lowest | Low | ❌ Breaks with >1 instance or service |
| **Atomic conditional `UPDATE … WHERE status = 'AVAILABLE'`** | Postgres row lock + `WHERE` re-check; affected rows = answer | ✅ Yes, the database is the arbiter | None | One statement | Low | ✅ **Chosen (primary guard)** |
| `SELECT … FOR UPDATE` then `UPDATE` | Pessimistic row lock in a transaction | ✅ Yes | None | Lock held across a round-trip | Low–medium | ✅ Used where the domain logic runs in Go first (rides, sagas) |
| Optimistic concurrency (`version` column, retry on conflict) | `UPDATE … WHERE version = $v` | ✅ Yes | None | Retries under contention | Medium (retry loops) | Considered; row locks are simpler here |
| `SERIALIZABLE` isolation | Postgres SSI aborts conflicting transactions | ✅ Yes | None | Aborts + retries | Medium; easy to misuse | ❌ Heavier than needed |
| Postgres advisory locks (`pg_advisory_xact_lock(hash(driverId))`) | Named lock in the database | ✅ (transaction-scoped) | None | Extra statement | Medium | ❌ Hides the rule outside the data; used only for migrations |
| Redis lock (`SET NX PX`) / Redlock | Lease in Redis | ⚠️ Lease can expire during a GC pause or network stall, giving two holders unless writes are fenced | Redis | Low | Medium–high | ❌ Fencing needs a DB check anyway |
| etcd / ZooKeeper / Consul leases | Consensus-backed lock + fencing token | ✅ With fencing | A consensus cluster | Higher | High | ❌ Overkill for one row |
| Single writer per partition (Kafka key = `driverId`, actor model: Akka/Orleans/Proto.Actor) | All commands for a driver handled serially by one consumer | ✅ While ownership is stable; during a rebalance two consumers can briefly overlap | Broker (have it) | Low | Medium | Partial: used to **reduce** contention, not relied on |
| Unique constraints | DB rejects a second "pending offer" row for a driver | ✅ Yes | None | One insert | Low | ✅ Used as **defence in depth** |

**Why conditional updates win:** the owner of the state decides atomically in one SQL statement. "Check and set" cannot be split, and no lock outlives a crashed process. This is the "simple database-based approach" the challenge suggests, and a common pattern for seat, stock and booking reservations.

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
* `driver.commands.v1` is keyed by `driverId`, so commands for one driver are usually handled serially, which lowers contention. Correctness does not depend on it.

## Consequences

* ✅ No lock service, correct with any number of replicas and after crashes.
* ✅ Proven by tests:
  * [postgres_integration_test.go](../../services/driver/internal/adapter/postgres/postgres_integration_test.go): 50 rounds of two goroutines behind a barrier on real Postgres; exactly one wins.
  * [orchestrator_integration_test.go](../../services/dispatch/internal/usecase/orchestrator_integration_test.go): two sagas, one driver; accept racing the timeout.
  * e2e `TestConcurrentRidesCannotShareADriver`: two rides through the gateway.
* ⚠️ Popular drivers (airport) cause wasted reservation attempts (B3). Mitigation at scale: area-affine dispatch or batch matching.
* ⚠️ The driver row is the serialisation point, so a driver's row lives on exactly one primary (shard by city at scale).

## When to revisit

* Reservation conflict rate above ~20 % in an area: area-owned dispatchers (single writer per area).
* Drivers sharded across databases: the conditional update stays within one shard; nothing changes as long as a driver lives in exactly one shard.
