# ADR-006: Architecture style: three microservices split by aggregate (database per service)

* **Status:** Accepted
* **Date:** 2026-11-01
* **Requirements:** challenge §2 ("a modular monolith is completely acceptable … boundaries could evolve into separate services"); team constraint T1 (microservices + saga)
* **Related:** [ADR-003 saga](ADR-003-saga-orchestration.md), [architecture.md](../architecture.md)

## Context

The challenge accepts a modular monolith and timeboxes the work to 4–6 hours. The team still chose **microservices + saga** as a deliberate learning goal (T1). This ADR records why that is defensible and how small we kept it.

## Part 1: architecture style

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Microservices (3 services, database per service, broker)** | Independent scaling per service; practises sagas, idempotent messaging and distributed tracing (team goal T1) | Eventual consistency between services; more infrastructure | ✅ **Chosen** |
| Modular monolith (modules per bounded context, one database) | One transaction; simplest to run; enough for 1,000 drivers | No independent scaling; does not meet T1 | The recommended default without T1 |

For 1,000 drivers a modular monolith would be enough. Microservices pay off when parts scale differently, which is real for the location-heavy driver path at 100k+ drivers. We keep the cost low with only **three** services, one shared bootstrap package and one Compose file.

## Part 2: where to cut

**Three services, one per aggregate family.** Each part changes and scales at a different rate, and the split maps directly to the challenge's components:

| Service | Owns | Why separate |
|---|---|---|
| **ride-service** | Ride, passenger, Idempotency-Key, ride state machine, status history | Lifecycle authority; low write rate; passenger-facing |
| **driver-service** | Driver status + location, driver state machine, atomic reservation | Highest write rate (location); driver-facing; correctness point for AC-09 |
| **dispatch-service** | Saga, offers, exclusions, timeouts, matching, GEO projection | CPU and coordination; partitioned by ride; changes most often |

## Part 3: data ownership and integration

**Database per service** (3 databases and 3 users on one Postgres server locally). Each service writes only its own data, and database users enforce the boundary, so a service can move to its own cluster later without code changes.

Integration is **asynchronous only** between services (commands, replies and events on Redpanda, [ADR-004](ADR-004-messaging-outbox-inbox.md)). The one synchronous internal call is dispatch-service's `GET /internal/drivers`, used to rebuild the GEO index on startup.

Clients reach services through one **API gateway** (nginx, [deploy/nginx.conf](../../deploy/nginx.conf); setup in [local-development.md](../local-development.md)) that routes REST by path (`:8080`) and gRPC by service name (`:9090`). Clients never know there are three services.

## Consequences

* ✅ Demonstrates the patterns the team wanted: saga, outbox/inbox, idempotent consumers, tracing across services.
* ✅ The driver/location path can scale independently (the main scaling need, [scalability](../scalability.md)).
* ⚠️ Well over the 4–6 h timebox, with more infrastructure to run (broker, tracing, three databases).

## When to revisit

* If the system stays small (< ~10–50k drivers): fold back into a modular monolith. The `internal/` packages already are the modules.
* At 100k+ drivers: split **location ingestion** into its own service first (S1).
