# ADR-008: Data stores: PostgreSQL as the source of truth, Redis as a disposable GEO index

* **Status:** Accepted
* **Date:** 2026-11-01
* **Requirements:** team constraint T5 (PostgreSQL + Redis GEO + Kafka/Redpanda); AC-06 (persisted state), AC-08, AC-09 ("database-based approach")
* **Related:** [ADR-001 matching](ADR-001-matching.md), [ADR-002 concurrency](ADR-002-concurrency.md), [ADR-006 architecture](ADR-006-service-architecture.md)

## Context

The system needs:

1. a **transactional store** for rides, drivers, offers, sagas, idempotency keys, outbox and inbox, with row locks and conditional updates;
2. a **fast spatial index** for "nearest available drivers".

T5 names PostgreSQL and Redis. This ADR records why they fit.

## Part 1: transactional store

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **PostgreSQL 18** | `UPDATE … WHERE … RETURNING`, `FOR UPDATE SKIP LOCKED`, partial unique indexes, `CHECK`, `jsonb`: every correctness tool of [ADR-002](ADR-002-concurrency.md) is first-class | Horizontal write scaling must be designed (sharding by city) | ✅ **Chosen** |
| MySQL 8 | Mature, familiar | No `UPDATE … RETURNING` and no partial unique indexes (needs workarounds) | Viable, fewer tools |

**Deployment shape:** one Postgres server locally with three databases and three users. Each service can only reach its own data, and it stays cheap on a laptop. Production would give each service its own cluster.

## Part 2: spatial index

**Redis 8** (team constraint T5), used **only** as a read model: the GEO index of `AVAILABLE` drivers plus a versioned hash per driver. It is never the source of truth, so it runs with persistence disabled. Losing it pauses matching until the rebuild; it never loses data. Why Redis GEO over PostGIS for matching: [ADR-001](ADR-001-matching.md).

## Part 3: where idempotency keys live

**In Postgres, in the same transaction as the ride.** The key and the ride are saved together or not at all, and concurrent duplicates wait on the primary key. A Redis `SET NX` would be faster but not atomic with the ride insert, so a crash between the two would break idempotency.

## Decision

* **PostgreSQL 18** (`postgres:18-alpine`): `ride_db`, `driver_db`, `dispatch_db`, each with its own user; schema migrations embedded per service.
* **Redis 8** (`redis:8-alpine`), no persistence: GEO index owned by dispatch-service, rebuilt from driver-service when empty.

## Consequences

* ✅ All invariants are enforced by the database (constraints, conditional updates), not only by code.
* ✅ Redis can be wiped at any time (`make rebuild-geo`).
* ⚠️ Postgres write scaling must be designed explicitly at 100k+ drivers (location fast path, sharding by city).

## When to revisit

* Location writes beyond one primary: move location off Postgres (S1); shard `driver_db` by city.
* Multi-region active-active: a Postgres-compatible distributed database (CockroachDB, YugabyteDB).
