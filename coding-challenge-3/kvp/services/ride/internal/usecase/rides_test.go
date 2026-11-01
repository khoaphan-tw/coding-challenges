package usecase

import (
	"context"
	"errors"
	"testing"

	"ridehail/pkg/geo"
	"ridehail/pkg/messaging"
	"ridehail/services/ride/internal/domain"
)

// In-memory fakes of the ports: use cases are tested without a database.
// (Concurrency and SQL behaviour are covered by the Postgres integration tests.)

type fakeTx struct{}

func (fakeTx) WithinTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

type fakeRepo struct {
	rides   map[string]domain.Ride
	history []domain.Transition
	keys    map[string]*IdempotencyRecord
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{rides: map[string]domain.Ride{}, keys: map[string]*IdempotencyRecord{}}
}

func (f *fakeRepo) Get(_ context.Context, id string) (domain.Ride, error) {
	r, ok := f.rides[id]
	if !ok {
		return r, ErrRideNotFound
	}
	return r, nil
}
func (f *fakeRepo) GetForUpdate(ctx context.Context, id string) (domain.Ride, error) {
	return f.Get(ctx, id)
}
func (f *fakeRepo) Insert(_ context.Context, r domain.Ride) (domain.Ride, error) {
	f.rides[r.ID] = r
	return r, nil
}
func (f *fakeRepo) Update(_ context.Context, r domain.Ride) (domain.Ride, error) {
	r.Version++
	f.rides[r.ID] = r
	return r, nil
}
func (f *fakeRepo) AddTransition(_ context.Context, t domain.Transition) error {
	f.history = append(f.history, t)
	return nil
}
func (f *fakeRepo) History(context.Context, string) ([]domain.Transition, error) {
	return f.history, nil
}
func (f *fakeRepo) Exists(_ context.Context, id string) (bool, error) { return id == "P001", nil }
func (f *fakeRepo) Reserve(_ context.Context, key, hash, _ string) (*IdempotencyRecord, error) {
	if rec, ok := f.keys[key]; ok {
		return rec, nil
	}
	f.keys[key] = &IdempotencyRecord{RequestHash: hash}
	return nil, nil
}
func (f *fakeRepo) SaveResult(_ context.Context, key string, r domain.Ride) error {
	f.keys[key].Ride = r
	return nil
}

type fakeBus struct{ sent []messaging.Envelope }

func (b *fakeBus) Publish(_ context.Context, _, _ string, env messaging.Envelope) (messaging.Envelope, error) {
	b.sent = append(b.sent, env)
	return env, nil
}

type fakeInbox map[string]bool

func (i fakeInbox) Claim(_ context.Context, id string) (bool, error) {
	if i[id] {
		return false, nil
	}
	i[id] = true
	return true, nil
}

func newRides() (*Rides, *fakeRepo, *fakeBus) {
	repo, bus := newFakeRepo(), &fakeBus{}
	return &Rides{Tx: fakeTx{}, Rides: repo, Passengers: repo, Idempotency: repo, Publisher: bus,
		Inbox: fakeInbox{}}, repo, bus
}

var input = CreateRideInput{IdempotencyKey: "k1", PassengerID: "P001",
	Pickup: geo.Point{Latitude: 10.7769, Longitude: 106.7009}, Destination: geo.Point{Latitude: 10.8231, Longitude: 106.6297}}

func TestCreateRidePublishesRideRequested(t *testing.T) {
	u, repo, bus := newRides()
	out, err := u.CreateRide(context.Background(), input)
	if err != nil || out.Replayed || out.Ride.Status != domain.Requested {
		t.Fatalf("got %+v %v", out, err)
	}
	if len(repo.rides) != 1 || len(bus.sent) != 1 || bus.sent[0].Type != messaging.RideRequested {
		t.Fatalf("want 1 ride and 1 RideRequested, got %d rides, %+v", len(repo.rides), bus.sent)
	}
}

func TestCreateRideIdempotency(t *testing.T) {
	u, repo, _ := newRides()
	first, _ := u.CreateRide(context.Background(), input)
	again, err := u.CreateRide(context.Background(), input)
	if err != nil || !again.Replayed || again.Ride.ID != first.Ride.ID || len(repo.rides) != 1 {
		t.Fatalf("replay: %+v %v (rides=%d)", again, err, len(repo.rides))
	}
	other := input
	other.Destination.Latitude = 10.9
	if _, err := u.CreateRide(context.Background(), other); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("same key, different request: want ErrKeyReused, got %v", err)
	}
}

func TestCreateRideValidation(t *testing.T) {
	u, _, _ := newRides()
	bad := input
	bad.IdempotencyKey, bad.PassengerID = "k2", "P999"
	if _, err := u.CreateRide(context.Background(), bad); !errors.Is(err, ErrUnknownPassenger) {
		t.Fatalf("want ErrUnknownPassenger, got %v", err)
	}
	bad = input
	bad.IdempotencyKey, bad.Pickup.Latitude = "k3", 91
	if _, err := u.CreateRide(context.Background(), bad); !errors.Is(err, geo.ErrInvalidCoordinates) {
		t.Fatalf("want ErrInvalidCoordinates, got %v", err)
	}
}

func TestDuplicateCommandIsAppliedAndRepliedOnce(t *testing.T) {
	u, repo, bus := newRides()
	out, _ := u.CreateRide(context.Background(), input)
	cmd := Command{EventID: "e1", Type: messaging.CmdStartMatching, RideID: out.Ride.ID}
	for i := 0; i < 3; i++ {
		if err := u.HandleCommand(context.Background(), cmd); err != nil {
			t.Fatal(err)
		}
	}
	if repo.rides[out.Ride.ID].Status != domain.Matching {
		t.Fatalf("status %s", repo.rides[out.Ride.ID].Status)
	}
	replies := 0
	for _, e := range bus.sent {
		if e.Type == messaging.ReplyCommandSucceeded {
			replies++
		}
	}
	if replies != 1 {
		t.Fatalf("want 1 reply, got %d", replies)
	}
}

func TestCommandOnTerminalRideIsRejectedWithReply(t *testing.T) {
	u, _, bus := newRides()
	out, _ := u.CreateRide(context.Background(), input)
	if _, err := u.Cancel(context.Background(), out.Ride.ID, "test"); err != nil {
		t.Fatal(err)
	}
	err := u.HandleCommand(context.Background(), Command{EventID: "e2", Type: messaging.CmdAssignDriver,
		RideID: out.Ride.ID, DriverID: "D1", OfferID: "O1"})
	if err != nil {
		t.Fatal(err)
	}
	last := bus.sent[len(bus.sent)-1]
	var res messaging.CommandResult
	_ = last.Decode(&res)
	if last.Type != messaging.ReplyCommandRejected || res.Reason != "RIDE_TERMINAL" {
		t.Fatalf("want CommandRejected RIDE_TERMINAL, got %s %+v", last.Type, res)
	}
}
