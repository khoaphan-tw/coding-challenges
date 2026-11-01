//go:build integration

package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"ridehail/pkg/broker"
	"ridehail/pkg/messaging"
	"ridehail/pkg/pgutil"
	"ridehail/pkg/testpg"
	rhttp "ridehail/services/ride/internal/adapter/http"
	"ridehail/services/ride/internal/adapter/postgres"
	"ridehail/services/ride/internal/domain"
	"ridehail/services/ride/internal/usecase"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) { testpg.Main(m, &pool, postgres.Migrations) }

// wire builds the use cases with the real Postgres adapters (as cmd/main does).
func wire() *usecase.Rides {
	repos := &postgres.Repos{Pool: pool}
	return &usecase.Rides{Tx: pgutil.TxManager{Pool: pool}, Rides: repos, Passengers: repos,
		Idempotency: repos, Publisher: broker.Publisher{}, Inbox: broker.Inbox{Consumer: "ride-service"}}
}

func server() *httptest.Server {
	mux := http.NewServeMux()
	(&rhttp.Handler{Rides: wire(), Ready: pool.Ping}).Routes(mux)
	return httptest.NewServer(mux)
}

const body = `{"passengerId":"P001","pickup":{"latitude":10.7769,"longitude":106.7009},
	"destination":{"latitude":10.8231,"longitude":106.6297}}`

func post(t *testing.T, url, key, b string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/rides", bytes.NewBufferString(b))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func countRides(t *testing.T) int {
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM rides`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateRideStartsRequestedAndCanBeRetrieved(t *testing.T) {
	srv := server()
	defer srv.Close()
	resp, r := post(t, srv.URL, "k-create-"+messaging.NewID(), body)
	if resp.StatusCode != 201 || r["status"] != "REQUESTED" || r["rideId"] == "" {
		t.Fatalf("got %d %v", resp.StatusCode, r)
	}
	get, err := http.Get(srv.URL + "/rides/" + r["rideId"].(string))
	if err != nil || get.StatusCode != 200 {
		t.Fatalf("get ride: %v %v", err, get.StatusCode)
	}
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox WHERE msg_key=$1 AND type=$2`,
		r["rideId"], messaging.RideRequested).Scan(&n)
	if n != 1 {
		t.Fatalf("want one RideRequested event, got %d", n)
	}
}

func TestDuplicateRideCreationReturnsOriginal(t *testing.T) {
	srv := server()
	defer srv.Close()
	key := "k-dup-" + messaging.NewID()
	before := countRides(t)
	_, first := post(t, srv.URL, key, body)
	for i := 0; i < 3; i++ {
		resp, again := post(t, srv.URL, key, body)
		if resp.StatusCode != 201 || again["rideId"] != first["rideId"] || resp.Header.Get("Idempotent-Replayed") != "true" {
			t.Fatalf("replay %d: %d %v", i, resp.StatusCode, again)
		}
	}
	if got := countRides(t) - before; got != 1 {
		t.Fatalf("want 1 ride created, got %d", got)
	}
}

func TestConcurrentDuplicateCreationCreatesOneRide(t *testing.T) {
	srv := server()
	defer srv.Close()
	key := "k-conc-" + messaging.NewID()
	before := countRides(t)
	ids := make([]any, 10)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, r := post(t, srv.URL, key, body)
			ids[i] = r["rideId"]
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] || id == nil {
			t.Fatalf("all responses must carry the same ride: %v", ids)
		}
	}
	if got := countRides(t) - before; got != 1 {
		t.Fatalf("want 1 ride, got %d", got)
	}
}

func TestIdempotencyKeyReusedWithDifferentRequestIsRejected(t *testing.T) {
	srv := server()
	defer srv.Close()
	key := "k-reuse-" + messaging.NewID()
	post(t, srv.URL, key, body)
	other := `{"passengerId":"P002","pickup":{"latitude":10.7769,"longitude":106.7009},
		"destination":{"latitude":10.8231,"longitude":106.6297}}`
	resp, r := post(t, srv.URL, key, other)
	if resp.StatusCode != 422 || r["code"] != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("got %d %v", resp.StatusCode, r)
	}
}

