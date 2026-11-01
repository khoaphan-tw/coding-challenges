// Package postgres is dispatch-service's driven adapter: saga, offer and
// exclusion repositories with pgx. Queries join the transaction in ctx.
package postgres

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ridehail/pkg/pgutil"
	"ridehail/services/dispatch/internal/domain"
	"ridehail/services/dispatch/internal/usecase"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations is dispatch-service's schema.
var Migrations, _ = fs.Sub(migrationsFS, "migrations")

// Sagas implements usecase.SagaRepository.
type Sagas struct{ Pool *pgxpool.Pool }

// Offers implements usecase.OfferRepository.
type Offers struct{ Pool *pgxpool.Pool }

// Exclusions implements usecase.ExclusionRepository.
type Exclusions struct{ Pool *pgxpool.Pool }

var (
	_ usecase.SagaRepository      = (*Sagas)(nil)
	_ usecase.OfferRepository     = (*Offers)(nil)
	_ usecase.ExclusionRepository = (*Exclusions)(nil)
)

func (r *Sagas) db(ctx context.Context) pgutil.DB      { return pgutil.Conn(ctx, r.Pool) }
func (r *Offers) db(ctx context.Context) pgutil.DB     { return pgutil.Conn(ctx, r.Pool) }
func (r *Exclusions) db(ctx context.Context) pgutil.DB { return pgutil.Conn(ctx, r.Pool) }

func validID(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil
}

// ---- sagas ----

const sagaCols = `ride_id, status, pickup_lat, pickup_lng, radius_m, reserving_driver_id, reserving_offer_id,
	current_offer_id, round, round_skips, next_retry_at, version, created_at, updated_at, coalesce(trace_parent, '')`

func scanSaga(row pgx.Row) (domain.Saga, error) {
	var s domain.Saga
	err := row.Scan(&s.RideID, &s.Status, &s.Pickup.Latitude, &s.Pickup.Longitude, &s.RadiusM,
		&s.ReservingDriverID, &s.ReservingOfferID, &s.CurrentOfferID, &s.Round, &s.RoundSkips,
		&s.NextRetryAt, &s.Version, &s.CreatedAt, &s.UpdatedAt, &s.TraceParent)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, usecase.ErrSagaNotFound
	}
	return s, err
}

// Create inserts a saga; the ride id is the primary key (one saga per ride).
func (r *Sagas) Create(ctx context.Context, s domain.Saga) (bool, error) {
	tag, err := r.db(ctx).Exec(ctx, `INSERT INTO dispatch_sagas(ride_id, status, pickup_lat, pickup_lng, radius_m, trace_parent)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,'')) ON CONFLICT (ride_id) DO NOTHING`,
		s.RideID, s.Status, s.Pickup.Latitude, s.Pickup.Longitude, s.RadiusM, s.TraceParent)
	return tag.RowsAffected() == 1, err
}

// Get returns the saga of a ride.
func (r *Sagas) Get(ctx context.Context, rideID string) (domain.Saga, error) {
	if !validID(rideID) {
		return domain.Saga{}, usecase.ErrSagaNotFound
	}
	return scanSaga(r.db(ctx).QueryRow(ctx, `SELECT `+sagaCols+` FROM dispatch_sagas WHERE ride_id=$1`, rideID))
}

// GetForUpdate locks the saga row until the transaction ends.
func (r *Sagas) GetForUpdate(ctx context.Context, rideID string) (domain.Saga, error) {
	if !validID(rideID) {
		return domain.Saga{}, usecase.ErrSagaNotFound
	}
	return scanSaga(r.db(ctx).QueryRow(ctx, `SELECT `+sagaCols+` FROM dispatch_sagas WHERE ride_id=$1 FOR UPDATE`, rideID))
}

// TryGetForUpdate locks the saga row unless another transaction holds it.
func (r *Sagas) TryGetForUpdate(ctx context.Context, rideID string) (domain.Saga, bool, error) {
	s, err := scanSaga(r.db(ctx).QueryRow(ctx, `SELECT `+sagaCols+` FROM dispatch_sagas
		WHERE ride_id=$1 FOR UPDATE SKIP LOCKED`, rideID))
	if errors.Is(err, usecase.ErrSagaNotFound) {
		return s, false, nil
	}
	return s, err == nil, err
}

