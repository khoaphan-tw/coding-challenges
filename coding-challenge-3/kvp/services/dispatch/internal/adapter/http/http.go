// Package http is dispatch-service's driving adapter for HTTP: offer
// responses, the dispatch view and the driver's pending offer.
package http

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"ridehail/pkg/geo"
	"ridehail/pkg/httpx"
	"ridehail/pkg/obs"
	"ridehail/services/dispatch/internal/domain"
	"ridehail/services/dispatch/internal/usecase"
)

// Handler exposes the dispatch use cases over HTTP.
type Handler struct {
	Orchestrator *usecase.Orchestrator
	Projection   *usecase.Projection
	Ready        func(context.Context) error
}

// OfferResponse is returned by accept/reject.
type OfferResponse struct {
	OfferID  string `json:"offerId"`
	RideID   string `json:"rideId"`
	DriverID string `json:"driverId"`
	Status   string `json:"status"`
}

// OfferJSON is an offer in views.
type OfferJSON struct {
	OfferID     string     `json:"offerId"`
	RideID      string     `json:"rideId"`
	DriverID    string     `json:"driverId"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"createdAt"`
	ExpiresAt   time.Time  `json:"expiresAt"`
	RespondedAt *time.Time `json:"respondedAt,omitempty"`
}

func presentOffer(o domain.Offer) OfferJSON {
	return OfferJSON{OfferID: o.ID, RideID: o.RideID, DriverID: o.DriverID, Status: o.Status,
		CreatedAt: o.CreatedAt, ExpiresAt: o.ExpiresAt, RespondedAt: o.RespondedAt}
}

// SagaJSON is the saga in the dispatch view.
type SagaJSON struct {
	RideID            string     `json:"rideId"`
	Status            string     `json:"status"`
	Pickup            geo.Point  `json:"pickup"`
	RadiusM           float64    `json:"radiusM"`
	ReservingDriverID *string    `json:"reservingDriverId,omitempty"`
	ReservingOfferID  *string    `json:"reservingOfferId,omitempty"`
	CurrentOfferID    *string    `json:"currentOfferId,omitempty"`
	Round             int        `json:"round"`
	RoundSkips        []string   `json:"roundSkips"`
	NextRetryAt       *time.Time `json:"nextRetryAt,omitempty"`
	Version           int64      `json:"version"`
	TraceID           string     `json:"traceId,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

// StepJSON is one audit step.
type StepJSON struct {
	Step     string `json:"step"`
	Detail   string `json:"detail,omitempty"`
	DriverID string `json:"driverId,omitempty"`
	OfferID  string `json:"offerId,omitempty"`
	EventID  string `json:"eventId,omitempty"`
	At       string `json:"at"`
}

func toProblem(err error) error {
	switch {
	case errors.Is(err, usecase.ErrOfferNotFound):
		return httpx.NewError(404, "OFFER_NOT_FOUND", err.Error())
	case errors.Is(err, usecase.ErrOfferNotCurrent):
		return httpx.NewError(409, "OFFER_NOT_CURRENT", err.Error())
	case errors.Is(err, usecase.ErrOfferExpired):
		return httpx.NewError(409, "OFFER_EXPIRED", err.Error())
	case errors.Is(err, usecase.ErrSagaNotFound):
		return httpx.NewError(404, "SAGA_NOT_FOUND", "no dispatch for this ride (yet)")
	}
	return err
}

func validID(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil
}

// Routes registers dispatch-service endpoints.
func (h *Handler) Routes(mux *http.ServeMux) {
	respond := func(action func(context.Context, string, string) (usecase.OfferResult, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ride, drv := r.PathValue("rideId"), r.PathValue("driverId")
			ctx := obs.With(r.Context(), obs.RideID, ride, obs.DriverID, drv)
			if !validID(ride) {
				httpx.WriteError(ctx, w, toProblem(usecase.ErrOfferNotFound))
				return
			}
			res, err := action(ctx, ride, drv)
			if err != nil {
				httpx.WriteError(ctx, w, toProblem(err))
				return
			}
			if res.Replayed {
				w.Header().Set("Idempotent-Replayed", "true")
			}
			httpx.WriteJSON(w, 200, OfferResponse{OfferID: res.Offer.ID, RideID: res.Offer.RideID,
				DriverID: res.Offer.DriverID, Status: res.Offer.Status})
		}
	}
	mux.HandleFunc("POST /rides/{rideId}/offers/{driverId}/accept", respond(h.Orchestrator.Accept))
	mux.HandleFunc("POST /rides/{rideId}/offers/{driverId}/reject", respond(h.Orchestrator.Reject))

	mux.HandleFunc("GET /rides/{rideId}/dispatch", func(w http.ResponseWriter, r *http.Request) {
		v, err := h.Orchestrator.Describe(r.Context(), r.PathValue("rideId"))
		if err != nil {
			httpx.WriteError(r.Context(), w, toProblem(err))
			return
		}
		s := v.Saga
		out := map[string]any{
			"saga": SagaJSON{RideID: s.RideID, Status: s.Status, Pickup: s.Pickup, RadiusM: s.RadiusM,
				ReservingDriverID: s.ReservingDriverID, ReservingOfferID: s.ReservingOfferID, CurrentOfferID: s.CurrentOfferID,
				Round: s.Round, RoundSkips: s.RoundSkips, NextRetryAt: s.NextRetryAt, Version: s.Version,
				TraceID: obs.TraceID(s.TraceParent), CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt},
			"exclusions": v.Exclusions,
		}
		offers := make([]OfferJSON, 0, len(v.Offers))
		for _, o := range v.Offers {
			offers = append(offers, presentOffer(o))
		}
		steps := make([]StepJSON, 0, len(v.Steps))
		for _, st := range v.Steps {
			steps = append(steps, StepJSON{Step: st.Name, Detail: st.Detail, DriverID: st.DriverID, OfferID: st.OfferID,
				EventID: st.EventID, At: st.At.UTC().Format("2006-01-02T15:04:05.000Z")})
		}
		out["offers"], out["steps"] = offers, steps
		httpx.WriteJSON(w, 200, out)
	})

	mux.HandleFunc("GET /drivers/{driverId}/offer", func(w http.ResponseWriter, r *http.Request) {
		off, err := h.Orchestrator.PendingOffer(r.Context(), r.PathValue("driverId"))
		if err != nil {
			httpx.WriteError(r.Context(), w, toProblem(err))
			return
		}
		if off == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		httpx.WriteJSON(w, 200, presentOffer(*off))
	})

	mux.HandleFunc("POST /internal/geo/rebuild", func(w http.ResponseWriter, r *http.Request) {
		n, err := h.Projection.Rebuild(r.Context())
		if err != nil {
			httpx.WriteError(r.Context(), w, err)
			return
		}
		httpx.WriteJSON(w, 200, map[string]int{"drivers": n})
	})

	httpx.Health(mux, h.Ready)
}
