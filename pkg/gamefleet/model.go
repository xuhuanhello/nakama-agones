// Package gamefleet is the opt-in Nakama adapter for GameFleet's private
// business API. It owns neither allocation state nor Kubernetes resources.
package gamefleet

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const (
	RoomVersion         = "gamefleet.player-room.v1"
	SearchVersion       = "gamefleet.player-search.v1"
	TicketVersion       = "gamefleet.player-ticket.v1"
	MaxGeneration int64 = 9007199254740991
)

type Scope struct {
	CallerID      string   `json:"callerId"`
	ApplicationID string   `json:"applicationId"`
	PlacementID   string   `json:"placementId"`
	RevisionID    string   `json:"revisionId"`
	Region        string   `json:"region"`
	Operations    []string `json:"operations"`
}

type Reservation struct {
	ReservationID         string    `json:"reservationId"`
	AllocationID          string    `json:"allocationId"`
	RoomID                string    `json:"roomId"`
	ApplicationID         string    `json:"applicationId"`
	PlacementID           string    `json:"placementId"`
	RevisionID            string    `json:"revisionId"`
	Region                string    `json:"region"`
	State                 string    `json:"state"`
	FailureCode           string    `json:"failureCode,omitempty"`
	CancellationRequested bool      `json:"cancellationRequested"`
	CreatedAt             time.Time `json:"createdAt"`
	UpdatedAt             time.Time `json:"updatedAt"`
}

type Current struct {
	Reservation          Reservation `json:"reservation"`
	ConnectionGeneration int64       `json:"connectionGeneration"`
}
type CurrentResult struct {
	Current *Current `json:"current"`
}
type ReservationResult struct {
	Reservation Reservation `json:"reservation"`
	Replay      bool        `json:"replay"`
}
type ReservationStatus struct {
	Reservation Reservation `json:"reservation"`
}
type Ticket struct {
	ID         string    `json:"id"`
	State      string    `json:"state"`
	Token      string    `json:"token,omitempty"`
	Generation int64     `json:"generation"`
	ExpiresAt  time.Time `json:"expiresAt"`
}
type Port struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
}
type Endpoint struct {
	Address string `json:"address"`
	Ports   []Port `json:"ports"`
}
type Assignment struct {
	Ticket   Ticket    `json:"ticket"`
	Endpoint *Endpoint `json:"endpoint,omitempty"`
}
type AssignmentResult struct {
	Assignment Assignment `json:"assignment"`
	Replay     bool       `json:"replay"`
}

// Backend is deliberately limited to the scoped player API. It has no host
// creation, admission override, database or Kubernetes access.
type Backend interface {
	Current(context.Context, string) (CurrentResult, error)
	Status(context.Context, string, string) (ReservationStatus, error)
	BeginSearch(context.Context, string, string) (SearchResult, error)
	SearchStatus(context.Context, string, string) (SearchStatus, error)
	CancelSearch(context.Context, string, string) (SearchResult, error)
	MatchSearches(context.Context, string, []SearchMatchMember) (ReservationResult, error)
	Issue(context.Context, string, string, string, int64, bool) (AssignmentResult, error)
	Cancel(context.Context, string) (ReservationResult, error)
}

// Error contains only a locally chosen status class, never an upstream body,
// credential, player ticket or transport URL.
type Error struct{ Status int }

func (e *Error) Error() string { return "gamefleet request failed" }

// These booleans are required wire facts. Missing/null must not be silently
// interpreted as false (especially for lifecycle and exact-retry responses).
func (r *Reservation) UnmarshalJSON(raw []byte) error {
	type plain Reservation
	var wire struct {
		*plain
		Required    *bool           `json:"cancellationRequested"`
		FailureCode json.RawMessage `json:"failureCode"`
	}
	wire.plain = (*plain)(r)
	r.FailureCode = ""
	if err := strictObject(raw, &wire); err != nil {
		return err
	}
	if wire.Required == nil {
		return errors.New("missing cancellationRequested")
	}
	if wire.FailureCode != nil {
		var failureCode *string
		if err := json.Unmarshal(wire.FailureCode, &failureCode); err != nil || failureCode == nil {
			return errors.New("invalid failureCode")
		}
		r.FailureCode = *failureCode
	}
	r.CancellationRequested = *wire.Required
	return nil
}
func (r *ReservationResult) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Reservation Reservation `json:"reservation"`
		Replay      *bool       `json:"replay"`
	}
	if err := strictObject(raw, &wire); err != nil {
		return err
	}
	if wire.Replay == nil {
		return errors.New("missing replay")
	}
	r.Reservation = wire.Reservation
	r.Replay = *wire.Replay
	return nil
}
func (r *AssignmentResult) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Assignment Assignment `json:"assignment"`
		Replay     *bool      `json:"replay"`
	}
	if err := strictObject(raw, &wire); err != nil {
		return err
	}
	if wire.Replay == nil {
		return errors.New("missing replay")
	}
	r.Assignment = wire.Assignment
	r.Replay = *wire.Replay
	return nil
}
