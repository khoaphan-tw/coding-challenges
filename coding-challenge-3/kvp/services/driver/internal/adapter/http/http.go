// Package http is driver-service's driving adapter for HTTP.
// /internal/* is not routed by the gateway (service-to-service and tests only).
package http

import (
	"context"
	"errors"
	"net/http"
	"time"

	"ridehail/pkg/geo"
	"ridehail/pkg/httpx"
	"ridehail/pkg/obs"
	"ridehail/services/driver/internal/domain"
	"ridehail/services/driver/internal/usecase"
)

// Handler exposes the driver use cases over HTTP.
type Handler struct {
	Drivers *usecase.Drivers
	Ready   func(context.Context) error
}

// DriverResponse is the public JSON shape of a driver.
type DriverResponse struct {
	DriverID          string     `json:"driverId"`
	Status            string     `json:"status"`
	Latitude          float64    `json:"latitude"`
	Longitude         float64    `json:"longitude"`
	CurrentRideID     *string    `json:"currentRideId"`
	Version           int64      `json:"version"`
	LocationUpdatedAt *time.Time `json:"locationUpdatedAt,omitempty"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

func present(d domain.Driver) DriverResponse {
	return DriverResponse{DriverID: d.ID, Status: string(d.Status), Latitude: d.Latitude, Longitude: d.Longitude,
		CurrentRideID: d.CurrentRideID, Version: d.Version, LocationUpdatedAt: d.LocationUpdatedAt, UpdatedAt: d.UpdatedAt}
}

func toProblem(err error) error {
	switch {
	case errors.Is(err, usecase.ErrDriverNotFound):
		return httpx.NewError(404, "DRIVER_NOT_FOUND", "driver not found")
	case errors.Is(err, usecase.ErrInvalidStatus):
		return httpx.NewError(422, "INVALID_STATUS", err.Error())
	case errors.Is(err, domain.ErrInvalidTransition):
		return httpx.NewError(409, "INVALID_TRANSITION",
			"only OFFLINE <-> AVAILABLE is allowed; OFFERED and BUSY drivers cannot change availability")
	case errors.Is(err, geo.ErrInvalidCoordinates):
		return httpx.NewError(422, "INVALID_COORDINATES", "latitude must be in [-90, 90] and longitude in [-180, 180]")
	}
	return err
}

// Routes registers driver-service endpoints.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /drivers/{driverId}", func(w http.ResponseWriter, r *http.Request) {
		d, err := h.Drivers.Get(r.Context(), r.PathValue("driverId"))
		if err != nil {
			httpx.WriteError(r.Context(), w, toProblem(err))
			return
		}
		httpx.WriteJSON(w, 200, present(d))
	})

	mux.HandleFunc("PUT /drivers/{driverId}/availability", func(w http.ResponseWriter, r *http.Request) {
		ctx := obs.With(r.Context(), obs.DriverID, r.PathValue("driverId"))
		var req struct {
			Status string `json:"status"`
		}
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(ctx, w, err)
			return
		}
		d, err := h.Drivers.SetAvailability(ctx, r.PathValue("driverId"), domain.Status(req.Status))
		if err != nil {
			httpx.WriteError(ctx, w, toProblem(err))
			return
		}
		httpx.WriteJSON(w, 200, present(d))
	})

	mux.HandleFunc("PUT /drivers/{driverId}/location", func(w http.ResponseWriter, r *http.Request) {
		ctx := obs.With(r.Context(), obs.DriverID, r.PathValue("driverId"))
		var req geo.PointInput
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(ctx, w, err)
			return
		}
		p, err := req.ToPoint()
		if err != nil {
			httpx.WriteError(ctx, w, toProblem(geo.ErrInvalidCoordinates))
			return
		}
		d, err := h.Drivers.UpdateLocation(ctx, r.PathValue("driverId"), p)
		if err != nil {
			httpx.WriteError(ctx, w, toProblem(err))
			return
		}
		httpx.WriteJSON(w, 200, present(d))
	})

	mux.HandleFunc("GET /internal/drivers", func(w http.ResponseWriter, r *http.Request) {
		ds, err := h.Drivers.List(r.Context(), r.URL.Query().Get("status"))
		if err != nil {
			httpx.WriteError(r.Context(), w, toProblem(err))
			return
		}
		out := make([]DriverResponse, 0, len(ds))
		for _, d := range ds {
			out = append(out, present(d))
		}
		httpx.WriteJSON(w, 200, out)
	})

	mux.HandleFunc("PUT /internal/drivers/{driverId}", func(w http.ResponseWriter, r *http.Request) {
		ctx := obs.With(r.Context(), obs.DriverID, r.PathValue("driverId"))
		var req struct {
			Status    string   `json:"status"`
			Latitude  *float64 `json:"latitude"`
			Longitude *float64 `json:"longitude"`
		}
		if err := httpx.DecodeJSON(r, &req); err != nil {
			httpx.WriteError(ctx, w, err)
			return
		}
		p, err := (&geo.PointInput{Latitude: req.Latitude, Longitude: req.Longitude}).ToPoint()
		if err != nil {
			httpx.WriteError(ctx, w, toProblem(geo.ErrInvalidCoordinates))
			return
		}
		d, err := h.Drivers.Upsert(ctx, r.PathValue("driverId"), domain.Status(req.Status), p)
		if err != nil {
			httpx.WriteError(ctx, w, toProblem(err))
			return
		}
		httpx.WriteJSON(w, 200, present(d))
	})

	httpx.Health(mux, h.Ready)
}
