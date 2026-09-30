package gamefleet

import (
	"bytes"
	"context"
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
	"unicode"
	"unicode/utf8"
)

var opaqueID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var attemptID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)
var playerToken = regexp.MustCompile(`^gft1\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{86}$`)

type Config struct {
	URL           string
	Key           string
	ApplicationID string
	PlacementID   string
	RevisionID    string
	Region        string
	Compatibility string
}

type Client struct {
	config Config
	base   string
	http   *http.Client
}

func exactText(s string, max int) bool {
	return s != "" && len(s) <= max && utf8.ValidString(s) && strings.TrimSpace(s) == s && !strings.ContainsFunc(s, unicode.IsControl)
}

// NewClient only permits an explicit loopback origin. Cross-VPS traffic must
// use a separately managed SSH local forward. Proxy variables and redirects
// cannot move the machine credential to another destination.
func NewClient(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u == nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return nil, errors.New("GAMEFLEET_BUSINESS_URL must be an explicit loopback HTTP origin")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	port, perr := strconv.Atoi(u.Port())
	if err != nil || !ip.IsLoopback() || ip.Zone() != "" || perr != nil || port < 1 || port > 65535 || u.Host != net.JoinHostPort(ip.String(), strconv.Itoa(port)) {
		return nil, errors.New("GAMEFLEET_BUSINESS_URL requires a canonical loopback IP and port")
	}
	if !opaqueID.MatchString(cfg.ApplicationID) || !opaqueID.MatchString(cfg.PlacementID) || !opaqueID.MatchString(cfg.RevisionID) || !exactText(cfg.Region, 128) || !exactText(cfg.Compatibility, 128) {
		return nil, errors.New("GameFleet application, placement, revision, region and compatibility are required")
	}
	if len(cfg.Key) < 16 || len(cfg.Key) > 4096 || strings.ContainsFunc(cfg.Key, func(r rune) bool { return r < 33 || r > 126 }) {
		return nil, errors.New("invalid GameFleet business key")
	}
	transport := &http.Transport{
		Proxy: nil, DialContext: (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxConnsPerHost: 16, MaxIdleConns: 16, MaxIdleConnsPerHost: 16,
		IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 5 * time.Second,
		MaxResponseHeaderBytes: 16 << 10, DisableCompression: true,
	}
	return &Client{config: cfg, base: "http://" + u.Host, http: &http.Client{
		Transport: transport, Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

// CheckScope is run before registering any runtime hooks. A wrong credential
// or missing permission fails startup rather than silently selecting a pool.
func (c *Client) CheckScope(ctx context.Context) error {
	var scope Scope
	if err := c.call(ctx, http.MethodGet, "/business/v1/caller", nil, &scope, 200); err != nil {
		return err
	}
	if !opaqueID.MatchString(scope.CallerID) || scope.ApplicationID != c.config.ApplicationID || scope.PlacementID != c.config.PlacementID || scope.RevisionID != c.config.RevisionID || scope.Region != c.config.Region {
		return &Error{Status: 502}
	}
	ops := map[string]bool{}
	for _, op := range scope.Operations {
		ops[op] = true
	}
	for _, op := range []string{"reserve", "read", "cancel", "assignment", "resume"} {
		if !ops[op] {
			return &Error{Status: 403}
		}
	}
	return nil
}

func (c *Client) call(ctx context.Context, method, path string, body, out any, statuses ...int) error {
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
	ok := false
	for _, status := range statuses {
		if resp.StatusCode == status {
			ok = true
		}
	}
	if !ok {
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
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		return &Error{Status: 502}
	}
	var envelope struct {
		Data      json.RawMessage `json:"data"`
		RequestID string          `json:"requestId"`
	}
	if strictObject(raw, &envelope) != nil || len(envelope.Data) == 0 || bytes.Equal(envelope.Data, []byte("null")) || strictObject(envelope.Data, out) != nil {
		return &Error{Status: 502}
	}
	return nil
}

func strictObject(raw []byte, out any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return errors.New("expected object")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("invalid object")
	}
	return nil
}

func (c *Client) validReservation(r Reservation) bool {
	stateValid := r.State == "reserved" || r.State == "prepared" || r.State == "completed" ||
		(r.State == "technical_aborted" && r.FailureCode == "host_process_terminated")
	failureCodeValid := r.State == "technical_aborted" || r.FailureCode == ""
	return opaqueID.MatchString(r.AllocationID) && opaqueID.MatchString(r.ReservationID) && opaqueID.MatchString(r.RoomID) &&
		r.ApplicationID == c.config.ApplicationID && r.PlacementID == c.config.PlacementID && r.RevisionID == c.config.RevisionID && r.Region == c.config.Region &&
		stateValid && failureCodeValid && !r.CreatedAt.IsZero() && !r.UpdatedAt.Before(r.CreatedAt)
}

func (c *Client) Current(ctx context.Context, user string) (CurrentResult, error) {
	var out CurrentResult
	if !exactText(user, 256) {
		return out, &Error{Status: 422}
	}
	// A pointer distinguishes a required null result from a missing field.
	var wire struct {
		Current json.RawMessage `json:"current"`
	}
	err := c.call(ctx, "POST", "/business/v1/reservations/current", map[string]string{"version": RoomVersion, "participantId": user}, &wire, 200)
	if err != nil {
		return out, err
	}
	if bytes.Equal(wire.Current, []byte("null")) {
		return out, nil
	}
	var current struct {
		Reservation          Reservation `json:"reservation"`
		ConnectionGeneration *int64      `json:"connectionGeneration"`
	}
	if strictObject(wire.Current, &current) != nil || current.ConnectionGeneration == nil || !c.validReservation(current.Reservation) || current.Reservation.State == "completed" || current.Reservation.State == "technical_aborted" || *current.ConnectionGeneration < 0 || *current.ConnectionGeneration > MaxGeneration {
		return out, &Error{Status: 502}
	}
	out.Current = &Current{Reservation: current.Reservation, ConnectionGeneration: *current.ConnectionGeneration}
	return out, nil
}

func (c *Client) Status(ctx context.Context, allocationID, user string) (ReservationStatus, error) {
	var out ReservationStatus
	if !opaqueID.MatchString(allocationID) || !exactText(user, 256) {
		return out, &Error{Status: 422}
	}
	err := c.call(ctx, "POST", "/business/v1/reservations/"+allocationID+"/status", map[string]string{"version": RoomVersion, "participantId": user}, &out, 200)
	if err == nil && (!c.validReservation(out.Reservation) || out.Reservation.AllocationID != allocationID) {
		err = &Error{Status: 502}
	}
	return out, err
}

func (c *Client) Reserve(ctx context.Context, key string, users []string) (ReservationResult, error) {
	var out ReservationResult
	if !attemptID.MatchString(key) || len(users) != 2 || users[0] == users[1] || !exactText(users[0], 256) || !exactText(users[1], 256) {
		return out, &Error{Status: 422}
	}
	body := struct {
		Version       string   `json:"version"`
		Key           string   `json:"idempotencyKey"`
		Participants  []string `json:"participants"`
		Compatibility string   `json:"compatibility"`
	}{RoomVersion, key, users, c.config.Compatibility}
	err := c.call(ctx, "POST", "/business/v1/reservations", body, &out, 200, 202)
	if err == nil && !c.validReservation(out.Reservation) {
		err = &Error{Status: 502}
	}
	return out, err
}

func (c *Client) Cancel(ctx context.Context, id string) (ReservationResult, error) {
	var out ReservationResult
	if !opaqueID.MatchString(id) {
		return out, &Error{Status: 422}
	}
	err := c.call(ctx, "POST", "/business/v1/reservations/"+id+"/cancel", struct{}{}, &out, 200)
	if err == nil && (!c.validReservation(out.Reservation) || out.Reservation.AllocationID != id) {
		err = &Error{Status: 502}
	}
	return out, err
}

func (c *Client) Issue(ctx context.Context, id, user, key string, previous int64, resume bool) (AssignmentResult, error) {
	var out AssignmentResult
	if !opaqueID.MatchString(id) || !exactText(user, 256) || !attemptID.MatchString(key) || previous < 0 || previous >= MaxGeneration || (!resume && previous != 0) || (resume && previous == 0) {
		return out, &Error{Status: 422}
	}
	action := "assignment"
	if resume {
		action = "resume"
	}
	body := struct {
		Version     string `json:"version"`
		Key         string `json:"idempotencyKey"`
		Participant string `json:"participantId"`
		Previous    int64  `json:"previousConnectionGeneration"`
	}{TicketVersion, key, user, previous}
	err := c.call(ctx, "POST", "/business/v1/reservations/"+id+"/"+action, body, &out, 200)
	if err == nil && (!validAssignment(out.Assignment, previous+1) || (out.Assignment.Ticket.State != "issued" && !out.Replay)) {
		err = &Error{Status: 502}
	}
	return out, err
}

func validAssignment(a Assignment, generation int64) bool {
	t := a.Ticket
	if !opaqueID.MatchString(t.ID) || t.Generation != generation || t.ExpiresAt.IsZero() {
		return false
	}
	if t.State == "expired" || t.State == "superseded" {
		return t.Token == "" && a.Endpoint == nil
	}
	if t.State != "issued" || !playerToken.MatchString(t.Token) || len(t.Token) > 6144 || !exactText(t.Token, 6144) || a.Endpoint == nil {
		return false
	}
	ip, err := netip.ParseAddr(a.Endpoint.Address)
	if err != nil || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsLoopback() || len(a.Endpoint.Ports) < 1 || len(a.Endpoint.Ports) > 16 {
		return false
	}
	names := map[string]bool{}
	for _, p := range a.Endpoint.Ports {
		if !exactText(p.Name, 63) || names[p.Name] || (p.Protocol != "UDP" && p.Protocol != "TCP") || p.Port < 1 || p.Port > 65535 {
			return false
		}
		names[p.Name] = true
	}
	return true
}
