//go:build integration

package usecase_test

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ridehail/pkg/broker"
	"ridehail/pkg/geo"
	"ridehail/pkg/messaging"
	"ridehail/pkg/pgutil"
	"ridehail/pkg/testpg"
	dmsg "ridehail/services/dispatch/internal/adapter/messaging"
	"ridehail/services/dispatch/internal/adapter/postgres"
	"ridehail/services/dispatch/internal/domain"
	"ridehail/services/dispatch/internal/usecase"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) { testpg.Main(m, &pool, postgres.Migrations) }

var pickup = geo.Point{Latitude: 10.7769, Longitude: 106.7009}

func north(m float64) geo.Point {
	return geo.Point{Latitude: pickup.Latitude + m/111195.0, Longitude: pickup.Longitude}
}

// world simulates the geo index, ride-service and driver-service.
type world struct {
	mu     sync.Mutex
	t      *testing.T
	o      *usecase.Orchestrator
	h      dmsg.Handlers // the messaging adapter, as in production
	loc    map[string]geo.Point
	holder map[string]string // driverId -> rideId (OFFERED/BUSY)
	sent   []messaging.Envelope
	prefix string
}

func newWorld(t *testing.T) *world {
	w := &world{t: t, loc: map[string]geo.Point{}, holder: map[string]string{}, prefix: messaging.NewID()[24:] + "-"}
	w.o = (&usecase.Orchestrator{Tx: pgutil.TxManager{Pool: pool}, Sagas: &postgres.Sagas{Pool: pool},
		Offers: &postgres.Offers{Pool: pool}, Exclusions: &postgres.Exclusions{Pool: pool}, Candidates: w,
		Publisher: broker.Publisher{}, Inbox: broker.Inbox{Consumer: "dispatch-service"},
		OfferTimeout: 30 * time.Second, RetryInterval: time.Hour}).Init()
	w.h = dmsg.Handlers{Orchestrator: w.o}
	return w
}

func (w *world) driver(name string, metresNorth float64) string {
	id := w.prefix + name
	w.loc[id] = north(metresNorth)
	return id
}

// Candidates implements CandidateSource: available drivers only, nearest first.
func (w *world) Candidates(_ context.Context, p geo.Point, _ float64, count int) ([]domain.Candidate, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []domain.Candidate
	for id, l := range w.loc {
		if _, busy := w.holder[id]; !busy {
			out = append(out, domain.Candidate{DriverID: id, Location: l, Status: "AVAILABLE"})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return geo.HaversineM(p, out[i].Location) < geo.HaversineM(p, out[j].Location)
	})
	if len(out) > count {
		out = out[:count]
	}
	return out, nil
}

func (w *world) requestRide() string {
	id := messaging.NewID()
	env := messaging.New(messaging.RideRequested, id, "", "", map[string]any{"pickup": pickup})
	env.EventID = messaging.NewID()
	if err := w.h.RideEvent(context.Background(), env); err != nil {
		w.t.Fatal(err)
	}
	return id
}

// pump answers every pending saga command like the real participants would,
// until the saga stops sending commands.
func (w *world) pump() {
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		rows, err := pool.Query(ctx, `UPDATE outbox SET published_at=now() WHERE id IN (
			SELECT id FROM outbox WHERE published_at IS NULL AND topic IN ($1,$2) ORDER BY id)
			RETURNING id, envelope`, messaging.TopicRideCommands, messaging.TopicDriverCommands)
		if err != nil {
			w.t.Fatal(err)
		}
		type item struct {
			id  int64
			env messaging.Envelope
		}
		var items []item
		for rows.Next() {
			var it item
			var raw []byte
			if err := rows.Scan(&it.id, &raw); err != nil {
				w.t.Fatal(err)
			}
			_ = json.Unmarshal(raw, &it.env)
			items = append(items, it)
		}
		rows.Close()
		if len(items) == 0 {
			return
		}
		sort.Slice(items, func(a, b int) bool { return items[a].id < items[b].id })
		for _, it := range items {
			w.answer(it.env)
		}
	}
	w.t.Fatal("saga did not settle")
}

