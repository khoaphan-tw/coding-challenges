//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"ridehail/pkg/broker"
	"ridehail/pkg/geo"
	"ridehail/pkg/messaging"
	"ridehail/pkg/pgutil"
	"ridehail/pkg/testpg"
	"ridehail/services/driver/internal/adapter/postgres"
	"ridehail/services/driver/internal/domain"
	"ridehail/services/driver/internal/usecase"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) { testpg.Main(m, &pool, postgres.Migrations) }

var hcm = geo.Point{Latitude: 10.7769, Longitude: 106.7009}

// wire builds the use cases with the real Postgres adapter (as cmd/main does).
func wire() (*usecase.Drivers, *postgres.Drivers) {
	repo := &postgres.Drivers{Pool: pool}
	return (&usecase.Drivers{Tx: pgutil.TxManager{Pool: pool}, Drivers: repo,
		Publisher: broker.Publisher{}, Inbox: broker.Inbox{Consumer: "driver-service"}}).Init(), repo
}

func newDriver(t *testing.T, u *usecase.Drivers, status domain.Status) string {
	t.Helper()
	id := "T-" + messaging.NewID()
	if _, err := u.Upsert(context.Background(), id, status, hcm); err != nil {
		t.Fatal(err)
	}
	return id
}

func cmd(typ, driverID, rideID string) usecase.Command {
	return usecase.Command{EventID: messaging.NewID(), Type: typ, DriverID: driverID, RideID: rideID, OfferID: messaging.NewID()}
}

// lastReply returns the type of the latest reply sent for a driver.
func lastReply(t *testing.T, driverID string) string {
	t.Helper()
	var typ string
	if err := pool.QueryRow(context.Background(), `SELECT type FROM outbox WHERE topic=$1 AND envelope->>'driverId'=$2
		ORDER BY id DESC LIMIT 1`, messaging.TopicDispatchReplies, driverID).Scan(&typ); err != nil {
		t.Fatal(err)
	}
	return typ
}

// AC-09: two rides race for the same driver; the atomic compare-and-set lets exactly one win.
func TestConcurrentReservationOnlyOneWins(t *testing.T) {
	u, repo := wire()
	tx := pgutil.TxManager{Pool: pool}
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		id := newDriver(t, u, domain.Available)
		rides := []string{messaging.NewID(), messaging.NewID()}
		var wg sync.WaitGroup
		start := make(chan struct{})
		applied := make([]bool, 2)
		errs := make([]error, 2)
		for k := range rides {
			wg.Add(1)
			go func(k int) {
				defer wg.Done()
				<-start
				errs[k] = tx.WithinTx(ctx, func(ctx context.Context) error {
					var err error
					_, applied[k], err = repo.ReserveIfAvailable(ctx, id, rides[k])
					return err
				})
			}(k)
		}
		close(start)
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("iteration %d: %v", i, errors.Join(errs...))
		}
		if applied[0] == applied[1] {
			t.Fatalf("iteration %d: applied=%v, want exactly one winner", i, applied)
		}
		d, _ := repo.Get(ctx, id)
		if d.Status != domain.Offered || d.CurrentRideID == nil {
			t.Fatalf("driver should be OFFERED for one ride, got %+v", d)
		}
	}
}

// The same race through the use case: the loser's saga is told DriverReservationFailed.
func TestSecondReservationIsRejectedWithReply(t *testing.T) {
	u, _ := wire()
	ctx := context.Background()
	id := newDriver(t, u, domain.Available)
	if err := u.HandleCommand(ctx, cmd(messaging.CmdReserveDriver, id, messaging.NewID())); err != nil {
		t.Fatal(err)
	}
	if lastReply(t, id) != messaging.ReplyDriverReserved {
		t.Fatal("first reservation must succeed")
	}
	if err := u.HandleCommand(ctx, cmd(messaging.CmdReserveDriver, id, messaging.NewID())); err != nil {
		t.Fatal(err)
	}
	if lastReply(t, id) != messaging.ReplyDriverReservationFailed {
		t.Fatal("second reservation for another ride must fail")
	}
}

