//go:build e2e

// Package e2e runs black-box tests against the running docker compose stack:
//
//	make up && make test-e2e
//
// Each test places its own drivers at a random, isolated spot on the map, so
// tests do not interfere with each other or with the seeded drivers.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

var (
	gateway = env("E2E_GATEWAY", "http://localhost:8080")
	drivers = env("E2E_DRIVER_SERVICE", "http://localhost:8082") // fixtures only (/internal)
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

type point struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

type area struct{ center point }

// newArea returns a random place far from everything else.
func newArea() area {
	return area{point{Latitude: -60 + rand.Float64()*120, Longitude: -170 + rand.Float64()*340}}
}

// at returns a point ~metres north of the area centre.
func (a area) at(metres float64) point {
	return point{Latitude: a.center.Latitude + metres/111195.0, Longitude: a.center.Longitude}
}

func call(t *testing.T, method, url string, body any, headers ...string) (int, map[string]any, http.Header) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, r)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, resp.Header
}

func newDriver(t *testing.T, a area, metres float64) string {
	t.Helper()
	id := "E2E-" + uuid.NewString()[:8]
	p := a.at(metres)
	code, body, _ := call(t, "PUT", drivers+"/internal/drivers/"+id,
		map[string]any{"status": "AVAILABLE", "latitude": p.Latitude, "longitude": p.Longitude})
	if code != 200 {
		t.Fatalf("create driver: %d %v", code, body)
	}
	return id
}

func requestRide(t *testing.T, a area, key string) string {
	t.Helper()
	code, body, _ := call(t, "POST", gateway+"/rides", map[string]any{
		"passengerId": "P001", "pickup": a.center, "destination": a.at(5000),
	}, "Idempotency-Key", key)
	if code != 201 {
		t.Fatalf("create ride: %d %v", code, body)
	}
	return body["rideId"].(string)
}

func ride(t *testing.T, id string) map[string]any {
	_, body, _ := call(t, "GET", gateway+"/rides/"+id, nil)
	return body
}

func driverStatus(t *testing.T, id string) string {
	_, body, _ := call(t, "GET", gateway+"/drivers/"+id, nil)
	s, _ := body["status"].(string)
	return s
}

// eventually polls cond until it returns "" or the timeout passes.
func eventually(t *testing.T, timeout time.Duration, cond func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		if last = cond(); last == "" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, last)
}

func rideIs(t *testing.T, id, status, driver string) func() string {
	return func() string {
		r := ride(t, id)
		d, _ := r["assignedDriverId"].(string)
		if r["status"] == status && d == driver {
			return ""
		}
		return fmt.Sprintf("ride %v/%v, want %s/%s", r["status"], r["assignedDriverId"], status, driver)
	}
}

func driverIs(t *testing.T, id, status string) func() string {
	return func() string {
		if s := driverStatus(t, id); s != status {
			return fmt.Sprintf("driver %s is %s, want %s", id, s, status)
		}
		return ""
	}
}

func offer(t *testing.T, ride, driver, action string) (int, map[string]any, http.Header) {
	return call(t, "POST", gateway+"/rides/"+ride+"/offers/"+driver+"/"+action, nil)
}

const wait = 10 * time.Second

func TestHappyPath(t *testing.T) {
	a := newArea()
	near := newDriver(t, a, 300)
	newDriver(t, a, 1500)
	newDriver(t, a, 3200) // outside 2 km
	id := requestRide(t, a, uuid.NewString())

	eventually(t, wait, rideIs(t, id, "DRIVER_ASSIGNED", near))
	eventually(t, wait, driverIs(t, near, "OFFERED"))

	_, off, _ := call(t, "GET", gateway+"/drivers/"+near+"/offer", nil)
	if off["rideId"] != id {
		t.Fatalf("driver should see the offer: %v", off)
	}
	if code, body, _ := offer(t, id, near, "accept"); code != 200 || body["status"] != "ACCEPTED" {
		t.Fatalf("accept: %d %v", code, body)
	}
	eventually(t, wait, rideIs(t, id, "DRIVER_ACCEPTED", near))
	eventually(t, wait, driverIs(t, near, "BUSY"))

	if code, body, _ := call(t, "POST", gateway+"/rides/"+id+"/complete", nil); code != 200 || body["status"] != "COMPLETED" {
		t.Fatalf("complete: %d %v", code, body)
	}
	eventually(t, wait, driverIs(t, near, "AVAILABLE"))

	// Terminal: no way back.
	if code, _, _ := call(t, "POST", gateway+"/rides/"+id+"/cancel", nil); code != 409 {
		t.Fatalf("cancel after complete: want 409, got %d", code)
	}
}

