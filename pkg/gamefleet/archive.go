package gamefleet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	HistoryServiceKeyPrefix = "gfsvc_"
	archiveResponseLimit    = 64 << 10
)

var historyServiceKeyPayload = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
var historyIdentityIssuer = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

// TerminalArchiveConfig identifies the one historical business scope this
// read-only client may inspect. Placement and revision are deliberately absent:
// retained records keep their immutable historical placement and revision.
type TerminalArchiveConfig struct {
	URL            string
	Key            string
	ServiceID      string
	ApplicationID  string
	IdentityIssuer string
	Region         string
	Compatibility  string
}

// TerminalArchiveClient exposes one identity metadata preflight and two
// participant-bound terminal status reads. It has no current, ticket,
// cancellation, search creation, match, or other business methods.
type TerminalArchiveClient struct {
	config TerminalArchiveConfig
	base   string
	http   *http.Client
}

func validHistoryServiceKey(key string) bool {
	if len(key) != len(HistoryServiceKeyPrefix)+43 || !strings.HasPrefix(key, HistoryServiceKeyPrefix) {
		return false
	}
	payload := strings.TrimPrefix(key, HistoryServiceKeyPrefix)
	if !historyServiceKeyPayload.MatchString(payload) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == payload
}

func NewTerminalArchiveClient(cfg TerminalArchiveConfig) (*TerminalArchiveClient, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u == nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return nil, errors.New("terminal archive URL must be an explicit loopback HTTP origin")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	port, portErr := strconv.Atoi(u.Port())
	if err != nil || !ip.IsLoopback() || ip.Zone() != "" || portErr != nil || port < 1 || port > 65535 ||
		u.Host != net.JoinHostPort(ip.String(), strconv.Itoa(port)) {
		return nil, errors.New("terminal archive URL requires a canonical loopback IP and port")
	}
	if !opaqueID.MatchString(cfg.ServiceID) || !opaqueID.MatchString(cfg.ApplicationID) || !historyIdentityIssuer.MatchString(cfg.IdentityIssuer) ||
		!exactText(cfg.Region, 128) || !exactText(cfg.Compatibility, 128) || !validHistoryServiceKey(cfg.Key) {
		return nil, errors.New("invalid terminal archive scope or gfsvc credential")
	}
	transport := &http.Transport{
		Proxy:           nil,
		DialContext:     (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxConnsPerHost: 16, MaxIdleConns: 16, MaxIdleConnsPerHost: 16,
		IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 5 * time.Second,
		MaxResponseHeaderBytes: 16 << 10, DisableCompression: true,
	}
	return &TerminalArchiveClient{
		config: cfg,
		base:   "http://" + u.Host,
		http: &http.Client{
			Transport: transport,
			Timeout:   8 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c *TerminalArchiveClient) Close() {
	if c != nil && c.http != nil {
		c.http.CloseIdleConnections()
	}
}

func (c *TerminalArchiveClient) call(ctx context.Context, method, path string, body any, out any) error {
	if ctx == nil {
		return &Error{Status: 422}
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return &Error{Status: 422}
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return &Error{Status: 503}
	}
	req.Header.Set("Authorization", "Bearer "+c.config.Key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &Error{Status: 503}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case 401, 403, 404, 409, 422, 429:
			return &Error{Status: resp.StatusCode}
		default:
			return &Error{Status: 503}
		}
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return &Error{Status: 502}
	}
	encoded, err := io.ReadAll(io.LimitReader(resp.Body, archiveResponseLimit+1))
	if err != nil || len(encoded) > archiveResponseLimit {
		return &Error{Status: 502}
	}
	var envelope struct {
		Data      json.RawMessage `json:"data"`
		RequestID *string         `json:"requestId"`
	}
	if strictObject(encoded, &envelope) != nil || len(envelope.Data) == 0 || bytes.Equal(envelope.Data, []byte("null")) ||
		envelope.RequestID == nil || !exactText(*envelope.RequestID, 128) || strictObject(envelope.Data, out) != nil {
		return &Error{Status: 502}
	}
	return nil
}

// CheckScope verifies the configured history service identity before callers
// register the archive client. This metadata route returns no participants or
// delegated routes and does not inspect a player record.
func (c *TerminalArchiveClient) CheckScope(ctx context.Context) error {
	if c == nil || c.http == nil || ctx == nil {
		return &Error{Status: 422}
	}
	var scope struct {
		ServiceID      string   `json:"serviceId"`
		ApplicationID  string   `json:"applicationId"`
		IdentityIssuer string   `json:"identityIssuer"`
		Operations     []string `json:"operations"`
	}
	if err := c.call(ctx, http.MethodGet, "/business/v1/history/service", nil, &scope); err != nil {
		return err
	}
	if !opaqueID.MatchString(scope.ServiceID) || scope.ServiceID != c.config.ServiceID ||
		scope.ApplicationID != c.config.ApplicationID || scope.IdentityIssuer != c.config.IdentityIssuer {
		return &Error{Status: 502}
	}
	seen := make(map[string]bool, len(scope.Operations))
	read := false
	for _, operation := range scope.Operations {
		if seen[operation] {
			return &Error{Status: 502}
		}
		seen[operation] = true
		switch operation {
		case "read":
			read = true
		case "cancel", "assignment", "resume":
		default:
			return &Error{Status: 502}
		}
	}
	if !read {
		return &Error{Status: 403}
	}
	return nil
}

func (c *TerminalArchiveClient) validReservation(r Reservation, allocationID string) bool {
	if !opaqueID.MatchString(r.ReservationID) || !opaqueID.MatchString(r.AllocationID) || r.AllocationID != allocationID ||
		!opaqueID.MatchString(r.RoomID) || r.ApplicationID != c.config.ApplicationID || !opaqueID.MatchString(r.PlacementID) ||
		!opaqueID.MatchString(r.RevisionID) || r.Region != c.config.Region || r.CreatedAt.IsZero() || r.CreatedAt.UnixMilli() <= 0 ||
		r.UpdatedAt.IsZero() || r.UpdatedAt.UnixMilli() <= 0 || r.UpdatedAt.Before(r.CreatedAt) {
		return false
	}
	switch r.State {
	case "completed":
		return r.FailureCode == ""
	case "technical_aborted":
		return r.FailureCode == "host_process_terminated"
	default:
		return false
	}
}

// Status reads only a terminal historical reservation for the exact supplied
// allocation and authenticated participant identity.
func (c *TerminalArchiveClient) Status(ctx context.Context, allocationID, authenticatedUser string) (ReservationStatus, error) {
	var out ReservationStatus
	if c == nil || c.http == nil || ctx == nil || !opaqueID.MatchString(allocationID) || !exactText(authenticatedUser, 256) {
		return out, &Error{Status: 422}
	}
	body := struct {
		Version       string `json:"version"`
		ParticipantID string `json:"participantId"`
	}{RoomVersion, authenticatedUser}
	err := c.call(ctx, http.MethodPost, "/business/v1/history/archive/reservations/"+allocationID+"/status", body, &out)
	if err == nil && !c.validReservation(out.Reservation, allocationID) {
		err = &Error{Status: 502}
	}
	return out, err
}

func (c *TerminalArchiveClient) validSearch(s Search, searchID string) bool {
	ttl := s.ExpiresAt.Sub(s.CreatedAt)
	if !attemptID.MatchString(s.ID) || s.ID != searchID || s.Region != c.config.Region || s.Compatibility != c.config.Compatibility ||
		s.CreatedAt.IsZero() || s.CreatedAt.UnixMilli() <= 0 || s.ExpiresAt.IsZero() || s.ExpiresAt.UnixMilli() <= 0 ||
		ttl < 30*time.Second || ttl > 600*time.Second || ttl%time.Second != 0 || s.ResolvedAt == nil ||
		s.ResolvedAt.IsZero() || s.ResolvedAt.UnixMilli() <= 0 || s.ResolvedAt.Before(s.CreatedAt) {
		return false
	}
	noRoom := s.AllocationID == "" && s.Reservation == nil
	switch s.State {
	case "cancelled":
		return s.ResolvedAt.Before(s.ExpiresAt) && noRoom
	case "expired":
		return !s.ResolvedAt.Before(s.ExpiresAt) && noRoom
	case "bound":
		return s.ResolvedAt.Before(s.ExpiresAt) && opaqueID.MatchString(s.AllocationID) && s.Reservation != nil &&
			s.Reservation.AllocationID == s.AllocationID && s.Reservation.CreatedAt.Equal(*s.ResolvedAt) &&
			c.validReservation(*s.Reservation, s.AllocationID)
	default:
		return false
	}
}

// SearchStatus reads one exact historical search. Pending searches and bound
// searches without matching terminal reservation evidence fail closed.
func (c *TerminalArchiveClient) SearchStatus(ctx context.Context, searchID, authenticatedUser string) (SearchStatus, error) {
	var out SearchStatus
	if c == nil || c.http == nil || ctx == nil || !attemptID.MatchString(searchID) || !exactText(authenticatedUser, 256) {
		return out, &Error{Status: 422}
	}
	body := struct {
		Version       string `json:"version"`
		ParticipantID string `json:"participantId"`
	}{SearchVersion, authenticatedUser}
	err := c.call(ctx, http.MethodPost, "/business/v1/history/archive/searches/"+searchID+"/status", body, &out)
	if err == nil && !c.validSearch(out.Search, searchID) {
		err = &Error{Status: 502}
	}
	return out, err
}