// Save stores the saga and increments its version.
func (r *Sagas) Save(ctx context.Context, s domain.Saga) error {
	if s.RoundSkips == nil {
		s.RoundSkips = []string{}
	}
	_, err := r.db(ctx).Exec(ctx, `UPDATE dispatch_sagas SET status=$2, reserving_driver_id=$3, reserving_offer_id=$4,
		current_offer_id=$5, round=$6, round_skips=$7, next_retry_at=$8, version=version+1, updated_at=now()
		WHERE ride_id=$1`, s.RideID, s.Status, s.ReservingDriverID, s.ReservingOfferID, s.CurrentOfferID,
		s.Round, s.RoundSkips, s.NextRetryAt)
	return err
}

// AddStep appends an audit row.
func (r *Sagas) AddStep(ctx context.Context, st domain.Step) error {
	_, err := r.db(ctx).Exec(ctx, `INSERT INTO saga_steps(ride_id, step, detail, driver_id, offer_id, event_id)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,NULLIF($6,''))`,
		st.RideID, st.Name, st.Detail, st.DriverID, domain.Ptr(st.OfferID), st.EventID)
	return err
}

// Steps returns the audit rows of a ride, oldest first.
func (r *Sagas) Steps(ctx context.Context, rideID string) ([]domain.Step, error) {
	rows, err := r.db(ctx).Query(ctx, `SELECT ride_id::text, step, coalesce(detail,''), coalesce(driver_id,''),
		coalesce(offer_id::text,''), coalesce(event_id,''), at FROM saga_steps WHERE ride_id=$1 ORDER BY id`, rideID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Step, error) {
		var s domain.Step
		err := row.Scan(&s.RideID, &s.Name, &s.Detail, &s.DriverID, &s.OfferID, &s.EventID, &s.At)
		return s, err
	})
}