func TestCreateValidation(t *testing.T) {
	srv := server()
	defer srv.Close()
	cases := []struct {
		name, key, body, code string
		status                int
	}{
		{"missing key", "", body, "IDEMPOTENCY_KEY_REQUIRED", 400},
		{"missing passenger", "v1", `{"pickup":{"latitude":1,"longitude":1},"destination":{"latitude":1,"longitude":1}}`, "PASSENGER_REQUIRED", 422},
		{"unknown passenger", "v2", `{"passengerId":"P999","pickup":{"latitude":1,"longitude":1},"destination":{"latitude":1,"longitude":1}}`, "UNKNOWN_PASSENGER", 422},
		{"latitude out of range", "v3", `{"passengerId":"P001","pickup":{"latitude":91,"longitude":1},"destination":{"latitude":1,"longitude":1}}`, "INVALID_COORDINATES", 422},
		{"longitude out of range", "v4", `{"passengerId":"P001","pickup":{"latitude":1,"longitude":1},"destination":{"latitude":1,"longitude":-181}}`, "INVALID_COORDINATES", 422},
		{"missing coordinate", "v5", `{"passengerId":"P001","pickup":{"latitude":1},"destination":{"latitude":1,"longitude":1}}`, "INVALID_COORDINATES", 422},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key := c.key
			if key != "" {
				key += messaging.NewID()
			}
			resp, r := post(t, srv.URL, key, c.body)
			if resp.StatusCode != c.status || r["code"] != c.code {
				t.Fatalf("got %d %v", resp.StatusCode, r)
			}
		})
	}
}

func TestCommandsAreIdempotentAndHistoryIsRecorded(t *testing.T) {
	srv := server()
	defer srv.Close()
	ctx := context.Background()
	s := wire()
	_, r := post(t, srv.URL, "k-cmd-"+messaging.NewID(), body)
	id := r["rideId"].(string)

	start := messaging.New(messaging.CmdStartMatching, id, "", "", nil)
	start.EventID = messaging.NewID()
	assign := messaging.New(messaging.CmdAssignDriver, id, "D001", messaging.NewID(), nil)
	assign.EventID = messaging.NewID()
	for _, env := range []messaging.Envelope{start, start, assign, assign} { // each delivered twice
		if err := s.HandleCommand(ctx, usecase.Command{EventID: env.EventID, Type: env.Type, RideID: env.RideID,
			DriverID: env.DriverID, OfferID: env.OfferID}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.GetRide(ctx, id)
	if got.Status != domain.DriverAssigned || *got.AssignedDriverID != "D001" {
		t.Fatalf("got %+v", got)
	}
	h, _ := s.History(ctx, id)
	if len(h) != 3 { // create, matching, assigned — duplicates add nothing
		t.Fatalf("want 3 history rows, got %d: %+v", len(h), h)
	}
	if _, err := s.Complete(ctx, id, "test"); err == nil {
		t.Fatal("DRIVER_ASSIGNED -> COMPLETED must be rejected")
	}
	if _, err := s.Cancel(ctx, id, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(ctx, id, "test"); err != nil {
		t.Fatal("duplicate cancel must succeed as a no-op")
	}
	// Assign after cancel is rejected with a reply, not an error.
	late := messaging.New(messaging.CmdAssignDriver, id, "D002", messaging.NewID(), nil)
	late.EventID = messaging.NewID()
	if err := s.HandleCommand(ctx, usecase.Command{EventID: late.EventID, Type: late.Type, RideID: late.RideID,
		DriverID: late.DriverID, OfferID: late.OfferID}); err != nil {
		t.Fatal(err)
	}
	var reason string
	_ = pool.QueryRow(ctx, `SELECT envelope->'payload'->>'reason' FROM outbox WHERE type='CommandRejected' AND msg_key=$1`,
		id).Scan(&reason)
	if reason != "RIDE_TERMINAL" {
		t.Fatalf("want RIDE_TERMINAL rejection, got %q", reason)
	}
}
