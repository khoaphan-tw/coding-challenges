// Package messaging is the message CONTRACT shared by the services: the
// envelope, topic names and message types. It is pure (no broker, no
// database), so use cases may depend on it. The infrastructure that moves
// envelopes (outbox, relay, inbox, consumer) lives in pkg/broker.
package messaging

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Topics. Keys: ride topics by rideId, driver topics by driverId.
const (
	TopicRideEvents      = "ride.events.v1"
	TopicDriverEvents    = "driver.events.v1"
	TopicRideCommands    = "ride.commands.v1"
	TopicDriverCommands  = "driver.commands.v1"
	TopicDispatchReplies = "dispatch.replies.v1"
)

// AllTopics lists every topic (DLQ topics are derived with DLQ()).
var AllTopics = []string{TopicRideEvents, TopicDriverEvents, TopicRideCommands, TopicDriverCommands, TopicDispatchReplies}

// DLQ returns the dead-letter topic for topic.
func DLQ(topic string) string { return topic + ".dlq" }

// Handler processes one message. Returning an error triggers a retry.
type Handler func(ctx context.Context, env Envelope) error

// Envelope is the wire format of every event, command and reply.
type Envelope struct {
	EventID       string          `json:"eventId"`
	Type          string          `json:"type"`
	SchemaVersion int             `json:"schemaVersion"`
	OccurredAt    time.Time       `json:"occurredAt"`
	CorrelationID string          `json:"correlationId,omitempty"`
	CausationID   string          `json:"causationId,omitempty"`
	RideID        string          `json:"rideId,omitempty"`
	DriverID      string          `json:"driverId,omitempty"`
	OfferID       string          `json:"offerId,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
}

// New builds an envelope; EventID/OccurredAt are filled in by the outbox.
func New(typ, rideID, driverID, offerID string, payload any) Envelope {
	env := Envelope{Type: typ, SchemaVersion: 1, RideID: rideID, DriverID: driverID, OfferID: offerID}
	if payload != nil {
		b, _ := json.Marshal(payload)
		env.Payload = b
	}
	return env
}

// Decode unmarshals the payload into v.
func (e Envelope) Decode(v any) error {
	if len(e.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(e.Payload, v)
}

// NewID returns a time-sortable UUIDv7 string.
func NewID() string { return uuid.Must(uuid.NewV7()).String() }

// Reply payloads shared by participants and the orchestrator.
type (
	// CommandResult is the payload of CommandSucceeded / CommandRejected.
	CommandResult struct {
		Command        string `json:"command"`
		CommandEventID string `json:"commandEventId"`
		Reason         string `json:"reason,omitempty"`
	}
)

// Message type names.
const (
	// ride.events
	RideRequested     = "RideRequested"
	RideStatusChanged = "RideStatusChanged"
	RideCancelled     = "RideCancelled"
	RideCompleted     = "RideCompleted"
	// driver.events
	DriverStatusChanged   = "DriverStatusChanged"
	DriverLocationUpdated = "DriverLocationUpdated"
	// ride.commands
	CmdStartMatching  = "StartMatching"
	CmdAssignDriver   = "AssignDriver"
	CmdUnassignDriver = "UnassignDriver"
	CmdMarkAccepted   = "MarkAccepted"
	// driver.commands
	CmdReserveDriver  = "ReserveDriver"
	CmdReleaseDriver  = "ReleaseDriver"
	CmdMarkDriverBusy = "MarkDriverBusy"
	// dispatch.replies
	ReplyCommandSucceeded        = "CommandSucceeded"
	ReplyCommandRejected         = "CommandRejected"
	ReplyDriverReserved          = "DriverReserved"
	ReplyDriverReservationFailed = "DriverReservationFailed"
)
