package gamefleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// ServiceHistoryConfig identifies one history service and its immutable
// player profile. Placement and revision are intentionally absent because a
// history route follows the original caller and placement of each record.
type ServiceHistoryConfig struct {
	TLS            TLSFiles
	URL            string
	Key            string
	ServiceID      string
	ApplicationID  string
	IdentityIssuer string
	Region         string
	Compatibility  string
}

// ServiceHistoryError retains the HTTP status and a validated platform code.
// Its text form never includes a response message, request ID, or credential.
type ServiceHistoryError struct {
	Status int
	Code   string
}

func (*ServiceHistoryError) Error() string { return "gamefleet service history request failed" }

// ServiceHistoryClient exposes only explicitly routed history reads,
// cancellation intent, and ticket assignment or resume operations.
type ServiceHistoryClient struct {
	transport *ServiceSearchClient
}

// NewServiceHistoryClient reuses the already hardened private gfsvc transport
// and validates the complete service identity and profile at construction.
func NewServiceHistoryClient(cfg ServiceHistoryConfig) (*ServiceHistoryClient, error) {
	transport, err := NewServiceSearchClient(ServiceSearchConfig{
		TLS: cfg.TLS, URL: cfg.URL, Key: cfg.Key, ServiceID: cfg.ServiceID, ApplicationID: cfg.ApplicationID,
		IdentityIssuer: cfg.IdentityIssuer, Region: cfg.Region, Compatibility: cfg.Compatibility,
	})
	if err != nil {
		return nil, err
	}
	return &ServiceHistoryClient{transport: transport}, nil
}

func (c *ServiceHistoryClient) Close() {
	if c != nil && c.transport != nil {
		c.transport.Close()
	}
}

func serviceHistoryError(status int, code string) *ServiceHistoryError {
	return &ServiceHistoryError{Status: status, Code: code}
}

func (c *ServiceHistoryClient) call(ctx context.Context, method, path string, body, out any) error {
	if c == nil || c.transport == nil {
		return serviceHistoryError(http.StatusUnprocessableEntity, "")
	}
	_, err := c.transport.call(ctx, method, path, body, out, http.StatusOK)
	if err == nil {
		return nil
	}
	var searchErr *ServiceSearchError
	if errors.As(err, &searchErr) {
		return serviceHistoryError(searchErr.Status, searchErr.Code)
	}
	return serviceHistoryError(http.StatusBadGateway, "")
}

// CheckScope requires the exact service identity and all four operations used
// by the historical client. These operations still require a matching
// participant-specific owner route on every request.
func (c *ServiceHistoryClient) CheckScope(ctx context.Context) error {
	var scope struct {
		ServiceID      string   `json:"serviceId"`
		ApplicationID  string   `json:"applicationId"`
		IdentityIssuer string   `json:"identityIssuer"`
		Operations     []string `json:"operations"`
	}
	if err := c.call(ctx, http.MethodGet, "/business/v1/history/service", nil, &scope); err != nil {
		return err
	}
	if !opaqueID.MatchString(scope.ServiceID) || scope.ServiceID != c.transport.config.ServiceID ||
		scope.ApplicationID != c.transport.config.ApplicationID || scope.IdentityIssuer != c.transport.config.IdentityIssuer {
		return serviceHistoryError(http.StatusBadGateway, "")
	}
	seen := make(map[string]bool, len(scope.Operations))
	for _, operation := range scope.Operations {
		if seen[operation] {
			return serviceHistoryError(http.StatusBadGateway, "")
		}
		seen[operation] = true
		switch operation {
		case "read", "cancel", "assignment", "resume":
		default:
			return serviceHistoryError(http.StatusBadGateway, "")
		}
	}
	for _, required := range []string{"read", "cancel", "assignment", "resume"} {
		if !seen[required] {
			return serviceHistoryError(http.StatusForbidden, "")
		}
	}
	return nil
}

func (c *ServiceHistoryClient) validReservation(r Reservation) bool {
	return c != nil && c.transport != nil && c.transport.validReservation(r)
}

func (c *ServiceHistoryClient) validSearch(s Search) bool {
	return c != nil && c.transport != nil && c.transport.validSearch(s, s.ID)
}

// Current returns the original routed room, or a required explicit null when
// the service has no route holding a current reservation for this participant.
func (c *ServiceHistoryClient) Current(ctx context.Context, participant string) (CurrentResult, error) {
	var out CurrentResult
	if c == nil || c.transport == nil || ctx == nil || !exactText(participant, 256) {
		return out, serviceHistoryError(http.StatusUnprocessableEntity, "")
	}
	body := struct {
		Version       string `json:"version"`
		ParticipantID string `json:"participantId"`
	}{RoomVersion, participant}
	var wire struct {
		Current json.RawMessage `json:"current"`
	}
	if err := c.call(ctx, http.MethodPost, "/business/v1/history/reservations/current", body, &wire); err != nil {
		return CurrentResult{}, err
	}
	if len(wire.Current) == 0 {
		return CurrentResult{}, serviceHistoryError(http.StatusBadGateway, "")
	}
	if bytes.Equal(bytes.TrimSpace(wire.Current), []byte("null")) {
		return out, nil
	}
	var current struct {
		Reservation          Reservation `json:"reservation"`
		ConnectionGeneration *int64      `json:"connectionGeneration"`
	}
	if strictObject(wire.Current, &current) != nil || current.ConnectionGeneration == nil ||
		!c.validReservation(current.Reservation) || current.Reservation.State == "completed" ||
		current.Reservation.State == "technical_aborted" || *current.ConnectionGeneration < 0 ||
		*current.ConnectionGeneration > MaxGeneration {
		return CurrentResult{}, serviceHistoryError(http.StatusBadGateway, "")
	}
	out.Current = &Current{Reservation: current.Reservation, ConnectionGeneration: *current.ConnectionGeneration}
	return out, nil
}

