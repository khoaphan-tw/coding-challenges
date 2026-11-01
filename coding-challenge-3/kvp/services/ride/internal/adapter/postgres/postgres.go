// Package postgres is ride-service's driven adapter: it implements the use
// case ports (RideRepository, PassengerRepository, IdempotencyStore) with pgx.
// Queries join the transaction carried by ctx (pgutil.TxManager).
package postgres

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ridehail/pkg/geo"
	"ridehail/pkg/pgutil"
	"ridehail/services/ride/internal/domain"
	"ridehail/services/ride/internal/usecase"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations is ride-service's schema.
var Migrations, _ = fs.Sub(migrationsFS, "migrations")

// Repos implements usecase.RideRepository, PassengerRepository and IdempotencyStore.
type Repos struct {
	Pool *pgxpool.Pool
}

var (
	_ usecase.RideRepository      = (*Repos)(nil)
	_ usecase.PassengerRepository = (*Repos)(nil)
	_ usecase.IdempotencyStore    = (*Repos)(nil)
)

func (r *Repos) db(ctx context.Context) pgutil.DB { return pgutil.Conn(ctx, r.Pool) }

const rideCols = `id, passenger_id, pickup_lat, pickup_lng, dest_lat, dest_lng, status,
	assigned_driver_id, current_offer_id, version, created_at, updated_at, coalesce(trace_parent, '')`

func scanRide(row pgx.Row) (domain.Ride, error) {
	var r domain.Ride
	err := row.Scan(&r.ID, &r.PassengerID, &r.Pickup.Latitude, &r.Pickup.Longitude,
		&r.Destination.Latitude, &r.Destination.Longitude, &r.Status,
		&r.AssignedDriverID, &r.CurrentOfferID, &r.Version, &r.CreatedAt, &r.UpdatedAt, &r.TraceParent)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, usecase.ErrRideNotFound
	}
	return r, err
}

func validID(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil
}

// Get returns one ride.
func (r *Repos) Get(ctx context.Context, id string) (domain.Ride, error) {
	if !validID(id) {
		return domain.Ride{}, usecase.ErrRideNotFound
	}
	return scanRide(r.db(ctx).QueryRow(ctx, `SELECT `+rideCols+` FROM rides WHERE id=$1`, id))
}

// GetForUpdate loads the ride with a row lock (SELECT … FOR UPDATE, ADR-002).
func (r *Repos) GetForUpdate(ctx context.Context, id string) (domain.Ride, error) {
	if !validID(id) {
		return domain.Ride{}, usecase.ErrRideNotFound
	}
	return scanRide(r.db(ctx).QueryRow(ctx, `SELECT `+rideCols+` FROM rides WHERE id=$1 FOR UPDATE`, id))
}

// Insert stores a new ride.
func (r *Repos) Insert(ctx context.Context, d domain.Ride) (domain.Ride, error) {
	return scanRide(r.db(ctx).QueryRow(ctx, `INSERT INTO rides(id, passenger_id, pickup_lat, pickup_lng,
		dest_lat, dest_lng, status, version, created_at, updated_at, trace_parent)
		VALUES ($1,$2,$3,$4,$5,$6,$7,1,$8,$8,NULLIF($9,'')) RETURNING `+rideCols,
		d.ID, d.PassengerID, d.Pickup.Latitude, d.Pickup.Longitude, d.Destination.Latitude, d.Destination.Longitude,
		d.Status, d.CreatedAt, d.TraceParent))
}

// Update stores the new status/assignment and increments the version.
func (r *Repos) Update(ctx context.Context, d domain.Ride) (domain.Ride, error) {
	return scanRide(r.db(ctx).QueryRow(ctx, `UPDATE rides SET status=$2, assigned_driver_id=$3, current_offer_id=$4,
		version=version+1, updated_at=now() WHERE id=$1 RETURNING `+rideCols,
		d.ID, d.Status, d.AssignedDriverID, d.CurrentOfferID))
}

