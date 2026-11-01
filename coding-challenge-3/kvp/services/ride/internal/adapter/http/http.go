// Package http is ride-service's driving adapter for HTTP: it decodes
// requests, calls the use cases and presents results as JSON or RFC 9457
// problems. No business rules live here.
package http

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"

	"ridehail/pkg/geo"
	"ridehail/pkg/httpx"
	"ridehail/pkg/obs"
	"ridehail/services/ride/internal/domain"
	"ridehail/services/ride/internal/usecase"
)

// Handler exposes the ride use cases over HTTP.
type Handler struct {
	Rides *usecase.Rides
	Ready func(context.Context) error
}

// RideResponse is the public JSON shape of a ride.
type RideResponse struct {
	RideID           string    `json:"rideId"`
	PassengerID      string    `json:"passengerId"`
	Pickup           geo.Point `json:"pickup"`
	Destination      geo.Point `json:"destination"`
	Status           string    `json:"status"`
	AssignedDriverID *string   `json:"assignedDriverId"`
	CurrentOfferID   *string   `json:"currentOfferId,omitempty"`
	Version          int64     `json:"version"`
	TraceID          string    `json:"traceId,omitempty"` // the ride's whole journey in Tempo / logs
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

func present(r domain.Ride) RideResponse {
	return RideResponse{RideID: r.ID, PassengerID: r.PassengerID, Pickup: r.Pickup, Destination: r.Destination,
		Status: string(r.Status), AssignedDriverID: r.AssignedDriverID, CurrentOfferID: r.CurrentOfferID,
		Version: r.Version, TraceID: obs.TraceID(r.TraceParent), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

// TransitionResponse is one history row.
type TransitionResponse struct {
	From     *string `json:"from"`
	To       string  `json:"to"`
	Action   string  `json:"action"`
	DriverID string  `json:"driverId,omitempty"`
	OfferID  string  `json:"offerId,omitempty"`
	Cause    string  `json:"cause,omitempty"`
	At       string  `json:"at"`
}

// toProblem maps use-case and domain errors to HTTP problems.
func toProblem(err error) error {
	switch {
	case errors.Is(err, usecase.ErrRideNotFound):
		return httpx.NewError(404, "RIDE_NOT_FOUND", "ride not found")
	case errors.Is(err, domain.ErrInvalidTransition):
		return httpx.NewError(409, "INVALID_TRANSITION", "transition not allowed from the current status")
	case errors.Is(err, usecase.ErrUnknownPassenger):
		return httpx.NewError(422, "UNKNOWN_PASSENGER", err.Error())
	case errors.Is(err, usecase.ErrKeyReused):
		return httpx.NewError(422, "IDEMPOTENCY_KEY_REUSED", err.Error())
	case errors.Is(err, domain.ErrPassengerRequired):
		return httpx.NewError(422, "PASSENGER_REQUIRED", err.Error())
	case errors.Is(err, geo.ErrInvalidCoordinates):
		return httpx.NewError(422, "INVALID_COORDINATES",
			"pickup and destination need latitude in [-90, 90] and longitude in [-180, 180]")
	}
	return err
}

type createRideRequest struct {
	PassengerID string          `json:"passengerId"`
	Pickup      *geo.PointInput `json:"pickup"`
	Destination *geo.PointInput `json:"destination"`
}

// Routes registers ride-service endpoints.
func (h *Handler) Routes(mux *http.ServeMux) {
	created, _ := otel.Meter("ride-service").Int64Counter("rides_created_total")

	mux.HandleFunc("POST /rides", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" || len(key) > 200 {
			httpx.WriteError(ctx, w, httpx.NewError(400, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key header is required (max 200 chars)"))
			return
		}
		var req createRideRequest
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(ctx, w, err)
			return
		}
		if strings.TrimSpace(req.PassengerID) == "" {
			httpx.WriteError(ctx, w, toProblem(domain.ErrPassengerRequired))
			return
		}
		pickup, err1 := req.Pickup.ToPoint()
		dest, err2 := req.Destination.ToPoint()
		if err := errors.Join(err1, err2); err != nil {
			httpx.WriteError(ctx, w, toProblem(geo.ErrInvalidCoordinates))
			return
		}
		out, err := h.Rides.CreateRide(ctx, usecase.CreateRideInput{
			IdempotencyKey: key, PassengerID: req.PassengerID, Pickup: pickup, Destination: dest})
		if err != nil {
			httpx.WriteError(ctx, w, toProblem(err))
			return
		}
		if out.Replayed {
			w.Header().Set("Idempotent-Replayed", "true")
		} else {
			created.Add(ctx, 1)
		}
		httpx.WriteJSON(w, http.StatusCreated, present(out.Ride))
	})

	mux.HandleFunc("GET /rides/{rideId}", func(w http.ResponseWriter, r *http.Request) {
		rd, err := h.Rides.GetRide(r.Context(), r.PathValue("rideId"))
		if err != nil {
			httpx.WriteError(r.Context(), w, toProblem(err))
			return
		}
		httpx.WriteJSON(w, 200, present(rd))
	})

	mux.HandleFunc("GET /rides/{rideId}/history", func(w http.ResponseWriter, r *http.Request) {
		hist, err := h.Rides.History(r.Context(), r.PathValue("rideId"))
		if err != nil {
			httpx.WriteError(r.Context(), w, toProblem(err))
			return
		}
		out := make([]TransitionResponse, 0, len(hist))
		for _, t := range hist {
			tr := TransitionResponse{To: string(t.To), Action: t.Action, DriverID: t.DriverID, OfferID: t.OfferID,
				Cause: t.Cause, At: t.At.UTC().Format("2006-01-02T15:04:05.000Z")}
			if t.From != nil {
				f := string(*t.From)
				tr.From = &f
			}
			out = append(out, tr)
		}
		httpx.WriteJSON(w, 200, map[string]any{"rideId": r.PathValue("rideId"), "transitions": out})
	})

	lifecycle := func(action func(context.Context, string, string) (domain.Ride, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("rideId")
			ctx := obs.With(r.Context(), obs.RideID, id)
			rd, err := action(ctx, id, "http:"+r.Method+" "+r.URL.Path)
			if err != nil {
				httpx.WriteError(ctx, w, toProblem(err))
				return
			}
			httpx.WriteJSON(w, 200, present(rd))
		}
	}
	mux.HandleFunc("POST /rides/{rideId}/cancel", lifecycle(h.Rides.Cancel))
	mux.HandleFunc("POST /rides/{rideId}/complete", lifecycle(h.Rides.Complete))

	httpx.Health(mux, h.Ready)
}