func (w *world) answer(cmd messaging.Envelope) {
	w.mu.Lock()
	w.sent = append(w.sent, cmd)
	reply := messaging.ReplyCommandSucceeded
	switch cmd.Type {
	case messaging.CmdReserveDriver:
		if r, busy := w.holder[cmd.DriverID]; busy && r != cmd.RideID {
			reply = messaging.ReplyDriverReservationFailed
		} else {
			w.holder[cmd.DriverID] = cmd.RideID
			reply = messaging.ReplyDriverReserved
		}
	case messaging.CmdReleaseDriver:
		if w.holder[cmd.DriverID] == cmd.RideID {
			delete(w.holder, cmd.DriverID)
		}
	}
	w.mu.Unlock()
	env := messaging.New(reply, cmd.RideID, cmd.DriverID, cmd.OfferID,
		messaging.CommandResult{Command: cmd.Type, CommandEventID: cmd.EventID})
	env.EventID = messaging.NewID()
	if err := w.h.Reply(context.Background(), env); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) saga(ride string) domain.Saga {
	s, err := (&postgres.Sagas{Pool: pool}).Get(context.Background(), ride)
	if err != nil {
		w.t.Fatal(err)
	}
	return s
}

func (w *world) pendingDriver(ride string) string {
	var d string
	err := pool.QueryRow(context.Background(), `SELECT driver_id FROM offers WHERE ride_id=$1 AND status='PENDING'`, ride).Scan(&d)
	if err != nil {
		return ""
	}
	return d
}

func (w *world) count(typ, ride string) int {
	n := 0
	for _, e := range w.sent {
		if e.Type == typ && e.RideID == ride {
			n++
		}
	}
	return n
}

func TestNearestDriverGetsOfferAndAcceptMovesForward(t *testing.T) {
	w := newWorld(t)
	near, _ := w.driver("near", 300), w.driver("far", 1500)
	w.driver("outside", 3200)
	ride := w.requestRide()
	w.pump()

	if got := w.pendingDriver(ride); got != near {
		t.Fatalf("offer went to %q, want nearest %q", got, near)
	}
	if s := w.saga(ride); s.Status != domain.Offered {
		t.Fatalf("saga %s, want OFFERED", s.Status)
	}
	if w.count(messaging.CmdAssignDriver, ride) != 1 {
		t.Fatal("ride must be assigned (DRIVER_ASSIGNED) when the offer is created")
	}
	if _, err := w.o.Accept(context.Background(), ride, near); err != nil {
		t.Fatal(err)
	}
	w.pump()
	if s := w.saga(ride); s.Status != domain.Accepted {
		t.Fatalf("saga %s, want ACCEPTED", s.Status)
	}
	if w.count(messaging.CmdMarkDriverBusy, ride) != 1 || w.count(messaging.CmdMarkAccepted, ride) != 1 {
		t.Fatal("accept must mark the driver BUSY and the ride DRIVER_ACCEPTED")
	}
}

func TestRejectionRematchesAndExcludesTheDriverForever(t *testing.T) {
	w := newWorld(t)
	d1, d2 := w.driver("d1", 200), w.driver("d2", 900)
	ride := w.requestRide()
	w.pump()
	if w.pendingDriver(ride) != d1 {
		t.Fatal("first offer must go to d1")
	}

	if _, err := w.o.Reject(context.Background(), ride, d1); err != nil {
		t.Fatal(err)
	}
	w.pump()
	if got := w.pendingDriver(ride); got != d2 {
		t.Fatalf("second offer went to %q, want %q", got, d2)
	}
	if w.count(messaging.CmdUnassignDriver, ride) != 1 || w.count(messaging.CmdReleaseDriver, ride) != 1 {
		t.Fatal("rejection must unassign the ride and release the driver")
	}

	// d1 is AVAILABLE again and nearest, but stays excluded for this ride.
	if _, err := w.o.Reject(context.Background(), ride, d2); err != nil {
		t.Fatal(err)
	}
	w.pump()
	s := w.saga(ride)
	if s.Status != domain.WaitingForDriver || w.pendingDriver(ride) != "" {
		t.Fatalf("no eligible driver left: want WAITING_FOR_DRIVER with no offer, got %s / %q", s.Status, w.pendingDriver(ride))
	}
	if n := w.count(messaging.CmdReserveDriver, ride); n != 2 {
		t.Fatalf("rejected drivers were re-offered: %d reservations", n)
	}

	// Retrying later still never re-offers the rejected drivers.
	_, _ = pool.Exec(context.Background(), `UPDATE dispatch_sagas SET next_retry_at=now() WHERE ride_id=$1`, ride)
	w.o.RetryAndResume(context.Background(), time.Hour)
	w.pump()
	if w.count(messaging.CmdReserveDriver, ride) != 2 {
		t.Fatal("retry re-offered an excluded driver")
	}

	// The exclusion is per ride: d1 can serve another ride.
	other := w.requestRide()
	w.pump()
	if w.pendingDriver(other) != d1 {
		t.Fatal("a driver who rejected one ride must stay eligible for other rides")
	}
}

