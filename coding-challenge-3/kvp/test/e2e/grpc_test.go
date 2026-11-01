//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	dispatchv2 "ridehail/api/gen/ridehail/dispatch/v2"
	driverv2 "ridehail/api/gen/ridehail/driver/v2"
)

var grpcGateway = env("E2E_GRPC_GATEWAY", "localhost:9090")

func grpcConn(t *testing.T) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(grpcGateway, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func f64(v float64) *float64 { return &v }

// API v2: location updates over one stream are applied like PUT /drivers/{id}/location.
func TestGRPCStreamLocationsUpdatesDriver(t *testing.T) {
	a := newArea()
	id := newDriver(t, a, 100)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := driverv2.NewDriverLocationServiceClient(grpcConn(t)).StreamLocations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := a.at(300)
	updates := []*driverv2.LocationUpdate{
		{DriverId: id, Latitude: f64(p.Latitude), Longitude: f64(p.Longitude), Seq: 1},
		{DriverId: id, Latitude: f64(200), Longitude: f64(p.Longitude), Seq: 2}, // invalid
		{DriverId: "E2E-NOPE", Latitude: f64(p.Latitude), Longitude: f64(p.Longitude), Seq: 3},
	}
	var acks []*driverv2.LocationAck
	for _, u := range updates {
		if err := stream.Send(u); err != nil {
			t.Fatal(err)
		}
		ack, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		acks = append(acks, ack)
	}
	_ = stream.CloseSend()

	if !acks[0].Accepted || acks[0].Seq != 1 || acks[0].Version < 2 {
		t.Fatalf("valid update not accepted: %v", acks[0])
	}
	if acks[1].Accepted || acks[1].ErrorCode != "INVALID_COORDINATES" {
		t.Fatalf("invalid update: %v", acks[1])
	}
	if acks[2].Accepted || acks[2].ErrorCode != "DRIVER_NOT_FOUND" {
		t.Fatalf("unknown driver: %v", acks[2])
	}
	_, d, _ := call(t, "GET", gateway+"/drivers/"+id, nil)
	if d["latitude"].(float64) != p.Latitude {
		t.Fatalf("REST view not updated: %v", d)
	}
}

// API v2: an offer is pushed to the driver's stream, then closed when accepted.
func TestGRPCWatchOffersPushesOfferAndClose(t *testing.T) {
	a := newArea()
	id := newDriver(t, a, 100)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := dispatchv2.NewOfferServiceClient(grpcConn(t)).WatchOffers(ctx, &dispatchv2.WatchOffersRequest{DriverId: id})
	if err != nil {
		t.Fatal(err)
	}
	next := func(want dispatchv2.OfferEvent_Type) *dispatchv2.Offer {
		t.Helper()
		for {
			ev, err := stream.Recv()
			if err != nil {
				t.Fatalf("waiting for %s: %v", want, err)
			}
			if ev.Type == dispatchv2.OfferEvent_HEARTBEAT {
				continue
			}
			if ev.Type != want {
				t.Fatalf("got %s, want %s: %v", ev.Type, want, ev)
			}
			return ev.Offer
		}
	}

	rideID := requestRide(t, a, "e2e-grpc-"+id)
	offered := next(dispatchv2.OfferEvent_OFFERED)
	if offered.RideId != rideID || offered.DriverId != id || offered.Status != "PENDING" {
		t.Fatalf("offer: %v", offered)
	}
	if code, body, _ := call(t, "POST", gateway+"/rides/"+rideID+"/offers/"+id+"/accept", nil); code != 200 {
		t.Fatalf("accept: %d %v", code, body)
	}
	closed := next(dispatchv2.OfferEvent_CLOSED)
	if closed.OfferId != offered.OfferId || closed.Status != "ACCEPTED" {
		t.Fatalf("closed: %v", closed)
	}
}

func TestGRPCWatchOffersRequiresDriverID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := dispatchv2.NewOfferServiceClient(grpcConn(t)).WatchOffers(ctx, &dispatchv2.WatchOffersRequest{})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}