func historyParticipantBody(version, participant string) any {
	return struct {
		Version       string `json:"version"`
		ParticipantID string `json:"participantId"`
	}{version, participant}
}

func (c *ServiceHistoryClient) Status(ctx context.Context, allocationID, participant string) (ReservationStatus, error) {
	var out ReservationStatus
	if c == nil || c.transport == nil || ctx == nil || !opaqueID.MatchString(allocationID) || !exactText(participant, 256) {
		return out, serviceHistoryError(http.StatusUnprocessableEntity, "")
	}
	path := "/business/v1/history/reservations/" + allocationID + "/status"
	if err := c.call(ctx, http.MethodPost, path, historyParticipantBody(RoomVersion, participant), &out); err != nil {
		return ReservationStatus{}, err
	}
	if !c.validReservation(out.Reservation) || out.Reservation.AllocationID != allocationID {
		return ReservationStatus{}, serviceHistoryError(http.StatusBadGateway, "")
	}
	return out, nil
}

func (c *ServiceHistoryClient) Cancel(ctx context.Context, allocationID, participant string) (ReservationResult, error) {
	var out ReservationResult
	if c == nil || c.transport == nil || ctx == nil || !opaqueID.MatchString(allocationID) || !exactText(participant, 256) {
		return out, serviceHistoryError(http.StatusUnprocessableEntity, "")
	}
	path := "/business/v1/history/reservations/" + allocationID + "/cancel"
	if err := c.call(ctx, http.MethodPost, path, historyParticipantBody(RoomVersion, participant), &out); err != nil {
		return ReservationResult{}, err
	}
	if !c.validReservation(out.Reservation) || out.Reservation.AllocationID != allocationID {
		return ReservationResult{}, serviceHistoryError(http.StatusBadGateway, "")
	}
	return out, nil
}

func (c *ServiceHistoryClient) Issue(ctx context.Context, allocationID, participant, idempotencyKey string, previous int64, resume bool) (AssignmentResult, error) {
	var out AssignmentResult
	if c == nil || c.transport == nil || ctx == nil || !opaqueID.MatchString(allocationID) || !exactText(participant, 256) ||
		!attemptID.MatchString(idempotencyKey) || previous < 0 || previous >= MaxGeneration || (!resume && previous != 0) ||
		(resume && previous == 0) {
		return out, serviceHistoryError(http.StatusUnprocessableEntity, "")
	}
	operation := "assignment"
	if resume {
		operation = "resume"
	}
	body := struct {
		Version                      string `json:"version"`
		IdempotencyKey               string `json:"idempotencyKey"`
		ParticipantID                string `json:"participantId"`
		PreviousConnectionGeneration int64  `json:"previousConnectionGeneration"`
	}{TicketVersion, idempotencyKey, participant, previous}
	path := "/business/v1/history/reservations/" + allocationID + "/" + operation
	if err := c.call(ctx, http.MethodPost, path, body, &out); err != nil {
		return AssignmentResult{}, err
	}
	if !validAssignment(out.Assignment, previous+1) || (out.Assignment.Ticket.State != "issued" && !out.Replay) {
		return AssignmentResult{}, serviceHistoryError(http.StatusBadGateway, "")
	}
	return out, nil
}

func (c *ServiceHistoryClient) SearchStatus(ctx context.Context, searchID, participant string) (SearchStatus, error) {
	var out SearchStatus
	if c == nil || c.transport == nil || ctx == nil || !attemptID.MatchString(searchID) || !exactText(participant, 256) {
		return out, serviceHistoryError(http.StatusUnprocessableEntity, "")
	}
	path := "/business/v1/history/searches/" + searchID + "/status"
	if err := c.call(ctx, http.MethodPost, path, historyParticipantBody(SearchVersion, participant), &out); err != nil {
		return SearchStatus{}, err
	}
	if !c.validSearch(out.Search) || out.Search.ID != searchID {
		return SearchStatus{}, serviceHistoryError(http.StatusBadGateway, "")
	}
	return out, nil
}

func (c *ServiceHistoryClient) CancelSearch(ctx context.Context, searchID, participant string) (SearchResult, error) {
	var out SearchResult
	if c == nil || c.transport == nil || ctx == nil || !attemptID.MatchString(searchID) || !exactText(participant, 256) {
		return out, serviceHistoryError(http.StatusUnprocessableEntity, "")
	}
	path := "/business/v1/history/searches/" + searchID + "/cancel"
	if err := c.call(ctx, http.MethodPost, path, historyParticipantBody(SearchVersion, participant), &out); err != nil {
		return SearchResult{}, err
	}
	if !c.validSearch(out.Search) || out.Search.ID != searchID || out.Search.State == "pending" {
		return SearchResult{}, serviceHistoryError(http.StatusBadGateway, "")
	}
	return out, nil
}