// AddTransition appends one history row.
func (r *Repos) AddTransition(ctx context.Context, t domain.Transition) error {
	var offer *string
	if t.OfferID != "" {
		offer = &t.OfferID
	}
	_, err := r.db(ctx).Exec(ctx, `INSERT INTO ride_transitions(ride_id, from_status, to_status, action, driver_id, offer_id, cause)
		VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$7)`, t.RideID, t.From, t.To, t.Action, t.DriverID, offer, t.Cause)
	return err
}

// History returns the status changes of a ride, oldest first.
func (r *Repos) History(ctx context.Context, rideID string) ([]domain.Transition, error) {
	rows, err := r.db(ctx).Query(ctx, `SELECT ride_id::text, from_status, to_status, action, coalesce(driver_id,''),
		coalesce(offer_id::text,''), coalesce(cause,''), at FROM ride_transitions WHERE ride_id=$1 ORDER BY id`, rideID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Transition, error) {
		var t domain.Transition
		err := row.Scan(&t.RideID, &t.From, &t.To, &t.Action, &t.DriverID, &t.OfferID, &t.Cause, &t.At)
		return t, err
	})
}

// Exists reports whether a passenger is seeded.
func (r *Repos) Exists(ctx context.Context, id string) (bool, error) {
	var ok bool
	err := r.db(ctx).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM passengers WHERE id=$1)`, id).Scan(&ok)
	return ok, err
}

// rideSnapshot is how the original response is stored with an Idempotency-Key.
// Same JSON shape as the HTTP response, so rows written by older versions still parse.
type rideSnapshot struct {
	RideID           string    `json:"rideId"`
	PassengerID      string    `json:"passengerId"`
	Pickup           geo.Point `json:"pickup"`
	Destination      geo.Point `json:"destination"`
	Status           string    `json:"status"`
	AssignedDriverID *string   `json:"assignedDriverId"`
	CurrentOfferID   *string   `json:"currentOfferId,omitempty"`
	Version          int64     `json:"version"`
	TraceParent      string    `json:"traceParent,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

// Reserve inserts the key first: the unique key serialises concurrent duplicates.
func (r *Repos) Reserve(ctx context.Context, key, requestHash, rideID string) (*usecase.IdempotencyRecord, error) {
	db := r.db(ctx)
	tag, err := db.Exec(ctx, `INSERT INTO idempotency_keys(key, request_hash, ride_id, status_code)
		VALUES ($1,$2,$3,201) ON CONFLICT (key) DO NOTHING`, key, requestHash, rideID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 1 {
		return nil, nil
	}
	var rec usecase.IdempotencyRecord
	var body []byte
	if err := db.QueryRow(ctx, `SELECT request_hash, response FROM idempotency_keys WHERE key=$1`, key).
		Scan(&rec.RequestHash, &body); err != nil {
		return nil, err
	}
	var s rideSnapshot
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, err
	}
	rec.Ride = domain.Ride{ID: s.RideID, PassengerID: s.PassengerID, Pickup: s.Pickup, Destination: s.Destination,
		Status: domain.Status(s.Status), AssignedDriverID: s.AssignedDriverID, CurrentOfferID: s.CurrentOfferID,
		Version: s.Version, TraceParent: s.TraceParent, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}
	return &rec, nil
}

// SaveResult stores the original response for replays.
func (r *Repos) SaveResult(ctx context.Context, key string, d domain.Ride) error {
	body, err := json.Marshal(rideSnapshot{RideID: d.ID, PassengerID: d.PassengerID, Pickup: d.Pickup,
		Destination: d.Destination, Status: string(d.Status), AssignedDriverID: d.AssignedDriverID,
		CurrentOfferID: d.CurrentOfferID, Version: d.Version, TraceParent: d.TraceParent,
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt})
	if err != nil {
		return err
	}
	_, err = r.db(ctx).Exec(ctx, `UPDATE idempotency_keys SET response=$2 WHERE key=$1`, key, body)
	return err
}

// Ready checks the database.
func (r *Repos) Ready(ctx context.Context) error { return r.Pool.Ping(ctx) }
