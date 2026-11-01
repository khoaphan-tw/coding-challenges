// Package grpc is driver-service's driving adapter for gRPC (API v2).
//
// It exposes DriverLocationService.StreamLocations: the v2 form of
// PUT /drivers/{driverId}/location. Each update calls the same use case as
// the REST endpoint, so validation, versioning and DriverLocationUpdated
// events are identical.
package grpc

import (
	"context"
	"errors"
	"io"

	"google.golang.org/protobuf/types/known/timestamppb"

	driverv2 "ridehail/api/gen/ridehail/driver/v2"
	"ridehail/pkg/geo"
	"ridehail/pkg/obs"
	"ridehail/services/driver/internal/usecase"
)

// Server implements driverv2.DriverLocationServiceServer.
type Server struct {
	driverv2.UnimplementedDriverLocationServiceServer
	Drivers *usecase.Drivers
}

// StreamLocations applies each update and answers it with an ack. Rejected
// updates are reported in the ack; the stream stays open.
func (s *Server) StreamLocations(stream driverv2.DriverLocationService_StreamLocationsServer) error {
	for {
		upd, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(s.apply(stream.Context(), upd)); err != nil {
			return err
		}
	}
}

func (s *Server) apply(ctx context.Context, upd *driverv2.LocationUpdate) *driverv2.LocationAck {
	ack := &driverv2.LocationAck{Seq: upd.GetSeq(), DriverId: upd.GetDriverId()}
	reject := func(code, detail string) *driverv2.LocationAck {
		ack.ErrorCode, ack.ErrorDetail = code, detail
		return ack
	}
	if upd.GetDriverId() == "" {
		return reject("INVALID_REQUEST", "driver_id is required")
	}
	p, err := (&geo.PointInput{Latitude: upd.Latitude, Longitude: upd.Longitude}).ToPoint()
	if err != nil {
		return reject("INVALID_COORDINATES", "latitude must be in [-90, 90] and longitude in [-180, 180]")
	}
	d, err := s.Drivers.UpdateLocation(obs.With(ctx, obs.DriverID, upd.GetDriverId()), upd.GetDriverId(), p)
	switch {
	case errors.Is(err, usecase.ErrDriverNotFound):
		return reject("DRIVER_NOT_FOUND", "driver not found")
	case err != nil:
		return reject("INTERNAL", "location update failed, retry")
	}
	ack.Accepted, ack.Version, ack.Status = true, d.Version, string(d.Status)
	ack.Latitude, ack.Longitude = d.Latitude, d.Longitude
	if d.LocationUpdatedAt != nil {
		ack.LocationUpdatedAt = timestamppb.New(*d.LocationUpdatedAt)
	}
	return ack
}
