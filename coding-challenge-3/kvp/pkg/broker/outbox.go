// Package broker is the messaging infrastructure shared by all services
// (an outer layer in Clean Architecture terms): the transactional outbox and
// its relay, the inbox (idempotent consumer), the Redpanda/Kafka consumer with
// retry and DLQ, and topic administration. Use cases never import it; they
// depend on the pure contract in pkg/messaging and on their own ports.
package broker

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"ridehail/pkg/messaging"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"

	"ridehail/pkg/obs"
	"ridehail/pkg/pgutil"
)

//go:embed schema/*.sql
var schemaFS embed.FS

// Migrations holds the outbox/inbox tables; every service applies them.
var Migrations, _ = fs.Sub(schemaFS, "schema")

// Enqueue appends env to the outbox inside tx. The message is published only
// if tx commits, which removes the dual-write problem (ADR-004).
// It fills EventID, OccurredAt, CorrelationID (= rideId) and CausationID
// (= the event being handled, from ctx), and stores the trace context.
func Enqueue(ctx context.Context, tx pgutil.DB, topic, key string, env messaging.Envelope) (messaging.Envelope, error) {
	if env.EventID == "" {
		env.EventID = messaging.NewID()
	}
	if env.OccurredAt.IsZero() {
		env.OccurredAt = time.Now().UTC()
	}
	if env.CorrelationID == "" {
		env.CorrelationID = env.RideID
	}
	if env.CausationID == "" {
		env.CausationID = obs.Field(ctx, obs.EventID)
	}
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	body, err := json.Marshal(env)
	if err != nil {
		return env, err
	}
	hdr, _ := json.Marshal(carrier)
	_, err = tx.Exec(ctx, `INSERT INTO outbox(topic, msg_key, event_id, type, envelope, headers)
		VALUES ($1,$2,$3,$4,$5,$6)`, topic, key, env.EventID, env.Type, body, hdr)
	if err == nil {
		// Log the produced message under its own ids; causation_id links it to
		// the message or request that caused it, trace_id to the ride's trace.
		mctx := obs.With(ctx, obs.EventID, env.EventID, obs.CausationID, env.CausationID, obs.MessageType, env.Type,
			obs.RideID, env.RideID, obs.DriverID, env.DriverID, obs.OfferID, env.OfferID)
		slog.InfoContext(mctx, "message produced", "direction", "produce", "via", "outbox",
			"topic", topic, "key", key, "value", obs.NewBody(body))
	}
	return env, err
}

// Relay publishes unpublished outbox rows in id order (at-least-once).
type Relay struct {
	Pool     *pgxpool.Pool
	Client   *kgo.Client
	Interval time.Duration
}

// Run polls until ctx is cancelled.
func (r *Relay) Run(ctx context.Context) {
	if r.Interval == 0 {
		r.Interval = 50 * time.Millisecond
	}
	meter := otel.Meter("ridehail/messaging")
	_, _ = meter.Int64ObservableGauge("outbox_unpublished",
		metric.WithDescription("Outbox rows waiting to be published"),
		metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
			var n int64
			if err := r.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&n); err == nil {
				o.Observe(n)
			}
			return nil
		}))

	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for {
			n, err := r.flush(ctx)
			if err != nil {
				if ctx.Err() == nil {
					slog.ErrorContext(ctx, "outbox relay failed", "error", err)
				}
				break
			}
			if n == 0 {
				break
			}
		}
	}
}

func (r *Relay) flush(ctx context.Context) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, topic, msg_key, type, envelope, headers FROM outbox
			WHERE published_at IS NULL ORDER BY id LIMIT 200 FOR UPDATE SKIP LOCKED`)
		if err != nil {
			return err
		}
		var ids []int64
		var recs []*kgo.Record
		for rows.Next() {
			var (
				id              int64
				topic, key, typ string
				body, hdrJSON   []byte
			)
			if err := rows.Scan(&id, &topic, &key, &typ, &body, &hdrJSON); err != nil {
				return err
			}
			hdr := map[string]string{}
			_ = json.Unmarshal(hdrJSON, &hdr)
			rec := &kgo.Record{Topic: topic, Key: []byte(key), Value: body}
			rec.Headers = append(rec.Headers, kgo.RecordHeader{Key: "type", Value: []byte(typ)})
			for k, v := range hdr {
				rec.Headers = append(rec.Headers, kgo.RecordHeader{Key: k, Value: []byte(v)})
			}
			ids = append(ids, id)
			recs = append(recs, rec)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(recs) == 0 {
			return nil
		}
		if err := r.Client.ProduceSync(ctx, recs...).FirstErr(); err != nil {
			return err
		}
		n = len(recs)
		_, err = tx.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = ANY($1)`, ids)
		if err == nil {
			slog.DebugContext(ctx, "outbox batch published to broker", "count", n)
		}
		return err
	})
	return n, err
}

// Claim records eventID as processed by consumer inside tx.
// It returns false when the message was already processed (duplicate).
func Claim(ctx context.Context, tx pgutil.DB, consumer, eventID string) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO inbox(consumer, event_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, consumer, eventID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Publisher is the outbox as a use-case port implementation: Publish writes
// the message into the outbox of the transaction carried by ctx.
type Publisher struct{}

// Publish enqueues env in the current transaction (see pgutil.TxManager).
func (Publisher) Publish(ctx context.Context, topic, key string, env messaging.Envelope) (messaging.Envelope, error) {
	tx, err := pgutil.Tx(ctx)
	if err != nil {
		return env, err
	}
	return Enqueue(ctx, tx, topic, key, env)
}

// Inbox is the idempotent-consumer port implementation for one consumer name.
type Inbox struct{ Consumer string }

// Claim marks eventID as processed in the current transaction; false = duplicate.
func (i Inbox) Claim(ctx context.Context, eventID string) (bool, error) {
	tx, err := pgutil.Tx(ctx)
	if err != nil {
		return false, err
	}
	return Claim(ctx, tx, i.Consumer, eventID)
}
