package gamefleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"sort"
	"time"
)

const (
	ServiceSearchVersion    = "gamefleet.service-player-search.v1"
	serviceSearchBodyLimit  = 64 << 10
	serviceMatchRetryWindow = 8 * time.Second
	serviceMatchMaxAttempts = 20
)

var serviceSearchIdentityIssuer = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

// ServiceSearchConfig identifies an exact machine service and its player
// profile. It intentionally has no caller key or deployment placement fields.
type ServiceSearchConfig struct {
	TLS            TLSFiles
	URL            string
	Key            string
	ServiceID      string
	ApplicationID  string
	IdentityIssuer string
	Region         string
	Compatibility  string
}

// ServiceSearchError preserves the HTTP status and, when a strict error
// envelope was received, its code. Its string form never includes wire data.
type ServiceSearchError struct {
	Status int
	Code   string
}

func (e *ServiceSearchError) Error() string { return "gamefleet service search request failed" }

// ServiceSearchClient exposes only scope checking, mapped search creation and
// status, exact pair matching, and transport cleanup. It cannot reserve a
// room directly or issue, cancel, or resume a player ticket.
type ServiceSearchClient struct {
	config ServiceSearchConfig
	base   string
	http   *http.Client
}

// NewServiceSearchClient requires authenticated HTTPS and an exact gfsvc identity.
func NewServiceSearchClient(cfg ServiceSearchConfig) (*ServiceSearchClient, error) {
	base, transport, err := businessTransport(cfg.URL, cfg.TLS)
	if err != nil {
		return nil, err
	}
	if !validHistoryServiceKey(cfg.Key) || !opaqueID.MatchString(cfg.ServiceID) ||
		!opaqueID.MatchString(cfg.ApplicationID) || !serviceSearchIdentityIssuer.MatchString(cfg.IdentityIssuer) ||
		!exactText(cfg.Region, 128) || !exactText(cfg.Compatibility, 128) {
		return nil, errors.New("invalid service search scope or gfsvc credential")
	}

	return &ServiceSearchClient{
		config: cfg,
		base:   base,
		http: &http.Client{
			Transport: transport,
			Timeout:   8 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c *ServiceSearchClient) Close() {
	if c != nil && c.http != nil {
		c.http.CloseIdleConnections()
	}
}

func serviceSearchError(status int, code string) *ServiceSearchError {
	return &ServiceSearchError{Status: status, Code: code}
}

func serviceSearchStatusExpected(status int, accepted []int) bool {
	for _, candidate := range accepted {
		if status == candidate {
			return true
		}
	}
	return false
}

func serviceSearchReadBounded(body io.Reader) ([]byte, bool) {
	raw, err := io.ReadAll(io.LimitReader(body, serviceSearchBodyLimit+1))
	if err != nil || len(raw) > serviceSearchBodyLimit {
		return nil, false
	}
	return raw, true
}

func serviceSearchErrorCode(resp *http.Response) string {
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return ""
	}
	raw, ok := serviceSearchReadBounded(resp.Body)
	if !ok {
		return ""
	}
	var envelope struct {
		Error *struct {
			Code    *string `json:"code"`
			Message *string `json:"message"`
		} `json:"error"`
		RequestID *string `json:"requestId"`
	}
	if strictObject(raw, &envelope) != nil || envelope.Error == nil || envelope.Error.Code == nil ||
		envelope.Error.Message == nil || envelope.RequestID == nil ||
		!exactText(*envelope.Error.Code, 128) || !exactText(*envelope.Error.Message, 4096) ||
		!exactText(*envelope.RequestID, 128) {
		return ""
	}
	return *envelope.Error.Code
}

func (c *ServiceSearchClient) call(ctx context.Context, method, path string, body any, out any, accepted ...int) (int, error) {
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return 0, serviceSearchError(422, "")
		}
	}
	return c.callEncoded(ctx, method, path, encoded, body != nil, out, accepted)
}

// callEncoded reuses an already marshaled request body. MatchSearches calls it
// repeatedly with one immutable byte slice so capacity retries cannot drift.
func (c *ServiceSearchClient) callEncoded(ctx context.Context, method, path string, encoded []byte, hasBody bool, out any, accepted []int) (int, error) {
	if c == nil || c.http == nil || ctx == nil {
		return 0, serviceSearchError(422, "")
	}
	var body io.Reader
	if hasBody {
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, serviceSearchError(503, "")
	}
	req.Header.Set("Authorization", "Bearer "+c.config.Key)
	req.Header.Set("Accept", "application/json")
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, serviceSearchError(503, "")
	}
	defer resp.Body.Close()
	if !serviceSearchStatusExpected(resp.StatusCode, accepted) {
		code := serviceSearchErrorCode(resp)
		return resp.StatusCode, serviceSearchError(resp.StatusCode, code)
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return resp.StatusCode, serviceSearchError(502, "")
	}
	raw, ok := serviceSearchReadBounded(resp.Body)
	if !ok {
		return resp.StatusCode, serviceSearchError(502, "")
	}
	var envelope struct {
		Data      json.RawMessage `json:"data"`
		RequestID *string         `json:"requestId"`
	}
	if strictObject(raw, &envelope) != nil || len(envelope.Data) == 0 || bytes.Equal(envelope.Data, []byte("null")) ||
		envelope.RequestID == nil || !exactText(*envelope.RequestID, 128) || strictObject(envelope.Data, out) != nil {
		return resp.StatusCode, serviceSearchError(502, "")
	}
	return resp.StatusCode, nil
}

