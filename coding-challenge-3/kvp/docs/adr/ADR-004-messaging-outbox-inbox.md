# ADR-004: Redpanda + transactional outbox/inbox (at-least-once, idempotent consumers)

* **Status:** Accepted
* **Date:** 2026-11-01
* **Context:** AC-08, AC-10; team constraint T5

## Why asynchronous

* `POST /rides` returns as soon as the ride is stored. Matching may take several offer rounds and up to minutes; the HTTP request must not wait for it.
* Load spikes (rush hour) queue up in Redpanda instead of overloading dispatch.
* Each service can be down without losing work, because messages wait in the log.
* Dispatch workers scale out by partition.

## The dual-write problem

"Commit to Postgres, then publish to Kafka" loses the event if the process dies between the two. "Publish, then commit" announces something that never happened. Postgres and Kafka do not share a transaction.

## Decision

1. **Transactional outbox.** A state change and the messages it causes are written in the **same** Postgres transaction (`outbox` table). An in-process **relay** goroutine polls every 50 ms:
   `SELECT … FROM outbox WHERE published_at IS NULL ORDER BY id LIMIT 500 FOR UPDATE SKIP LOCKED` → produce (idempotent producer, `acks=all`) → `UPDATE … SET published_at = now()`.
   A crash after produce and before update re-publishes with the **same `eventId`**.
2. **Inbox (idempotent consumer).** Each handler runs in a transaction that starts with
   `INSERT INTO inbox(consumer, event_id) VALUES (…) ON CONFLICT DO NOTHING`. If 0 rows were inserted, the message was already handled: log it, skip it, commit the offset.
3. **Offsets are committed after the DB transaction commits.** This gives at-least-once delivery, and the inbox makes processing effectively exactly-once.
4. **Domain-level idempotency** stays as a second guard: saga PK = `rideId`, state machines treat a repeat as a no-op, and commands are guarded by `rideId`.
5. **Ordering:** partition key = aggregate id (`rideId` / `driverId`). The relay publishes in `outbox.id` order per key.
6. **Failures:** a handler error triggers in-process retry with exponential backoff and jitter (100 ms … 5 s, 5 attempts). After that, the message goes to `<topic>.dlq` with error, stack and attempts in headers, and an error log with `ride_id`/`event_id`. The partition then continues, so one poison message does not block every ride.
7. **Broker:** Redpanda v25 (Kafka API). The Go client is `franz-go`. Redpanda Console runs at `:8088` to inspect topics.

## What happens if a message is delivered twice?

The inbox insert conflicts and the handler skips it. If it arrives under a new `eventId` (re-sent command), the domain guard makes it a no-op (`saga` PK, state machine "already in target state", `current_ride_id` guard).

## What happens if processing fails?

The DB transaction rolls back, so no partial state exists and no outbox rows are written. The offset is not committed and the message is retried. Persistent failures go to the DLQ and an alert fires on DLQ rate. The saga resumer re-drives stuck sagas after the fix.

## Consequences

* ✅ No lost or phantom events; consumers can crash anywhere.
* ⚠️ Two extra rows per change; `inbox` needs a 7-day cleanup job.
* ⚠️ Polling relay latency is ~50 ms. Move to CDC (Debezium / Redpanda Connect on the WAL) at high volume.
