# ADR-008: Data stores: PostgreSQL as the source of truth, Redis as a disposable GEO index

* **Status:** Accepted
* **Date:** 2026-11-01
* **Requirements:** team constraint T5 (PostgreSQL + Redis GEO + Kafka/Redpanda); AC-06 (persisted state), AC-08, AC-09 ("database-based approach")
* **Related:** [ADR-001 matching](ADR-001-matching.md), [ADR-002 concurrency](ADR-002-concurrency.md), [ADR-006 architecture](ADR-006-service-architecture.md)

## Context

The system needs:

1. a **transactional store** for rides, drivers, offers, sagas, idempotency keys, outbox and inbox, with row locks and conditional updates;
2. a **fast spatial index** for "nearest available drivers".

T5 names PostgreSQL and Redis. This ADR checks that they are the right fit and records the alternatives.

## Decision drivers

* ACID transactions with row-level locking, `FOR UPDATE SKIP LOCKED`, partial unique indexes, `CHECK` constraints. These are the correctness tools of [ADR-002](ADR-002-concurrency.md).
* Outbox and inbox in the same transaction as business writes.
* Small footprint in Docker; one command to start.
* A credible path to 1M drivers.

## Part 1: transactional store

| Capability needed | **PostgreSQL 18** | MySQL 8 / MariaDB | MongoDB 7 | CockroachDB / YugabyteDB | DynamoDB | Cassandra / ScyllaDB |
|---|---|---|---|---|---|---|
| Multi-row ACID transactions | ✅ | ✅ (InnoDB) | ✅ (replica set; more overhead) | ✅ (distributed, serializable) | ✅ (`TransactWriteItems`, ≤100 items) | ❌ (LWT per partition only) |
| Conditional update returning the row | ✅ `UPDATE … WHERE … RETURNING` | ⚠️ no `UPDATE … RETURNING`: needs affected-rows + a second `SELECT` | ✅ `findOneAndUpdate` | ✅ | ✅ `ConditionExpression` | ⚠️ LWT `IF`, slow |
| `FOR UPDATE SKIP LOCKED` (outbox relay, sweepers) | ✅ | ✅ (8.0+) | ❌ | ⚠️ partial | ❌ | ❌ |
| Partial unique index (`WHERE status='PENDING'`) | ✅ | ❌ (workaround: generated columns) | ✅ (partial index) | ✅ | ❌ | ❌ |
| Arrays, `jsonb`, `CHECK` constraints | ✅ | ⚠️ JSON, checks since 8.0.16 | ✅ documents | ✅ | ⚠️ | ⚠️ |
| Geo if needed later | ✅ PostGIS | ⚠️ basic spatial | ✅ 2dsphere | ⚠️ spatial (Cockroach) | ❌ | ❌ |
| Runs locally in one small container | ✅ ~50 MB RAM | ✅ | ✅ (heavier) | ⚠️ heavier, multi-node to make sense | ⚠️ DynamoDB Local (JVM) | ⚠️ JVM, heavy |
| Horizontal write scaling | ⚠️ manual (partitioning, Citus, sharding by city) | ⚠️ Vitess | ✅ sharding | ✅ native | ✅ native | ✅ native |
| Verdict | ✅ **Chosen** | Viable, fewer tools | ❌ weaker for relational invariants | Future option at global scale | ❌ cloud-only, item limits | ❌ wrong consistency model |

**Why PostgreSQL:** every correctness mechanism we rely on is a first-class Postgres feature: the atomic conditional update, `SKIP LOCKED` workers, partial unique indexes, `CHECK (status IN (...))`, `jsonb` outbox payloads and `text[]` columns. Horizontal write scaling is its weak spot. At 1M drivers we shard by city (driver rows stay single-shard) or move to a distributed SQL database. CockroachDB and YugabyteDB speak the Postgres wire protocol, so pgx and most SQL carry over (see [scalability](../scalability.md)).

### Deployment shape

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **One Postgres server, three databases, three users** | Cheap on a laptop; real ownership boundary (a service cannot read another's tables) | Shared CPU/RAM and failure domain | ✅ **Chosen** (local) |
| Three Postgres containers | Production-like isolation | ~3× memory, slower start | Production shape |
| One database, three schemas | Simplest | Too easy to cross boundaries | ❌ |

## Part 2: spatial index / cache

| Option | GEO commands | Persistence needed? | License / ecosystem | Footprint | Verdict |
|---|---|---|---|---|---|
| **Redis 8** | ✅ `GEOADD`, `GEOSEARCH`, Lua scripting | No (index is rebuildable; AOF/RDB off) | Redis 8 is AGPLv3/RSALv2/SSPLv1 (open source again since 8.0); largest ecosystem | ~5 MB RAM idle | ✅ **Chosen** (T5) |
| Valkey 8 (Linux Foundation fork of Redis 7.2) | ✅ same commands | No | BSD-3; drop-in replacement | Same | Drop-in alternative if licensing matters |
| Dragonfly | ✅ mostly compatible, multi-threaded | No | BSL 1.1 | Higher throughput per node | Alternative at very high load |
| KeyDB | ✅ multi-threaded fork | No | BSD | — | ❌ Slower development |
| Memcached | ❌ no GEO | — | BSD | — | ❌ |
| Tile38 / PostGIS / Elasticsearch | Spatial | — | — | — | See [ADR-001](ADR-001-matching.md) |

Redis is used **only** as a read model (the GEO index of `AVAILABLE` drivers plus a versioned hash per driver). It is never the source of truth, so it runs with persistence disabled. Losing it pauses matching until the rebuild; it never loses data.

### Where idempotency keys live

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Postgres table, same transaction as the ride** | Key and ride are atomic: no "key saved, ride lost" or the reverse; concurrent duplicates serialize on the primary key | One more row per request | ✅ **Chosen** |
| Redis `SET NX` with TTL | Fast, auto-expiry | Not atomic with the ride insert; a crash between them breaks idempotency | ❌ |
| In-memory map | Trivial | Lost on restart; not shared across replicas | ❌ |

## Decision

* **PostgreSQL 18** (`postgres:18-alpine`): `ride_db`, `driver_db`, `dispatch_db`, each with its own user; schema migrations embedded per service.
* **Redis 8** (`redis:8-alpine`), no persistence: GEO index owned by dispatch-service, rebuilt from driver-service when empty.

## Consequences

* ✅ All invariants are enforced by the database (constraints, conditional updates), not only by code.
* ✅ Redis can be wiped at any time (`make rebuild-geo`).
* ⚠️ Postgres write scaling must be designed explicitly at 100k+ drivers (location fast path, sharding by city).
* ⚠️ Missing cleanup jobs for `inbox`, published `outbox` rows and old `idempotency_keys`.

## When to revisit

* Location writes beyond one primary: move location off Postgres (S1); shard `driver_db` by city.
* Multi-region active-active: CockroachDB or YugabyteDB (Postgres-compatible).
* Redis licensing concerns: switch to **Valkey** (same commands, no code change).