func TestTimeoutIsTreatedAsRejection(t *testing.T) {
	w := newWorld(t)
	d1, d2 := w.driver("d1", 200), w.driver("d2", 900)
	ride := w.requestRide()
	w.pump()
	_, _ = pool.Exec(context.Background(), `UPDATE offers SET expires_at = now() - interval '1 second'
		WHERE ride_id=$1 AND status='PENDING'`, ride)

	if _, err := w.o.Accept(context.Background(), ride, d1); !errors.Is(err, usecase.ErrOfferExpired) {
		t.Fatalf("accept after the deadline must fail with OFFER_EXPIRED, got %v", err)
	}
	w.o.SweepExpired(context.Background())
	w.pump()

	var status string
	_ = pool.QueryRow(context.Background(), `SELECT status FROM offers WHERE ride_id=$1 AND driver_id=$2`, ride, d1).Scan(&status)
	if status != domain.OfferExpired {
		t.Fatalf("offer status %s, want EXPIRED", status)
	}
	if w.pendingDriver(ride) != d2 {
		t.Fatal("timeout must rematch to the next driver")
	}
	ex, _ := (&postgres.Exclusions{Pool: pool}).List(context.Background(), ride)
	if ex[d1] == "" {
		t.Fatal("timed-out driver must be excluded for this ride")
	}
}