// DueForRetry lists sagas to retry or resume.
func (r *Sagas) DueForRetry(ctx context.Context, stuckAfter time.Duration, limit int) ([]string, error) {
	rows, err := r.db(ctx).Query(ctx, `SELECT ride_id::text FROM dispatch_sagas
		WHERE (status='WAITING_FOR_DRIVER' AND next_retry_at <= now())
		   OR (status IN ('MATCHING','STARTED','RESERVING') AND updated_at < now() - make_interval(secs => $1))
		ORDER BY updated_at LIMIT $2`, stuckAfter.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// WakeWaiting makes waiting sagas due now.
func (r *Sagas) WakeWaiting(ctx context.Context) error {
	_, err := r.db(ctx).Exec(ctx, `UPDATE dispatch_sagas SET next_retry_at = now()
		WHERE status='WAITING_FOR_DRIVER' AND next_retry_at > now()`)
	return err
}

// ---- offers ----

const offerCols = `id::text, ride_id::text, driver_id, status, created_at, expires_at, responded_at`

func scanOffer(row pgx.Row) (domain.Offer, error) {
	var o domain.Offer
	err := row.Scan(&o.ID, &o.RideID, &o.DriverID, &o.Status, &o.CreatedAt, &o.ExpiresAt, &o.RespondedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, usecase.ErrOfferNotFound
	}
	return o, err
}

// casOffer maps "no row matched" to applied=false.
func casOffer(o domain.Offer, err error) (domain.Offer, bool, error) {
	if errors.Is(err, usecase.ErrOfferNotFound) {
		return o, false, nil
	}
	return o, err == nil, err
}

// Create inserts a PENDING offer. Partial unique indexes allow one pending
// offer per driver and per ride (defence in depth for AC-09).
func (r *Offers) Create(ctx context.Context, id, rideID, driverID string, timeout time.Duration) error {
	_, err := r.db(ctx).Exec(ctx, `INSERT INTO offers(id, ride_id, driver_id, status, expires_at)
		VALUES ($1,$2,$3,'PENDING', now() + make_interval(secs => $4))`, id, rideID, driverID, timeout.Seconds())
	if pgutil.IsUniqueViolation(err) {
		return usecase.ErrDuplicatePendingOffer
	}
	return err
}

// Get returns one offer.
func (r *Offers) Get(ctx context.Context, id string) (domain.Offer, error) {
	return scanOffer(r.db(ctx).QueryRow(ctx, `SELECT `+offerCols+` FROM offers WHERE id=$1`, id))
}

// RespondPending is the compare-and-set on status='PENDING'.
func (r *Offers) RespondPending(ctx context.Context, rideID, driverID, to string, notExpired bool) (domain.Offer, bool, error) {
	if !validID(rideID) {
		return domain.Offer{}, false, nil
	}
	return casOffer(scanOffer(r.db(ctx).QueryRow(ctx, `UPDATE offers SET status=$3, responded_at=now()
		WHERE ride_id=$1 AND driver_id=$2 AND status='PENDING' AND (NOT $4 OR expires_at > now())
		RETURNING `+offerCols, rideID, driverID, to, notExpired)))
}

// ExpireIfDue expires a PENDING offer past its deadline.
func (r *Offers) ExpireIfDue(ctx context.Context, offerID string) (domain.Offer, bool, error) {
	return casOffer(scanOffer(r.db(ctx).QueryRow(ctx, `UPDATE offers SET status='EXPIRED', responded_at=now()
		WHERE id=$1 AND status='PENDING' AND expires_at <= now() RETURNING `+offerCols, offerID)))
}

// CancelPending cancels a PENDING offer.
func (r *Offers) CancelPending(ctx context.Context, offerID string) (bool, error) {
	tag, err := r.db(ctx).Exec(ctx, `UPDATE offers SET status='CANCELLED', responded_at=now()
		WHERE id=$1 AND status='PENDING'`, offerID)
	return tag.RowsAffected() == 1, err
}

// Latest returns the newest offer for (ride, driver).
func (r *Offers) Latest(ctx context.Context, rideID, driverID string) (domain.Offer, error) {
	if !validID(rideID) {
		return domain.Offer{}, usecase.ErrOfferNotFound
	}
	return scanOffer(r.db(ctx).QueryRow(ctx, `SELECT `+offerCols+` FROM offers
		WHERE ride_id=$1 AND driver_id=$2 ORDER BY created_at DESC LIMIT 1`, rideID, driverID))
}

// PendingForDriver returns the driver's open offer, or nil.
func (r *Offers) PendingForDriver(ctx context.Context, driverID string) (*domain.Offer, error) {
	o, err := scanOffer(r.db(ctx).QueryRow(ctx, `SELECT `+offerCols+` FROM offers
		WHERE driver_id=$1 AND status='PENDING' AND expires_at > now()`, driverID))
	if errors.Is(err, usecase.ErrOfferNotFound) {
		return nil, nil
	}
	return &o, err
}

// ListByRide returns all offers of a ride, oldest first.
func (r *Offers) ListByRide(ctx context.Context, rideID string) ([]domain.Offer, error) {
	rows, err := r.db(ctx).Query(ctx, `SELECT `+offerCols+` FROM offers WHERE ride_id=$1 ORDER BY created_at`, rideID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Offer, error) { return scanOffer(row) })
}

// DueExpired lists PENDING offers past their deadline.
func (r *Offers) DueExpired(ctx context.Context, limit int) ([]domain.Offer, error) {
	rows, err := r.db(ctx).Query(ctx, `SELECT `+offerCols+` FROM offers
		WHERE status='PENDING' AND expires_at <= now() ORDER BY expires_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Offer, error) { return scanOffer(row) })
}

// ---- exclusions ----

// Add excludes a driver from a ride permanently (idempotent).
func (r *Exclusions) Add(ctx context.Context, rideID, driverID, reason string) error {
	_, err := r.db(ctx).Exec(ctx, `INSERT INTO ride_exclusions(ride_id, driver_id, reason) VALUES ($1,$2,$3)
		ON CONFLICT DO NOTHING`, rideID, driverID, reason)
	return err
}

// List returns excluded drivers of a ride and why.
func (r *Exclusions) List(ctx context.Context, rideID string) (map[string]string, error) {
	rows, err := r.db(ctx).Query(ctx, `SELECT driver_id, reason FROM ride_exclusions WHERE ride_id=$1`, rideID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var d, reason string
		if err := rows.Scan(&d, &reason); err != nil {
			return nil, err
		}
		out[d] = reason
	}
	return out, rows.Err()
}

// Ready checks the database.
func (r *Sagas) Ready(ctx context.Context) error { return r.Pool.Ping(ctx) }
