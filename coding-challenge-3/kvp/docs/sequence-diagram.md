# Sequence Diagrams

All arrows between services go through Redpanda via each service's **outbox**. Diagrams draw them as direct messages to keep them readable. `[tx]` means "in one Postgres transaction, together with the inbox and outbox rows".

Services: **RS** = ride-service, **DS** = driver-service, **XS** = dispatch-service (saga orchestrator).

| # | Flow | Requirements |
|---|---|---|
| 1 | [Happy path: request → offer → accept → complete](#1-happy-path-request--offer--accept--complete) | AC-01, 02, 04, 05, 06, 10 |
| 2 | [Reject → rematch](#2-reject--rematch-ac-05-ac-07) | AC-05, 07 |
| 3 | [Offer timeout](#3-offer-timeout-ac-05-ac-06) | AC-05, 06 |
| 4 | [Two rides race for the same driver](#4-two-rides-race-for-the-same-driver-ac-09) | AC-09 |
| 5 | [Duplicate delivery](#5-duplicate-delivery-ac-08-ac-10) | AC-08, 10 |
| 6 | [Cancel races](#6-cancel-races-ac-05-ac-06) | AC-05, 06 |
| 7 | [Driver availability and location feed the GEO index](#7-driver-availability-and-location-feed-the-geo-index) | AC-02, 03, 04 |
| 8 | [Crash and recovery](#8-crash-and-recovery-ac-10) | AC-10 |

---

## 1. Happy path: request → offer → accept → complete

```mermaid
sequenceDiagram
    autonumber
    actor P as Passenger
    actor D as Driver D001
    participant RS as ride-service
    participant XS as dispatch-service
    participant R as Redis GEO
    participant DS as driver-service

    P->>RS: POST /rides (Idempotency-Key: k1)
    RS->>RS: [tx] insert ride REQUESTED + idempotency key + outbox RideRequested
    RS-->>P: 201 {rideId: R1, status: REQUESTED}
    RS-)XS: RideRequested(R1)
    XS->>XS: [tx] insert saga(R1) STARTED + outbox StartMatching
    XS-)RS: StartMatching(R1)
    RS->>RS: [tx] REQUESTED → MATCHING
    RS-)XS: CommandSucceeded(StartMatching)
    XS->>R: GEOSEARCH pickup BYRADIUS 2km ASC WITHDIST
    R-->>XS: [D001 0.8km, D002 1.5km]
    XS->>XS: drop excluded, Haversine sort (distance, driverId) → D001
    XS->>XS: [tx] saga RESERVING(D001, offerId O1) + outbox ReserveDriver
    XS-)DS: ReserveDriver(R1, D001, O1)
    DS->>DS: [tx] UPDATE drivers SET OFFERED WHERE id=D001 AND status=AVAILABLE → 1 row
    DS-)XS: DriverReserved(R1, D001)
    DS-)XS: DriverStatusChanged(D001, OFFERED) → projector ZREMs D001
    XS->>XS: [tx] offer O1 PENDING (expires +30s), saga OFFERED + outbox AssignDriver
    XS-)RS: AssignDriver(R1, D001, O1)
    RS->>RS: [tx] MATCHING → DRIVER_ASSIGNED, assignedDriverId=D001

    D->>XS: GET /drivers/D001/offer
    XS-->>D: 200 {offerId O1, rideId R1, expiresAt}
    D->>XS: POST /rides/R1/offers/D001/accept
    XS->>XS: [tx] UPDATE offers SET ACCEPTED WHERE id=O1 AND status=PENDING AND expires_at>now() (pivot)
    XS-->>D: 200 {status: ACCEPTED}
    XS-)DS: MarkDriverBusy(R1, D001)
    XS-)RS: MarkAccepted(R1, D001)
    DS->>DS: OFFERED → BUSY
    RS->>RS: DRIVER_ASSIGNED → DRIVER_ACCEPTED

    D->>RS: POST /rides/R1/complete
    RS->>RS: [tx] DRIVER_ACCEPTED → COMPLETED + outbox RideCompleted
    RS-->>D: 200
    RS-)XS: RideCompleted(R1)
    XS-)DS: ReleaseDriver(R1, D001)
    DS->>DS: BUSY → AVAILABLE (WHERE current_ride_id=R1)
```

---

## 2. Reject → rematch (AC-05, AC-07)

```mermaid
sequenceDiagram
    autonumber
    actor D1 as Driver D001
    participant XS as dispatch-service
    participant DS as driver-service
    participant RS as ride-service

    Note over XS: saga R1 OFFERED to D001 (offer O1 PENDING)
    D1->>XS: POST /rides/R1/offers/D001/reject
    XS->>XS: [tx] O1 PENDING → REJECTED<br/>insert ride_exclusions(R1, D001)<br/>saga → MATCHING<br/>outbox ReleaseDriver + UnassignDriver
    XS-->>D1: 200 {status: REJECTED}
    XS-)DS: ReleaseDriver(R1, D001)  → D001 OFFERED → AVAILABLE
    XS-)RS: UnassignDriver(R1, O1)  → DRIVER_ASSIGNED → MATCHING, assignedDriverId = null
    XS->>XS: match again, excluding {D001} → D002
    XS-)DS: ReserveDriver(R1, D002, O2)
    DS-)XS: DriverReserved
    XS-)RS: AssignDriver(R1, D002, O2)
    Note over XS: D001 is back in Redis (AVAILABLE) but stays excluded for R1 forever,<br/>and is still eligible for other rides.
```

Because `UnassignDriver(O1)` and `AssignDriver(O2)` are both keyed by `R1`, ride-service always applies them in order.

If no eligible candidate remains, the saga goes to `WAITING_FOR_DRIVER`. The ride stays `MATCHING` with `assignedDriverId = null`, and the saga retries every `DISPATCH_RETRY_INTERVAL` (10 s).

---

## 3. Offer timeout (AC-05, AC-06)

```mermaid
sequenceDiagram
    autonumber
    participant SW as Timeout sweeper (XS, every 1s)
    participant XDB as dispatch_db
    participant DS as driver-service
    participant RS as ride-service

    SW->>XDB: SELECT ride_id, id FROM offers WHERE status='PENDING' AND expires_at <= now() LIMIT 100
    XDB-->>SW: [O1 (R1, D001)]
    SW->>XDB: [tx] lock saga R1 (FOR UPDATE SKIP LOCKED)<br/>UPDATE offers SET EXPIRED WHERE id=O1 AND status='PENDING' AND expires_at <= now()<br/>exclude(R1, D001, 'EXPIRED'), saga → MATCHING, outbox Release + Unassign
    SW-)DS: ReleaseDriver(R1, D001)
    SW-)RS: UnassignDriver(R1, O1)
    Note over SW: then continues matching, exactly like a rejection
```

The sweeper locks the saga first, like accept and reject do, so the three can never deadlock. `SKIP LOCKED` lets several dispatch replicas sweep without blocking each other, and the conditional `UPDATE … WHERE status='PENDING'` means an offer is expired at most once. If the driver accepted a moment earlier, the update matches nothing and the sweeper moves on.

---

## 4. Two rides race for the same driver (AC-09)

```mermaid
sequenceDiagram
    autonumber
    participant A as Saga R1 (worker A)
    participant B as Saga R2 (worker B)
    participant DS as driver-service
    participant DB as driver_db

    Note over A,B: both see D001 as nearest in Redis
    A-)DS: ReserveDriver(R1, D001)
    B-)DS: ReserveDriver(R2, D001)
    DS->>DB: UPDATE drivers SET status='OFFERED', current_ride_id=R1<br/>WHERE id='D001' AND status='AVAILABLE'
    DB-->>DS: 1 row ✔
    DS->>DB: UPDATE … current_ride_id=R2 WHERE id='D001' AND status='AVAILABLE'
    DB-->>DS: 0 rows ✘ (status is OFFERED now)
    DS-)A: DriverReserved(R1, D001)
    DS-)B: DriverReservationFailed(R2, D001, reason=NOT_AVAILABLE)
    B->>B: add D001 to round_skips, try next candidate (D002) or WAITING_FOR_DRIVER
```

The result is the same if the two updates run in parallel on different connections: Postgres row locking makes the second one wait, re-check the `WHERE` clause and update 0 rows.

---

## 5. Duplicate delivery (AC-08, AC-10)

```mermaid
sequenceDiagram
    autonumber
    participant K as Redpanda
    participant XS as dispatch-service
    participant XDB as dispatch_db

    K-)XS: RideRequested(R1, eventId E1)
    XS->>XDB: [tx] INSERT inbox(dispatch, E1) ✔ → INSERT saga(R1) → outbox StartMatching
    XS--xK: crash before committing offset
    K-)XS: RideRequested(R1, eventId E1) (redelivered)
    XS->>XDB: INSERT inbox(dispatch, E1) → conflict
    XS->>XS: log "duplicate message skipped" {event_id: E1, ride_id: R1}
    XS->>K: commit offset
```

Even with a *different* `eventId` (for example, the producer re-sent after a lost ack), the second `INSERT saga(R1)` hits the primary key. No second saga is created.

---

## 6. Cancel races (AC-05, AC-06)

```mermaid
sequenceDiagram
    autonumber
    actor P as Passenger
    actor D as Driver D001
    participant RS as ride-service
    participant XS as dispatch-service
    participant DS as driver-service

    Note over RS,XS: R1 DRIVER_ASSIGNED to D001, offer O1 PENDING
    par passenger cancels
        P->>RS: POST /rides/R1/cancel
        RS->>RS: [tx] → CANCELLED + outbox RideCancelled
        RS-->>P: 200 CANCELLED
    and driver accepts at the same time
        D->>XS: POST …/accept
        XS->>XS: O1 PENDING → ACCEPTED ✔
        XS-->>D: 200 ACCEPTED
    end
    XS-)RS: MarkAccepted(R1) → rejected: ride is terminal (CANCELLED)
    RS-)XS: CommandRejected(MarkAccepted, RIDE_TERMINAL)
    RS-)XS: RideCancelled(R1)
    XS->>XS: saga → CANCELLED (first of the two wins, second is no-op)
    XS-)DS: ReleaseDriver(R1, D001)
    DS->>DS: OFFERED/BUSY → AVAILABLE (only if current_ride_id = R1)
```

The ride decides cancellation (it owns the lifecycle). Whatever order the messages arrive in, the end state is the same: ride `CANCELLED`, driver `AVAILABLE`, saga `CANCELLED`. `ReleaseDriver` comes after `MarkDriverBusy` on the same `driverId` partition, so the driver cannot end up stuck in `BUSY`.

---

## 7. Driver availability and location feed the GEO index

```mermaid
sequenceDiagram
    autonumber
    actor D as Driver D011
    participant DS as driver-service
    participant XS as dispatch-service (projection)
    participant R as Redis GEO

    D->>DS: PUT /drivers/D011/availability {"status":"AVAILABLE"}
    DS->>DS: [tx] UPDATE drivers SET status='AVAILABLE', version=v+1<br/>WHERE id='D011' AND status='OFFLINE' + outbox DriverStatusChanged(v+1)
    DS-->>D: 200
    D->>DS: PUT /drivers/D011/location {lat, lng}
    DS->>DS: [tx] UPDATE drivers SET lat, lng, version=v+2 + outbox DriverLocationUpdated(v+2)
    DS-->>D: 200
    DS-)XS: DriverStatusChanged(D011, AVAILABLE, v+1)
    XS->>R: Lua: v+1 > stored? → HSET driver:D011 + GEOADD geo:drivers:available
    XS->>XS: driver became AVAILABLE → wake WAITING_FOR_DRIVER sagas
    DS-)XS: DriverLocationUpdated(D011, v+2)
    XS->>R: Lua: v+2 > v+1 → update position
    Note over XS,R: A late or duplicate event with an older version is ignored,<br/>so matching always uses the latest known location (AC-03).<br/>OFFERED/BUSY/OFFLINE → ZREM: only AVAILABLE drivers are candidates (AC-02, AC-04).
```

Tests: `TestIndexKeepsOnlyLatestAvailableDrivers`, `TestEveryChangeEmitsVersionedEvent`, Postman folder AC-09 (the waiting ride gets the driver as soon as that driver is AVAILABLE again).

---

## 8. Crash and recovery (AC-10)

```mermaid
sequenceDiagram
    autonumber
    actor P as Passenger
    participant RS as ride-service
    participant K as Redpanda
    participant XS as dispatch-service

    Note over XS: dispatch-service is down
    P->>RS: POST /rides
    RS->>RS: [tx] ride REQUESTED + outbox RideRequested
    RS-->>P: 201 REQUESTED (the API does not depend on dispatch)
    RS-)K: relay publishes RideRequested
    Note over K: The message waits in the topic.<br/>The consumer-group offset is not advanced.
    Note over XS: dispatch-service restarts
    K-)XS: RideRequested (from the last committed offset)
    XS->>XS: [tx] inbox + saga STARTED + outbox StartMatching
    Note over XS: The normal flow continues.<br/>A PENDING offer that expired while it was down<br/>is expired by the sweeper and rematched.
```

| Crash point | State left behind | Recovery | Duplicate risk handled by |
|---|---|---|---|
| Before the DB commit | Nothing (transaction rolled back) | Client or broker retries | — |
| After the commit, before publishing | Outbox row unpublished | Relay publishes after restart | — |
| After publishing, before marking published | Message published, row still unpublished | Relay re-publishes the **same `eventId`** | Consumer inbox |
| Consumer: after its DB commit, before the offset commit | Side effect done, offset not committed | Message redelivered | Inbox (skipped) |
| Consumer: handler keeps failing | Nothing committed | 5 retries with backoff + jitter, then `<topic>.dlq`; `make replay-dlq` | Inbox on replay |
| Participant reply lost | Saga stuck in `STARTED`/`RESERVING` | Resumer re-sends the last command after 30 s | Idempotent commands (`rideId`/`offerId` guards) |

Verified manually: kill dispatch-service, request a ride (`201 REQUESTED`), restart it, and the ride is `DRIVER_ASSIGNED` within seconds ([local-development.md §5](local-development.md#5-watch-with-lazydocker)).