// CheckScope confirms the configured service ID, application, and issuer
// before runtime registration. Match authority still comes from its separate
// owner grant and is checked by the server on each operation.
func (c *ServiceSearchClient) CheckScope(ctx context.Context) error {
	var scope struct {
		ServiceID      string   `json:"serviceId"`
		ApplicationID  string   `json:"applicationId"`
		IdentityIssuer string   `json:"identityIssuer"`
		Operations     []string `json:"operations"`
	}
	_, err := c.call(ctx, http.MethodGet, "/business/v1/history/service", nil, &scope, http.StatusOK)
	if err != nil {
		return err
	}
	if !opaqueID.MatchString(scope.ServiceID) || scope.ServiceID != c.config.ServiceID ||
		scope.ApplicationID != c.config.ApplicationID || scope.IdentityIssuer != c.config.IdentityIssuer {
		return serviceSearchError(502, "")
	}
	seen := make(map[string]bool, len(scope.Operations))
	read := false
	for _, operation := range scope.Operations {
		if seen[operation] {
			return serviceSearchError(502, "")
		}
		seen[operation] = true
		switch operation {
		case "read":
			read = true
		case "cancel", "assignment", "resume":
		default:
			return serviceSearchError(502, "")
		}
	}
	if !read {
		return serviceSearchError(403, "")
	}
	return nil
}

func (c *ServiceSearchClient) validReservation(r Reservation) bool {
	stateValid := r.State == "reserved" || r.State == "prepared" || r.State == "completed" ||
		(r.State == "technical_aborted" && r.FailureCode == "host_process_terminated")
	failureCodeValid := r.State == "technical_aborted" || r.FailureCode == ""
	return opaqueID.MatchString(r.AllocationID) && opaqueID.MatchString(r.ReservationID) && opaqueID.MatchString(r.RoomID) &&
		r.ApplicationID == c.config.ApplicationID && opaqueID.MatchString(r.PlacementID) && opaqueID.MatchString(r.RevisionID) &&
		r.Region == c.config.Region && stateValid && failureCodeValid &&
		r.CreatedAt.UnixMilli() > 0 && !r.UpdatedAt.IsZero() && r.UpdatedAt.UnixMilli() > 0 &&
		!r.UpdatedAt.Before(r.CreatedAt)
}

func (c *ServiceSearchClient) validSearch(s Search, expectedID string) bool {
	ttl := s.ExpiresAt.Sub(s.CreatedAt)
	if !attemptID.MatchString(s.ID) || s.ID != expectedID || s.Region != c.config.Region || s.Compatibility != c.config.Compatibility ||
		s.CreatedAt.UnixMilli() <= 0 || s.ExpiresAt.UnixMilli() <= 0 || ttl < 30*time.Second ||
		ttl > 600*time.Second || ttl%time.Second != 0 {
		return false
	}
	noRoom := s.AllocationID == "" && s.Reservation == nil
	if s.State == "pending" {
		return s.ResolvedAt == nil && noRoom
	}
	if s.ResolvedAt == nil || s.ResolvedAt.IsZero() || s.ResolvedAt.UnixMilli() <= 0 || s.ResolvedAt.Before(s.CreatedAt) {
		return false
	}
	switch s.State {
	case "cancelled":
		return s.ResolvedAt.Before(s.ExpiresAt) && noRoom
	case "expired":
		return !s.ResolvedAt.Before(s.ExpiresAt) && noRoom
	case "bound":
		return s.ResolvedAt.Before(s.ExpiresAt) && opaqueID.MatchString(s.AllocationID) && s.Reservation != nil &&
			s.Reservation.AllocationID == s.AllocationID && s.Reservation.CreatedAt.Equal(*s.ResolvedAt) &&
			c.validReservation(*s.Reservation)
	default:
		return false
	}
}

func (c *ServiceSearchClient) BeginSearch(ctx context.Context, participant, requestID string) (SearchResult, error) {
	var out SearchResult
	if c == nil || c.http == nil || ctx == nil || !exactText(participant, 256) || !attemptID.MatchString(requestID) {
		return out, serviceSearchError(422, "")
	}
	body := struct {
		Version       string `json:"version"`
		ParticipantID string `json:"participantId"`
		RequestID     string `json:"requestId"`
		Region        string `json:"region"`
		Compatibility string `json:"compatibility"`
	}{ServiceSearchVersion, participant, requestID, c.config.Region, c.config.Compatibility}
	status, err := c.call(ctx, http.MethodPost, "/business/v1/service-searches", body, &out, http.StatusCreated, http.StatusOK)
	if err != nil {
		return SearchResult{}, err
	}
	if !c.validSearch(out.Search, out.Search.ID) ||
		(status == http.StatusCreated && out.Replay) || (status == http.StatusOK && !out.Replay) {
		return SearchResult{}, serviceSearchError(502, "")
	}
	return out, nil
}