func TestDuplicateRideRequestedStartsOneSaga(t *testing.T) {
	w := newWorld(t)
	w.driver("d1", 200)
	ride := messaging.NewID()
	env := messaging.New(messaging.RideRequested, ride, "", "", map[string]any{"pickup": pickup})
	env.EventID = messaging.NewID()
	again := env
	again.EventID = messaging.NewID() // re-published under a new id
	for _, e := range []messaging.Envelope{env, env, env, again} {
		if err := w.h.RideEvent(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	w.pump()
	if n := w.count(messaging.CmdStartMatching, ride); n != 1 {
		t.Fatalf("want one StartMatching, got %d", n)
	}
	if n := w.count(messaging.CmdReserveDriver, ride); n != 1 {
		t.Fatalf("want one reservation, got %d", n)
	}
}

func TestDuplicateAcceptIsIdempotent(t *testing.T) {
	w := newWorld(t)
	d1 := w.driver("d1", 200)
	ride := w.requestRide()
	w.pump()
	first, err := w.o.Accept(context.Background(), ride, d1)
	if err != nil || first.Replayed {
		t.Fatal(err)
	}
	second, err := w.o.Accept(context.Background(), ride, d1)
	if err != nil || !second.Replayed || second.Offer.ID != first.Offer.ID || second.Offer.Status != domain.OfferAccepted {
		t.Fatalf("duplicate accept: %+v err=%v", second, err)
	}
	w.pump()
	if w.count(messaging.CmdMarkAccepted, ride) != 1 || w.count(messaging.CmdMarkDriverBusy, ride) != 1 {
		t.Fatal("duplicate accept must not send commands again")
	}
}

func TestOnlyTheOfferedDriverCanRespond(t *testing.T) {
	w := newWorld(t)
	d1, d2 := w.driver("d1", 200), w.driver("d2", 900)
	ride := w.requestRide()
	w.pump()
	if _, err := w.o.Accept(context.Background(), ride, d2); !errors.Is(err, usecase.ErrOfferNotFound) {
		t.Fatalf("another driver accepted: %v", err)
	}
	if _, err := w.o.Reject(context.Background(), ride, d1); err != nil {
		t.Fatal(err)
	}
	if _, err := w.o.Accept(context.Background(), ride, d1); !errors.Is(err, usecase.ErrOfferNotCurrent) {
		t.Fatalf("accept after reject must fail with OFFER_NOT_CURRENT, got %v", err)
	}
}

func TestAcceptRacingTimeoutHasOneWinner(t *testing.T) {
	for i := 0; i < 20; i++ {
		w := newWorld(t)
		d1 := w.driver("d1", 200)
		ride := w.requestRide()
		w.pump()
		_, _ = pool.Exec(context.Background(), `UPDATE offers SET expires_at = now() + interval '30 milliseconds'
			WHERE ride_id=$1 AND status='PENDING'`, ride)
		time.Sleep(25 * time.Millisecond)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = w.o.Accept(context.Background(), ride, d1) }()
		go func() { defer wg.Done(); time.Sleep(5 * time.Millisecond); w.o.SweepExpired(context.Background()) }()
		wg.Wait()
		var status string
		_ = pool.QueryRow(context.Background(), `SELECT status FROM offers WHERE ride_id=$1 AND driver_id=$2`, ride, d1).Scan(&status)
		s := w.saga(ride)
		ok := (status == domain.OfferAccepted && s.Status == domain.Accepted) ||
			(status == domain.OfferExpired && (s.Status == domain.Matching || s.Status == domain.WaitingForDriver)) ||
			(status == domain.OfferPending && s.Status == domain.Offered)
		if !ok {
			t.Fatalf("iteration %d: inconsistent outcome offer=%s saga=%s", i, status, s.Status)
		}
	}
}

// AC-09 at the saga level: two rides want the only driver at the same time.
func TestTwoRidesCannotOfferTheSameDriver(t *testing.T) {
	w := newWorld(t)
	d1 := w.driver("only", 200)
	r1, r2 := w.requestRide(), w.requestRide()
	w.pump()

	var pending int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM offers WHERE driver_id=$1 AND status='PENDING'`, d1).Scan(&pending)
	if pending != 1 {
		t.Fatalf("driver holds %d pending offers, want 1", pending)
	}
	s1, s2 := w.saga(r1), w.saga(r2)
	offered := 0
	for _, s := range []domain.Saga{s1, s2} {
		switch s.Status {
		case domain.Offered:
			offered++
		case domain.WaitingForDriver, domain.Matching:
		default:
			t.Fatalf("unexpected saga status %s", s.Status)
		}
	}
	if offered != 1 {
		t.Fatalf("want exactly one ride offered, got %d (%s, %s)", offered, s1.Status, s2.Status)
	}
}

func TestCancelReleasesTheOfferedDriver(t *testing.T) {
	w := newWorld(t)
	d1 := w.driver("d1", 200)
	ride := w.requestRide()
	w.pump()
	env := messaging.New(messaging.RideCancelled, ride, d1, "", nil)
	env.EventID = messaging.NewID()
	for i := 0; i < 2; i++ { // duplicate delivery
		if err := w.h.RideEvent(context.Background(), env); err != nil {
			t.Fatal(err)
		}
	}
	w.pump()
	if s := w.saga(ride); s.Status != domain.Cancelled {
		t.Fatalf("saga %s, want CANCELLED", s.Status)
	}
	if w.pendingDriver(ride) != "" {
		t.Fatal("pending offer must be cancelled")
	}
	if _, busy := w.holder[d1]; busy || w.count(messaging.CmdReleaseDriver, ride) != 1 {
		t.Fatal("cancel must release the driver exactly once")
	}
	if _, err := w.o.Accept(context.Background(), ride, d1); !errors.Is(err, usecase.ErrOfferNotCurrent) {
		t.Fatalf("accept after cancel must fail, got %v", err)
	}
}

func TestCompletionReleasesTheDriver(t *testing.T) {
	w := newWorld(t)
	d1 := w.driver("d1", 200)
	ride := w.requestRide()
	w.pump()
	if _, err := w.o.Accept(context.Background(), ride, d1); err != nil {
		t.Fatal(err)
	}
	w.pump()
	env := messaging.New(messaging.RideCompleted, ride, d1, "", nil)
	env.EventID = messaging.NewID()
	if err := w.h.RideEvent(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	w.pump()
	if _, busy := w.holder[d1]; busy || w.saga(ride).Status != domain.Completed {
		t.Fatal("completion must release the driver")
	}
}
