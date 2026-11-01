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

## Why asynchronous at all?

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| Synchronous HTTP/gRPC chain (`POST /rides` → dispatch → driver → …) | Simple to trace; immediate answer | Request held open across several offer rounds (up to minutes); one slow service blocks all; spikes hit every service at once; no retry buffer | ❌ |
| **Asynchronous messaging** | `POST /rides` returns at once; spikes queue in the broker; services can be down without losing work; scale out by partition | Eventual consistency; duplicates; harder debugging (solved with tracing) | ✅ **Chosen** (AC-10) |

## Part 1: delivery guarantee

| Guarantee | How | Pros | Cons | Verdict |
|---|---|---|---|---|
| At-most-once | Commit offset before processing | No duplicates | Loses messages on crash | ❌ |
| **At-least-once + idempotent consumers** | Commit offset after the handler's DB transaction commits; dedup on `event_id` | No loss; simple; works with any broker | Duplicates must be handled (they are) | ✅ **Chosen** |
| "Exactly-once" (Kafka transactions, `read_committed`) | Producer transactions + consumer offsets in the same Kafka transaction | No duplicates *within Kafka* | Does not cover the Postgres write; the handler's DB transaction is still separate, so dedup is needed anyway | ❌ Doesn't solve our case |

## Part 2: getting DB changes onto the broker (dual-write problem)

| Option | How | Latency | Extra infra | Ordering | Verdict |
|---|---|---|---|---|---|
| Dual write | `db.Commit()` then `producer.Send()` | Lowest | None | — | ❌ Loses or invents events |
| **Transactional outbox + polling relay** | Insert into `outbox` in the same transaction; a goroutine polls `FOR UPDATE SKIP LOCKED`, produces, marks published | ~50 ms | None | By `outbox.id` | ✅ **Chosen** |
| Outbox + CDC: Debezium (Kafka Connect) | Read the Postgres WAL, publish outbox rows (outbox event router) | ~10–100 ms | Kafka Connect + Debezium (JVM) | WAL order | Future, at volume |
| Outbox + Redpanda Connect (Benthos) `postgres_cdc` | WAL → topic, single Go binary | Low | One small container | WAL order | Future, lighter than Debezium |
| Postgres `LISTEN/NOTIFY` | Notify the relay on insert | Lower than polling | None | — | ❌ Notifications are lost while the relay is down; still needs polling as a fallback |
| Event sourcing (events are the state) | Append-only log is the truth | — | Event store | Native | ❌ Paradigm change |
| Libraries: Watermill (Go) forwarder, Eventuate Tram (Java), MassTransit outbox (.NET), NServiceBus outbox | Library-provided outbox/inbox | ~polling | Library | Library | Watermill considered (see below) |

**Why not Watermill** (the most popular Go messaging library, with an SQL outbox "forwarder")? It would save ~150 lines, but it adds its own message model and router abstractions on top of franz-go. Our relay and inbox are small, explicit and easy to explain. A library is worth it when there are many message types and middlewares; we have one envelope and one handler per topic.

## Part 3: duplicates on the consumer side

| Option | How | Verdict |
|---|---|---|
| **Inbox table** `inbox(consumer, event_id) PRIMARY KEY`, inserted in the handler's transaction | Duplicate insert conflicts, so the handler is skipped | ✅ **Chosen** (exact, transactional, survives restarts) |
| Redis `SETNX event_id` with TTL | Fast | ❌ Not in the same transaction as the side effect: a crash between them gives a lost or double effect |
| Naturally idempotent handlers only | Design every effect to be safe to repeat | ✅ **Also used** as a second guard (saga PK, state machines, `rideId` guards) |
| Broker-side dedup (SQS FIFO dedup window, NATS JetStream `Nats-Msg-Id`, Kafka idempotent producer) | Broker drops duplicate publishes | Partial: covers producer retries, not consumer redelivery; the idempotent producer is enabled anyway |

## Decision

1. **Outbox:** every state change and its messages are committed in one Postgres transaction. An in-process relay polls every 50 ms, produces with franz-go (idempotent producer, `acks=all`) and marks rows published. A crash after produce re-publishes the **same `eventId`**.
2. **Inbox:** each handler's transaction starts with `INSERT INTO inbox … ON CONFLICT DO NOTHING`. 0 rows means a duplicate: log it, skip it, commit the offset.
3. **Offsets** are committed only after the DB transaction commits.
4. **Domain idempotency** as a second guard: saga PK = `rideId`, state machines treat repeats as no-ops, commands carry `rideId`/`offerId`, the GEO projection is versioned.
5. **Ordering:** partition key = aggregate id (`rideId` / `driverId`); the relay publishes in `outbox.id` order (one relay per service instance).
6. **Failures:** retry in process with exponential backoff and jitter (100 ms → 5 s, 5 attempts). Then the message goes to `<topic>.dlq` with error, consumer and attempt count in headers, plus an `ERROR` log with `ride_id`/`event_id`. The partition continues. Replay with `make replay-dlq TOPIC=…`.
7. **Trace context** (W3C `traceparent`) is stored in `outbox.headers` and copied to Kafka headers.

**What happens if a message is delivered twice?** The inbox insert conflicts and the handler is skipped. If it arrives under a new `eventId` (a re-sent command), the domain guards make it a no-op.

**What happens if processing fails?** The transaction rolls back (no partial state, no outgoing messages) and the offset is not committed, so the message is retried. After 5 attempts it goes to the DLQ; the saga resumer re-sends stuck commands after 30 s.

## Consequences

* ✅ No lost or phantom events, regardless of where a process crashes.
* ✅ Tested: duplicate delivery in every service's integration tests; crash recovery by killing dispatch-service mid-flow.
* ⚠️ Two extra rows per change (outbox + inbox); both tables need cleanup jobs (not implemented).
* ⚠️ A transient outage longer than the retry budget (~10 s) sends messages to the DLQ, and they then need a replay.
* ⚠️ Per-key ordering assumes one relay per service. Replicas need relay leader election (advisory lock) or CDC.

## When to revisit

* Outbox write volume (location events at 100k+ drivers): CDC (Redpanda Connect or Debezium).
* Many message types and middlewares: Watermill.
* Moving to Temporal ([ADR-003](ADR-003-saga-orchestration.md)): the command/reply messaging becomes activity calls; the outbox stays for integration events.