func TestRejectRematchesToNextDriver(t *testing.T) {
	a := newArea()
	d1, d2 := newDriver(t, a, 200), newDriver(t, a, 900)
	id := requestRide(t, a, uuid.NewString())
	eventually(t, wait, rideIs(t, id, "DRIVER_ASSIGNED", d1))

	if code, _, _ := offer(t, id, d1, "reject"); code != 200 {
		t.Fatalf("reject: %d", code)
	}
	if code, _, h := offer(t, id, d1, "reject"); code != 200 || h.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("duplicate reject: %d", code)
	}
	eventually(t, wait, rideIs(t, id, "DRIVER_ASSIGNED", d2))
	eventually(t, wait, driverIs(t, d1, "AVAILABLE"))

	// d2 rejects too: d1 is available and nearest but excluded for this ride.
	offer(t, id, d2, "reject")
	eventually(t, wait, rideIs(t, id, "MATCHING", ""))
	time.Sleep(time.Second)
	if r := ride(t, id); r["status"] != "MATCHING" || r["assignedDriverId"] != nil {
		t.Fatalf("ride must stay MATCHING with no driver: %v", r)
	}
	_, view, _ := call(t, "GET", gateway+"/rides/"+id+"/dispatch", nil)
	if ex := view["exclusions"].(map[string]any); ex[d1] == nil || ex[d2] == nil {
		t.Fatalf("both drivers must be excluded: %v", ex)
	}
}

func TestDuplicateRideCreation(t *testing.T) {
	a := newArea()
	key := uuid.NewString()
	first := requestRide(t, a, key)
	for i := 0; i < 3; i++ {
		_, body, h := call(t, "POST", gateway+"/rides", map[string]any{
			"passengerId": "P001", "pickup": a.center, "destination": a.at(5000),
		}, "Idempotency-Key", key)
		if body["rideId"] != first || h.Get("Idempotent-Replayed") != "true" {
			t.Fatalf("replay %d returned %v", i, body)
		}
	}
	code, body, _ := call(t, "POST", gateway+"/rides", map[string]any{
		"passengerId": "P002", "pickup": a.center, "destination": a.at(5000),
	}, "Idempotency-Key", key)
	if code != 422 || body["code"] != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("reuse with another body: %d %v", code, body)
	}
}

func TestDuplicateAcceptKeepsValidState(t *testing.T) {
	a := newArea()
	d := newDriver(t, a, 100)
	id := requestRide(t, a, uuid.NewString())
	eventually(t, wait, rideIs(t, id, "DRIVER_ASSIGNED", d))
	var wg sync.WaitGroup
	codes := make([]int, 5)
	for i := range codes {
		wg.Add(1)
		go func(i int) { defer wg.Done(); codes[i], _, _ = offer(t, id, d, "accept") }(i)
	}
	wg.Wait()
	for _, c := range codes {
		if c != 200 {
			t.Fatalf("all duplicate accepts must return 200, got %v", codes)
		}
	}
	eventually(t, wait, rideIs(t, id, "DRIVER_ACCEPTED", d))
	eventually(t, wait, driverIs(t, d, "BUSY"))
}