func TestDriverStatusLifecycle(t *testing.T) {
	u, _ := wire()
	ctx := context.Background()
	id := newDriver(t, u, domain.Offline)

	if d, err := u.SetAvailability(ctx, id, domain.Available); err != nil || d.Status != domain.Available {
		t.Fatalf("OFFLINE -> AVAILABLE: %+v %v", d, err)
	}
	ride := messaging.NewID()
	if err := u.HandleCommand(ctx, cmd(messaging.CmdReserveDriver, id, ride)); err != nil {
		t.Fatal(err)
	}
	d, _ := u.Get(ctx, id)
	if d.Status != domain.Offered {
		t.Fatalf("AVAILABLE -> OFFERED: got %s", d.Status)
	}
	if _, err := u.SetAvailability(ctx, id, domain.Offline); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("OFFERED driver must not go OFFLINE: %v", err)
	}
	if err := u.HandleCommand(ctx, cmd(messaging.CmdReserveDriver, id, ride)); err != nil || lastReply(t, id) != messaging.ReplyDriverReserved {
		t.Fatalf("duplicate reserve for the same ride must succeed as a no-op: %v", err)
	}

	if err := u.HandleCommand(ctx, cmd(messaging.CmdMarkDriverBusy, id, ride)); err != nil {
		t.Fatal(err)
	}
	if d, _ = u.Get(ctx, id); d.Status != domain.Busy {
		t.Fatalf("OFFERED -> BUSY: got %s", d.Status)
	}
	if _, err := u.SetAvailability(ctx, id, domain.Offline); err == nil {
		t.Fatal("BUSY driver must not go OFFLINE")
	}
	// A release for another ride never frees the driver.
	if err := u.HandleCommand(ctx, cmd(messaging.CmdReleaseDriver, id, messaging.NewID())); err != nil {
		t.Fatal(err)
	}
	if d, _ = u.Get(ctx, id); d.Status != domain.Busy {
		t.Fatalf("stale release freed the driver: %+v", d)
	}
	if err := u.HandleCommand(ctx, cmd(messaging.CmdReleaseDriver, id, ride)); err != nil {
		t.Fatal(err)
	}
	if d, _ = u.Get(ctx, id); d.Status != domain.Available || d.CurrentRideID != nil {
		t.Fatalf("BUSY -> AVAILABLE on completion: %+v", d)
	}
}

func TestRejectBeforeAcceptReturnsAvailable(t *testing.T) {
	u, _ := wire()
	ctx := context.Background()
	id := newDriver(t, u, domain.Available)
	ride := messaging.NewID()
	_ = u.HandleCommand(ctx, cmd(messaging.CmdReserveDriver, id, ride))
	_ = u.HandleCommand(ctx, cmd(messaging.CmdReleaseDriver, id, ride))
	if d, _ := u.Get(ctx, id); d.Status != domain.Available {
		t.Fatalf("OFFERED -> AVAILABLE: got %s", d.Status)
	}
}

func TestEveryChangeEmitsVersionedEvent(t *testing.T) {
	u, _ := wire()
	ctx := context.Background()
	id := newDriver(t, u, domain.Available)
	if _, err := u.UpdateLocation(ctx, id, geo.Point{Latitude: 10.78, Longitude: 106.70}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE msg_key=$1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("want 2 events (status + location), got %d", n)
	}
}

func TestDuplicateCommandIsProcessedOnce(t *testing.T) {
	u, _ := wire()
	ctx := context.Background()
	id := newDriver(t, u, domain.Available)
	c := cmd(messaging.CmdReserveDriver, id, messaging.NewID())
	for i := 0; i < 3; i++ {
		if err := u.HandleCommand(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	var replies int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE topic=$1 AND envelope->>'driverId'=$2`,
		messaging.TopicDispatchReplies, id).Scan(&replies); err != nil {
		t.Fatal(err)
	}
	if replies != 1 {
		t.Fatalf("duplicate deliveries must produce one reply, got %d", replies)
	}
}
