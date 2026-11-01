# ADR-006: Architecture style: three microservices split by aggregate (database per service)

* **Status:** Accepted
* **Date:** 2026-11-01
* **Requirements:** challenge §2 ("a modular monolith is completely acceptable … boundaries could evolve into separate services"); team constraint T1 (microservices + saga)
* **Related:** [ADR-003 saga](ADR-003-saga-orchestration.md), [architecture.md](../architecture.md)

## Context

The challenge accepts a modular monolith and timeboxes the work to 4–6 hours. The root README warns against "over-engineering beyond scope". The team still chose **microservices + saga** as a deliberate learning goal. This ADR records why that is defensible, what it costs, and how small we kept it.

## Decision drivers

* Learning goal: practise sagas, idempotent messaging and distributed tracing for real.
* Boundaries should follow the domain, not the technical layers.
* Keep the number of moving parts as low as the goal allows.
* Every AC must still be met; scoring rewards ACs over architecture.

## Part 1: architecture style

| Option | Consistency | Ops cost | Independent scaling | Learning value for this challenge | Fit with 4–6 h | Verdict |
|---|---|---|---|---|---|---|
| Layered monolith (controllers/services/repos) | One transaction | Lowest | None | Low | Best | ❌ Boundaries not visible |
| **Modular monolith** (modules per bounded context, in-process bus, one DB with schemas per module) | One transaction (or outbox per module) | Low | None (scale the whole app) | Medium | Best | The recommended default; documented alternative |
| **Microservices (3 services, DB per service, broker)** | Eventual between services (saga) | Medium–high (broker, 3 DBs, tracing) | Per service | **High** (team goal T1) | Over the timebox | ✅ **Chosen** |
| Serverless / FaaS (AWS Lambda, Cloud Functions, Knative) | Eventual | Low ops, but not locally runnable as-is | Per function | Medium | Medium | ❌ Must run offline in Docker; cold starts; vendor coupling |
| Actor model (Akka/Pekko, Orleans, Proto.Actor; a grain per driver/ride) | Single writer per actor | Medium (cluster membership) | Per actor | High, but a different paradigm | Poor | ❌ Elegant for "one driver, one offer", but unfamiliar to the team; Go actor frameworks are niche |
| Cell-based (cell per city, each a full stack) | Per cell | High | Per cell | — | Poor | Future scale pattern ([scalability](../scalability.md)) |

**Honest assessment:** for 1,000 drivers, a modular monolith is simpler, immediately consistent and enough. Microservices pay off when the parts scale or change independently. Here that is real for the location-heavy driver path (25k–250k updates/s at 100k–1M drivers), but not at the size the challenge implements. We accept the cost to meet T1 and limit the damage with only **three** services, one shared bootstrap package, and one Compose file.

## Part 2: where to cut (granularity)

| Option | Services | Pros | Cons | Verdict |
|---|---|---|---|---|
| 2 services: ride+dispatch, driver | Fewer hops | Dispatch logic mixed with the ride lifecycle; can't scale matching separately | ❌ |
| **3 services: ride, driver, dispatch** | Each owns one aggregate family with a different change rate | Ride lifecycle, driver availability and location, and matching/offers each have one owner; maps to the challenge's components | ✅ **Chosen** |
| 5+ services: + location-service, offer-service, notification-service, passenger-service | Finest | Location could scale alone | Chatty; more sagas; more infra than the timebox allows | ❌ For now; location-service is the first split at scale (S1) |

| Service | Owns | Why separate |
|---|---|---|
| **ride-service** | Ride, passenger, Idempotency-Key, ride state machine, status history | Lifecycle authority; low write rate; passenger-facing |
| **driver-service** | Driver status + location, driver state machine, atomic reservation | Highest write rate (location); driver-facing; correctness point for AC-09 |
| **dispatch-service** | Saga, offers, exclusions, timeouts, matching, GEO projection | CPU and coordination; partitioned by ride; changes most often |

## Part 3: data ownership and integration

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| Shared database, all services read/write all tables | Easy joins | Hidden coupling; one schema change breaks three services; no clear owner | ❌ |
| **Database per service** (3 databases, 3 users, one Postgres server locally) | Clear ownership; services can move to separate clusters later | No cross-service joins; data duplicated as projections (GEO index) | ✅ **Chosen** |
| Schema per service in one database | Cheaper | Easy to cheat with cross-schema queries | ❌ (database-level users enforce the boundary) |

Integration is **asynchronous only** between services (commands, replies and events on Redpanda, [ADR-004](ADR-004-messaging-outbox-inbox.md)). The one synchronous internal call is dispatch-service's `GET /internal/drivers`, used to rebuild the GEO index on startup.

Clients reach services through one **API gateway** (nginx, [ADR-010](ADR-010-runtime-gateway-tooling.md)) that routes by path. Clients never know there are three services.

## Consequences

* ✅ Demonstrates the patterns the team wanted: saga, outbox/inbox, idempotent consumers, tracing across services.
* ✅ The driver/location path can scale independently (the main scaling need, [scalability](../scalability.md)).
* ⚠️ Eventual consistency between ride, driver and offer (milliseconds); the API documents it.
* ⚠️ More infrastructure (broker, tracing, three schemas), so local start-up takes ~30 s and needs ~4–6 GB RAM.
* ⚠️ Well over the 4–6 h timebox; the root README's "over-engineering" warning applies, and this ADR is our justification.

## When to revisit

* If the system stays small (< ~10–50k drivers) and one team owns it: **fold back into a modular monolith**. The `internal/` packages already are the modules; the saga would become a local transaction plus an in-process event bus.
* At 100k+ drivers: split **location ingestion** into its own service first (S1).