func (c *ServiceSearchClient) SearchStatus(ctx context.Context, searchID, participant string) (SearchStatus, error) {
	var out SearchStatus
	if c == nil || c.http == nil || ctx == nil || !attemptID.MatchString(searchID) || !exactText(participant, 256) {
		return out, serviceSearchError(422, "")
	}
	body := struct {
		Version       string `json:"version"`
		ParticipantID string `json:"participantId"`
	}{ServiceSearchVersion, participant}
	_, err := c.call(ctx, http.MethodPost, "/business/v1/service-searches/"+searchID+"/status", body, &out, http.StatusOK)
	if err != nil {
		return SearchStatus{}, err
	}
	if !c.validSearch(out.Search, searchID) {
		return SearchStatus{}, serviceSearchError(502, "")
	}
	return out, nil
}

func serviceSearchMatchMembers(members []SearchMatchMember) ([]SearchMatchMember, error) {
	if len(members) != 2 {
		return nil, serviceSearchError(422, "")
	}
	copyOfMembers := append([]SearchMatchMember(nil), members...)
	for _, member := range copyOfMembers {
		if !exactText(member.ParticipantID, 256) || !attemptID.MatchString(member.SearchID) || !exactText(member.NakamaTicket, 256) {
			return nil, serviceSearchError(422, "")
		}
	}
	if copyOfMembers[0].ParticipantID == copyOfMembers[1].ParticipantID ||
		copyOfMembers[0].SearchID == copyOfMembers[1].SearchID ||
		copyOfMembers[0].NakamaTicket == copyOfMembers[1].NakamaTicket {
		return nil, serviceSearchError(422, "")
	}
	sort.Slice(copyOfMembers, func(i, j int) bool {
		return copyOfMembers[i].ParticipantID < copyOfMembers[j].ParticipantID
	})
	return copyOfMembers, nil
}

// MatchSearches submits one exact pair. It sorts a private copy, marshals one
// body, and retries only the documented capacity code for at most eight
// seconds. Other conflicts and all transport/unknown errors return immediately.
func (c *ServiceSearchClient) MatchSearches(ctx context.Context, idempotencyKey string, members []SearchMatchMember) (ReservationResult, error) {
	var out ReservationResult
	if c == nil || c.http == nil || ctx == nil || !attemptID.MatchString(idempotencyKey) {
		return out, serviceSearchError(422, "")
	}
	ordered, err := serviceSearchMatchMembers(members)
	if err != nil {
		return out, err
	}
	body := struct {
		Version        string              `json:"version"`
		IdempotencyKey string              `json:"idempotencyKey"`
		Region         string              `json:"region"`
		Compatibility  string              `json:"compatibility"`
		Members        []SearchMatchMember `json:"members"`
	}{ServiceSearchVersion, idempotencyKey, c.config.Region, c.config.Compatibility, ordered}
	encoded, err := json.Marshal(body)
	if err != nil {
		return out, serviceSearchError(422, "")
	}
	retryCtx, cancel := context.WithTimeout(ctx, serviceMatchRetryWindow)
	defer cancel()
	delay := 100 * time.Millisecond
	for attempt := 0; attempt < serviceMatchMaxAttempts; attempt++ {
		var wire struct {
			Reservation Reservation `json:"reservation"`
			Replay      *bool       `json:"replay"`
		}
		status, callErr := c.callEncoded(retryCtx, http.MethodPost, "/business/v1/service-searches/match", encoded, true,
			&wire, []int{http.StatusOK, http.StatusAccepted})
		if callErr == nil {
			if wire.Replay == nil {
				return ReservationResult{}, serviceSearchError(502, "")
			}
			out = ReservationResult{Reservation: wire.Reservation, Replay: *wire.Replay}
			if !c.validReservation(out.Reservation) ||
				(status == http.StatusAccepted && out.Replay) || (status == http.StatusOK && !out.Replay) {
				return ReservationResult{}, serviceSearchError(502, "")
			}
			return out, nil
		}
		var serviceErr *ServiceSearchError
		if !errors.As(callErr, &serviceErr) || serviceErr.Status != http.StatusConflict ||
			serviceErr.Code != "service_search_match_capacity_unavailable" || attempt+1 >= serviceMatchMaxAttempts || retryCtx.Err() != nil {
			return ReservationResult{}, callErr
		}
		timer := time.NewTimer(delay)
		select {
		case <-retryCtx.Done():
			timer.Stop()
			return ReservationResult{}, callErr
		case <-timer.C:
		}
		delay = min(delay*2, 500*time.Millisecond)
	}
	return ReservationResult{}, serviceSearchError(503, "")
}
