-- One saga per ride. ride_id as PK: a duplicate RideRequested cannot start a second saga.
CREATE TABLE dispatch_sagas (
    ride_id             uuid PRIMARY KEY,
    status              text NOT NULL CHECK (status IN
                          ('STARTED','MATCHING','RESERVING','OFFERED','ACCEPTED',
                           'WAITING_FOR_DRIVER','COMPLETED','CANCELLED')),
    pickup_lat          double precision NOT NULL,
    pickup_lng          double precision NOT NULL,
    radius_m            double precision NOT NULL,
    reserving_driver_id text,
    reserving_offer_id  uuid,
    current_offer_id    uuid,
    round               int  NOT NULL DEFAULT 1,
    round_skips         text[] NOT NULL DEFAULT '{}',   -- reservation lost this round (not exclusions)
    next_retry_at       timestamptz,
    version             bigint NOT NULL DEFAULT 1,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dispatch_sagas_active ON dispatch_sagas (status, updated_at)
    WHERE status NOT IN ('COMPLETED','CANCELLED');

CREATE TABLE offers (
    id           uuid PRIMARY KEY,
    ride_id      uuid NOT NULL REFERENCES dispatch_sagas (ride_id),
    driver_id    text NOT NULL,
    status       text NOT NULL CHECK (status IN ('PENDING','ACCEPTED','REJECTED','EXPIRED','CANCELLED')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    responded_at timestamptz
);
-- Defence in depth for AC-09 (the primary guard is the driver row in driver_db).
CREATE UNIQUE INDEX one_pending_offer_per_driver ON offers (driver_id) WHERE status = 'PENDING';
CREATE UNIQUE INDEX one_pending_offer_per_ride   ON offers (ride_id)   WHERE status = 'PENDING';
CREATE INDEX offers_expiry ON offers (expires_at) WHERE status = 'PENDING';
CREATE INDEX offers_ride   ON offers (ride_id, created_at);

-- Drivers who rejected or timed out on a ride never get that ride again (AC-07).
CREATE TABLE ride_exclusions (
    ride_id   uuid NOT NULL,
    driver_id text NOT NULL,
    reason    text NOT NULL,
    at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (ride_id, driver_id)
);

-- Audit trail of every saga step ("what happened to ride X?").
CREATE TABLE saga_steps (
    id        bigserial PRIMARY KEY,
    ride_id   uuid NOT NULL,
    step      text NOT NULL,
    detail    text,
    driver_id text,
    offer_id  uuid,
    event_id  text,
    at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX saga_steps_ride ON saga_steps (ride_id, id);
