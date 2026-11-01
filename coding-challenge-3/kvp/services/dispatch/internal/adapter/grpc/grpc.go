// Package grpc is dispatch-service's driving adapter for gRPC (API v2).
//
// It exposes OfferService.WatchOffers: the v2 form of polling
// GET /drivers/{driverId}/offer. The driver app keeps one stream open and
// offers are pushed to it. Inside the service each open stream checks the
// driver's pending offer every OFFER_STREAM_POLL_INTERVAL (a cheap indexed
// read); the network round trips of client polling are gone.
package grpc

import (
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	dispatchv2 "ridehail/api/gen/ridehail/dispatch/v2"
	"ridehail/pkg/obs"
	"ridehail/services/dispatch/internal/domain"
	"ridehail/services/dispatch/internal/usecase"
)

// Server implements dispatchv2.OfferServiceServer.
type Server struct {
	dispatchv2.UnimplementedOfferServiceServer
	Orchestrator *usecase.Orchestrator
	PollInterval time.Duration // how often a stream checks for a new or closed offer
	Heartbeat    time.Duration // idle keep-alive event
}

func ts(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

func present(o domain.Offer) *dispatchv2.Offer {
	return &dispatchv2.Offer{OfferId: o.ID, RideId: o.RideID, DriverId: o.DriverID, Status: o.Status,
		CreatedAt: timestamppb.New(o.CreatedAt), ExpiresAt: timestamppb.New(o.ExpiresAt), RespondedAt: ts(o.RespondedAt)}
}

// WatchOffers pushes OFFERED when a new offer waits for the driver, CLOSED
// when it is no longer pending (with its final status), and HEARTBEAT when idle.
func (s *Server) WatchOffers(req *dispatchv2.WatchOffersRequest, stream dispatchv2.OfferService_WatchOffersServer) error {
	driverID := req.GetDriverId()
	if driverID == "" {
		return status.Error(codes.InvalidArgument, "driver_id is required")
	}
	ctx := obs.With(stream.Context(), obs.DriverID, driverID)
	send := func(typ dispatchv2.OfferEvent_Type, o *dispatchv2.Offer) error {
		return stream.Send(&dispatchv2.OfferEvent{Type: typ, Offer: o, SentAt: timestamppb.Now()})
	}

	var current *domain.Offer // the pending offer the driver was told about
	check := func() error {
		pending, err := s.Orchestrator.PendingOffer(ctx, driverID)
		if err != nil {
			slog.WarnContext(ctx, "offer stream check failed, will retry", "error", err)
			return nil
		}
		if current != nil && (pending == nil || pending.ID != current.ID) {
			closed, err := s.Orchestrator.Offer(ctx, current.ID)
			if err != nil {
				closed = *current
			}
			if err := send(dispatchv2.OfferEvent_CLOSED, present(closed)); err != nil {
				return err
			}
			current = nil
		}
		if pending != nil && current == nil {
			if err := send(dispatchv2.OfferEvent_OFFERED, present(*pending)); err != nil {
				return err
			}
			current = pending
		}
		return nil
	}

	poll := time.NewTicker(s.PollInterval)
	defer poll.Stop()
	heartbeat := time.NewTicker(s.Heartbeat)
	defer heartbeat.Stop()
	if err := check(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil // client closed the stream or the server is stopping
		case <-poll.C:
			if err := check(); err != nil {
				return err
			}
		case <-heartbeat.C:
			if err := send(dispatchv2.OfferEvent_HEARTBEAT, nil); err != nil {
				return err
			}
		}
	}
}