// AC-09: two rides compete for the only driver.
func TestConcurrentRidesCannotShareADriver(t *testing.T) {
	a := newArea()
	d := newDriver(t, a, 150)
	ids := make([]string, 2)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) { defer wg.Done(); ids[i] = requestRide(t, a, uuid.NewString()) }(i)
	}
	wg.Wait()

	var winner, loser string
	eventually(t, wait, func() string {
		r0, r1 := ride(t, ids[0]), ride(t, ids[1])
		switch {
		case r0["status"] == "DRIVER_ASSIGNED" && r1["status"] == "MATCHING":
			winner, loser = ids[0], ids[1]
		case r1["status"] == "DRIVER_ASSIGNED" && r0["status"] == "MATCHING":
			winner, loser = ids[1], ids[0]
		default:
			return fmt.Sprintf("rides %v / %v", r0["status"], r1["status"])
		}
		return ""
	})
	time.Sleep(time.Second) // give a wrong double-assignment time to show up
	if r := ride(t, loser); r["status"] != "MATCHING" || r["assignedDriverId"] != nil {
		t.Fatalf("loser must stay MATCHING with no driver: %v", r)
	}
	_, off, _ := call(t, "GET", gateway+"/drivers/"+d+"/offer", nil)
	if off["rideId"] != winner {
		t.Fatalf("driver must hold exactly the winner's offer: %v", off)
	}

	// When the winner's passenger cancels, the driver is freed and the waiting ride gets them.
	call(t, "POST", gateway+"/rides/"+winner+"/cancel", nil)
	eventually(t, 15*time.Second, rideIs(t, loser, "DRIVER_ASSIGNED", d))
}

func TestCancelReleasesOfferedDriver(t *testing.T) {
	a := newArea()
	d := newDriver(t, a, 100)
	id := requestRide(t, a, uuid.NewString())
	// DRIVER_ASSIGNED means the offer exists (the driver is OFFERED slightly earlier).
	eventually(t, wait, rideIs(t, id, "DRIVER_ASSIGNED", d))
	eventually(t, wait, driverIs(t, d, "OFFERED"))
	if code, _, _ := call(t, "PUT", gateway+"/drivers/"+d+"/availability", map[string]any{"status": "OFFLINE"}); code != 409 {
		t.Fatalf("OFFERED driver going OFFLINE: want 409, got %d", code)
	}
	if code, body, _ := call(t, "POST", gateway+"/rides/"+id+"/cancel", nil); code != 200 || body["status"] != "CANCELLED" {
		t.Fatalf("cancel: %d %v", code, body)
	}
	eventually(t, wait, driverIs(t, d, "AVAILABLE"))
	if code, _, _ := offer(t, id, d, "accept"); code != 409 {
		t.Fatalf("accept after cancel: want 409, got %d", code)
	}
}

func TestInvalidRequests(t *testing.T) {
	a := newArea()
	id := requestRide(t, a, uuid.NewString()) // no drivers here: stays MATCHING
	eventually(t, wait, rideIs(t, id, "MATCHING", ""))
	if code, body, _ := call(t, "POST", gateway+"/rides/"+id+"/complete", nil); code != 409 || body["code"] != "INVALID_TRANSITION" {
		t.Fatalf("complete while MATCHING: %d %v", code, body)
	}
	if code, _, _ := call(t, "PUT", gateway+"/drivers/D001/location", map[string]any{"latitude": 95, "longitude": 1}); code != 422 {
		t.Fatalf("invalid location: want 422, got %d", code)
	}
	if code, _, _ := call(t, "GET", gateway+"/internal/drivers", nil); code != 404 {
		t.Fatalf("internal endpoints must not be public, got %d", code)
	}
}

func TestOfferTimeoutRematches(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the real offer timeout (30 s by default)")
	}
	a := newArea()
	d1, d2 := newDriver(t, a, 100), newDriver(t, a, 600)
	id := requestRide(t, a, uuid.NewString())
	eventually(t, wait, rideIs(t, id, "DRIVER_ASSIGNED", d1))
	// Nobody answers: after the timeout d1 is released and d2 gets the ride.
	eventually(t, 45*time.Second, rideIs(t, id, "DRIVER_ASSIGNED", d2))
	eventually(t, wait, driverIs(t, d1, "AVAILABLE"))
	if code, _, _ := offer(t, id, d1, "accept"); code != 409 {
		t.Fatalf("accept after timeout: want 409, got %d", code)
	}
}
