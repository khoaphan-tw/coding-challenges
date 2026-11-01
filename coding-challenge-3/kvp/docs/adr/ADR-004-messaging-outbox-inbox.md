# ADR-004: Reliable messaging: transactional outbox + inbox, at-least-once with idempotent consumers

* **Status:** Accepted
* **Date:** 2026-11-01
* **Requirements:** AC-08 (idempotency), AC-10 (event-driven dispatch; why async, duplicates, failures)
* **Related:** [ADR-009 message broker](ADR-009-message-broker.md) (which broker), [ADR-003 saga](ADR-003-saga-orchestration.md)

## Context

Every saga step changes Postgres **and** sends messages through Redpanda. These are two systems with no shared transaction:

* commit, then publish: a crash in between **loses** the message (the saga stalls);
* publish, then commit: a crash in between announces something that **never happened**.

Brokers redeliver after crashes and rebalances, so consumers will see **duplicates**. The challenge asks what happens when a message is delivered twice and when processing fails.

## Why asynchronous?

`POST /rides` returns at once, while matching may take several offer rounds and minutes. Spikes queue in the broker instead of hitting every service; a service can restart without losing work; and dispatch scales out by partition. The cost is eventual consistency and duplicates, which the rest of this ADR handles.

## Part 1: delivery guarantee

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **At-least-once + idempotent consumers** | No loss; simple; works with any broker | Duplicates must be handled (they are) | ✅ **Chosen** |
| Kafka "exactly-once" transactions | No duplicates inside Kafka | Does not cover the Postgres write, so consumers still need dedup | ❌ |

## Part 2: publishing database changes (the dual-write problem)

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Transactional outbox + polling relay** | Messages commit with the state change; no extra infrastructure | ~50 ms polling latency; one relay per service | ✅ **Chosen** |
| Outbox + CDC (Debezium) | Reads the WAL; lower latency at high volume | Kafka Connect cluster to run | Future, at volume |

## Part 3: duplicates on the consumer side

**Inbox table** `inbox(consumer, event_id) PRIMARY KEY`, inserted in the handler's own transaction: a duplicate conflicts and the handler is skipped. It is exact and survives restarts because it commits together with the side effect (a Redis `SETNX` could not). Handlers are also naturally idempotent as a second guard.

## Decision

1. **Outbox:** every state change and its messages are committed in one Postgres transaction. An in-process relay polls every 50 ms, produces with franz-go (idempotent producer, `acks=all`) and marks rows published. A crash after produce re-publishes the **same `eventId`**.
2. **Inbox:** each handler's transaction starts with `INSERT INTO inbox … ON CONFLICT DO NOTHING`. 0 rows means a duplicate: log it, skip it, commit the offset.
3. **Offsets** are committed only after the DB transaction commits.
4. **Domain idempotency** as a second guard: saga PK = `rideId`, state machines treat repeats as no-ops, commands carry `rideId`/`offerId`, the GEO projection is versioned.
5. **Ordering:** partition key = aggregate id (`rideId` / `driverId`); the relay publishes in `outbox.id` order.
6. **Failures:** retry in process with exponential backoff and jitter (100 ms → 5 s, 5 attempts). Then the message goes to `<topic>.dlq` with error, consumer and attempt count in headers, plus an `ERROR` log with `ride_id`/`event_id`. The partition continues. Replay with `make replay-dlq TOPIC=…`.
7. **Trace context** (W3C `traceparent`) is stored in `outbox.headers` and copied to Kafka headers.

**What happens if a message is delivered twice?** The inbox insert conflicts and the handler is skipped. If it arrives under a new `eventId` (a re-sent command), the domain guards make it a no-op.

**What happens if processing fails?** The transaction rolls back (no partial state, no outgoing messages) and the offset is not committed, so the message is retried. After 5 attempts it goes to the DLQ; the saga resumer re-sends stuck commands after 30 s.

## Consequences

* ✅ No lost or phantom events, wherever a process crashes.
* ✅ Tested: duplicate delivery in every service's integration tests; crash recovery by killing dispatch-service mid-flow.
* ⚠️ Outbox and inbox tables grow; cleanup jobs are not implemented yet.

## When to revisit

* Outbox write volume (location events at 100k+ drivers): CDC.
* Several relay replicas per service: relay leader election (advisory lock) or CDC, to keep per-key order.
