package gamefleet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const terminalArchiveTestServiceID = "history-service-123"

func terminalArchiveKey() string {
	return HistoryServiceKeyPrefix + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x9b}, 32))
}

func terminalArchiveConfig(origin string) TerminalArchiveConfig {
	return TerminalArchiveConfig{
		URL: origin, Key: terminalArchiveKey(), ServiceID: terminalArchiveTestServiceID,
		ApplicationID: "history-app", IdentityIssuer: "issuer-history-01",
		Region: "eu-west", Compatibility: "dm-compat-v3",
	}
}

func mustTerminalArchiveClient(t *testing.T, cfg TerminalArchiveConfig) *TerminalArchiveClient {
	t.Helper()
	c, err := NewTerminalArchiveClient(cfg)
	if err != nil {
		t.Fatal("create terminal archive client:", err)
	}
	t.Cleanup(c.Close)
	return c
}

func archiveTestReservation(state string) Reservation {
	created := time.Date(2025, 8, 7, 6, 5, 4, 0, time.UTC)
	r := Reservation{
		ReservationID: "reservation-old-001", AllocationID: "allocation-old-001", RoomID: "room-old-001",
		ApplicationID: "history-app", PlacementID: "placement-before-rollout", RevisionID: "revision-before-rollout",
		Region: "eu-west", State: state, CancellationRequested: false,
		CreatedAt: created, UpdatedAt: created.Add(time.Minute),
	}
	if state == "technical_aborted" {
		r.FailureCode = "host_process_terminated"
	}
	return r
}

func archiveTestSearch(state string) Search {
	created := time.Date(2025, 8, 7, 6, 0, 0, 0, time.UTC)
	s := Search{ID: "search-old-0001", State: state, Region: "eu-west", Compatibility: "dm-compat-v3",
		CreatedAt: created, ExpiresAt: created.Add(120 * time.Second)}
	switch state {
	case "cancelled":
		resolved := created.Add(time.Second)
		s.ResolvedAt = &resolved
	case "expired":
		resolved := s.ExpiresAt
		s.ResolvedAt = &resolved
	case "bound":
		resolved := created.Add(10 * time.Second)
		r := archiveTestReservation("completed")
		r.AllocationID = "allocation-search-001"
		r.CreatedAt = resolved
		r.UpdatedAt = resolved.Add(time.Minute)
		s.ResolvedAt, s.AllocationID, s.Reservation = &resolved, r.AllocationID, &r
	}
	return s
}

func rawArchiveEnvelope(data string) []byte {
	return []byte(`{"data":` + data + `,"requestId":"archive-test"}`)
}

func rawArchiveResponse(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func TestTerminalArchiveConfigRequiresLoopbackOriginAndHistoryKeyClass(t *testing.T) {
	validOrigin := "http://127.0.0.1:17682"
	if c, err := NewTerminalArchiveClient(terminalArchiveConfig(validOrigin)); err != nil {
		t.Fatal("valid terminal archive config rejected:", err)
	} else {
		c.Close()
	}
	if c, err := NewTerminalArchiveClient(terminalArchiveConfig("http://[::1]:17682/")); err != nil {
		t.Fatal("canonical IPv6 loopback origin rejected:", err)
	} else {
		c.Close()
	}

	for _, origin := range []string{
		"", "https://127.0.0.1:17682", "http://localhost:17682", "http://192.0.2.1:17682",
		"http://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:99999", "http://127.0.0.1:017682",
		"http://user:pass@127.0.0.1:17682", "http://127.0.0.1:17682/business", "http://127.0.0.1:17682?x=1",
		"http://127.0.0.1:17682?", "http://127.0.0.1:17682/#fragment", "http://[::1%25lo0]:17682",
		"http://127.0.0.1:17682/%2f", "http://127.0.0.01:17682",
	} {
		t.Run("origin/"+origin, func(t *testing.T) {
			if c, err := NewTerminalArchiveClient(terminalArchiveConfig(origin)); err == nil {
				c.Close()
				t.Fatal("non-canonical or non-loopback origin accepted")
			}
		})
	}

	for name, change := range map[string]func(*TerminalArchiveConfig){
		"direct-business-key": func(c *TerminalArchiveConfig) { c.Key = "gfbiz_" + strings.Repeat("a", 43) },
		"wrong-prefix":        func(c *TerminalArchiveConfig) { c.Key = "gfsvc_" + strings.Repeat("a", 43) },
		"noncanonical-base64": func(c *TerminalArchiveConfig) { c.Key = HistoryServiceKeyPrefix + strings.Repeat("a", 42) + "=" },
		"short-key":           func(c *TerminalArchiveConfig) { c.Key = HistoryServiceKeyPrefix + strings.Repeat("a", 42) },
		"missing-service-id":  func(c *TerminalArchiveConfig) { c.ServiceID = "" },
		"invalid-service-id":  func(c *TerminalArchiveConfig) { c.ServiceID = "../service" },
		"bad-app":             func(c *TerminalArchiveConfig) { c.ApplicationID = "bad/app" },
		"missing-issuer":      func(c *TerminalArchiveConfig) { c.IdentityIssuer = "" },
		"bad-issuer":          func(c *TerminalArchiveConfig) { c.IdentityIssuer = "issuer with space" },
		"missing-region":      func(c *TerminalArchiveConfig) { c.Region = "" },
		"bad-region":          func(c *TerminalArchiveConfig) { c.Region = "eu-west\n" },
		"missing-compat":      func(c *TerminalArchiveConfig) { c.Compatibility = " " },
	} {
		t.Run("scope/"+name, func(t *testing.T) {
			cfg := terminalArchiveConfig(validOrigin)
			change(&cfg)
			if c, err := NewTerminalArchiveClient(cfg); err == nil {
				c.Close()
				t.Fatal("invalid history scope or credential class accepted")
			}
		})
	}
}

func TestTerminalArchiveScopeAndStatusUseOnlyApprovedRoutes(t *testing.T) {
	const user = "steam:verified-historical-user"
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+terminalArchiveKey() ||
			r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" || r.Header.Get("Accept") != "application/json" ||
			(r.Method == http.MethodPost && r.Header.Get("Content-Type") != "application/json") ||
			(r.Method == http.MethodGet && r.Header.Get("Content-Type") != "") {
			t.Errorf("unexpected archive request metadata: method=%s path=%s headers=%v", r.Method, r.URL.Path, r.Header)
		}
		switch r.URL.Path {
		case "/business/v1/history/service":
			if r.Method != http.MethodGet {
				t.Errorf("scope check must use GET, got %s", r.Method)
			}
			body, _ := io.ReadAll(r.Body)
			if len(body) != 0 {
				t.Errorf("scope check must not send a body: %s", body)
			}
			respond(w, map[string]any{"serviceId": terminalArchiveTestServiceID, "applicationId": "history-app", "identityIssuer": "issuer-history-01", "operations": []string{"read", "cancel"}})
		case "/business/v1/history/archive/reservations/allocation-old-001/status":
			if r.Method != http.MethodPost {
				t.Errorf("reservation status must use POST, got %s", r.Method)
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode request body: %v", err)
			}
			want := map[string]string{"version": RoomVersion, "participantId": user}
			if !reflect.DeepEqual(body, want) {
				t.Errorf("reservation status payload=%v want=%v", body, want)
			}
			respond(w, ReservationStatus{Reservation: archiveTestReservation("completed")})
		case "/business/v1/history/archive/searches/search-old-0001/status":
			if r.Method != http.MethodPost {
				t.Errorf("search status must use POST, got %s", r.Method)
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode request body: %v", err)
			}
			want := map[string]string{"version": SearchVersion, "participantId": user}
			if !reflect.DeepEqual(body, want) {
				t.Errorf("search status payload=%v want=%v", body, want)
			}
			respond(w, SearchStatus{Search: archiveTestSearch("bound")})
		default:
			t.Errorf("archive client called an unapproved route: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := mustTerminalArchiveClient(t, terminalArchiveConfig(server.URL))
	if err := c.CheckScope(context.Background()); err != nil {
		t.Fatalf("configured history scope check: %v", err)
	}
	reservation, err := c.Status(context.Background(), "allocation-old-001", user)
	if err != nil || reservation.Reservation.State != "completed" || reservation.Reservation.PlacementID != "placement-before-rollout" || reservation.Reservation.RevisionID != "revision-before-rollout" {
		t.Fatalf("historical reservation status: %+v err=%v", reservation, err)
	}
	search, err := c.SearchStatus(context.Background(), "search-old-0001", user)
	if err != nil || search.Search.State != "bound" || search.Search.Reservation == nil || search.Search.Reservation.State != "completed" {
		t.Fatalf("historical bound search status: %+v err=%v", search, err)
	}
	wantPaths := []string{
		"/business/v1/history/service",
		"/business/v1/history/archive/reservations/allocation-old-001/status",
		"/business/v1/history/archive/searches/search-old-0001/status",
	}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("terminal archive requests=%v want=%v", paths, wantPaths)
	}
}

func TestTerminalArchiveCheckScopeRequiresExactServiceApplicationIssuerAndRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		data any
		want int
	}{
		{name: "valid", data: map[string]any{"serviceId": terminalArchiveTestServiceID, "applicationId": "history-app", "identityIssuer": "issuer-history-01", "operations": []string{"read"}}, want: 0},
		{name: "different pinned service", data: map[string]any{"serviceId": "history-service-124", "applicationId": "history-app", "identityIssuer": "issuer-history-01", "operations": []string{"read"}}, want: 502},
		{name: "wrong service application", data: map[string]any{"serviceId": terminalArchiveTestServiceID, "applicationId": "primary-app", "identityIssuer": "issuer-history-01", "operations": []string{"read"}}, want: 502},
		{name: "wrong issuer", data: map[string]any{"serviceId": terminalArchiveTestServiceID, "applicationId": "history-app", "identityIssuer": "issuer-primary-01", "operations": []string{"read"}}, want: 502},
		{name: "invalid service id", data: map[string]any{"serviceId": "../service", "applicationId": "history-app", "identityIssuer": "issuer-history-01", "operations": []string{"read"}}, want: 502},
		{name: "missing read", data: map[string]any{"serviceId": terminalArchiveTestServiceID, "applicationId": "history-app", "identityIssuer": "issuer-history-01", "operations": []string{"cancel"}}, want: 403},
		{name: "unknown operation", data: map[string]any{"serviceId": terminalArchiveTestServiceID, "applicationId": "history-app", "identityIssuer": "issuer-history-01", "operations": []string{"read", "reserve"}}, want: 502},
		{name: "duplicate operation", data: map[string]any{"serviceId": terminalArchiveTestServiceID, "applicationId": "history-app", "identityIssuer": "issuer-history-01", "operations": []string{"read", "read"}}, want: 502},
		{name: "participants are not returned", data: map[string]any{"serviceId": terminalArchiveTestServiceID, "applicationId": "history-app", "identityIssuer": "issuer-history-01", "operations": []string{"read"}, "allowedParticipants": []string{"player-one"}}, want: 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { respond(w, tc.data) }))
			defer server.Close()
			c := mustTerminalArchiveClient(t, terminalArchiveConfig(server.URL))
			err := c.CheckScope(context.Background())
			if tc.want == 0 {
				if err != nil {
					t.Fatalf("valid service scope rejected: %v", err)
				}
			} else {
				statusError(t, err, tc.want)
			}
		})
	}
}

func TestTerminalArchiveReservationStatusRequiresExactTerminalFacts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Reservation)
		valid  bool
	}{
		{name: "completed", change: func(*Reservation) {}, valid: true},
		{name: "technical abort exact code", change: func(r *Reservation) { r.State, r.FailureCode = "technical_aborted", "host_process_terminated" }, valid: true},
		{name: "historical placement and revision", change: func(r *Reservation) {
			r.PlacementID, r.RevisionID = "placement-historical-44", "revision-historical-44"
		}, valid: true},
		{name: "prepared is not terminal", change: func(r *Reservation) { r.State = "prepared" }},
		{name: "reserved is not terminal", change: func(r *Reservation) { r.State = "reserved" }},
		{name: "unknown state", change: func(r *Reservation) { r.State = "released" }},
		{name: "abort without code", change: func(r *Reservation) { r.State = "technical_aborted" }},
		{name: "abort wrong code", change: func(r *Reservation) { r.State, r.FailureCode = "technical_aborted", "operator_cancelled" }},
		{name: "completed with code", change: func(r *Reservation) { r.FailureCode = "host_process_terminated" }},
		{name: "other allocation", change: func(r *Reservation) { r.AllocationID = "allocation-other-1" }},
		{name: "other application", change: func(r *Reservation) { r.ApplicationID = "another-history-app" }},
		{name: "other region", change: func(r *Reservation) { r.Region = "us-east" }},
		{name: "placement is not opaque", change: func(r *Reservation) { r.PlacementID = "old/placement" }},
		{name: "revision is not opaque", change: func(r *Reservation) { r.RevisionID = "old revision" }},
		{name: "missing reservation ID", change: func(r *Reservation) { r.ReservationID = "" }},
		{name: "missing room ID", change: func(r *Reservation) { r.RoomID = "" }},
		{name: "missing created timestamp", change: func(r *Reservation) { r.CreatedAt = time.Time{} }},
		{name: "pre-epoch timestamp", change: func(r *Reservation) { r.CreatedAt = time.Unix(0, 0).UTC(); r.UpdatedAt = time.Unix(1, 0).UTC() }},
		{name: "updated before created", change: func(r *Reservation) { r.UpdatedAt = r.CreatedAt.Add(-time.Millisecond) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := archiveTestReservation("completed")
			tc.change(&r)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				respond(w, ReservationStatus{Reservation: r})
			}))
			defer server.Close()
			c := mustTerminalArchiveClient(t, terminalArchiveConfig(server.URL))
			_, err := c.Status(context.Background(), "allocation-old-001", "player-authenticated")
			if tc.valid {
				if err != nil {
					t.Fatalf("valid terminal reservation rejected: %v", err)
				}
			} else {
				statusError(t, err, 502)
			}
		})
	}
}

func TestTerminalArchiveSearchStatusRequiresExactTerminalFacts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Search)
		valid  bool
	}{
		{name: "cancelled", change: func(s *Search) { *s = archiveTestSearch("cancelled") }, valid: true},
		{name: "expired", change: func(s *Search) { *s = archiveTestSearch("expired") }, valid: true},
		{name: "bound completed", change: func(s *Search) { *s = archiveTestSearch("bound") }, valid: true},
		{name: "bound technical abort", change: func(s *Search) {
			*s = archiveTestSearch("bound")
			s.Reservation.State, s.Reservation.FailureCode = "technical_aborted", "host_process_terminated"
		}, valid: true},
		{name: "pending rejected", change: func(s *Search) {
			*s = Search{ID: "search-old-0001", State: "pending", Region: "eu-west", Compatibility: "dm-compat-v3", CreatedAt: archiveTestSearch("cancelled").CreatedAt, ExpiresAt: archiveTestSearch("cancelled").ExpiresAt}
		}},
		{name: "unknown state", change: func(s *Search) { s.State = "released" }},
		{name: "wrong ID", change: func(s *Search) { s.ID = "search-other-001" }},
		{name: "wrong region", change: func(s *Search) { s.Region = "us-east" }},
		{name: "wrong compatibility", change: func(s *Search) { s.Compatibility = "another-compat" }},
		{name: "missing resolvedAt", change: func(s *Search) { s.ResolvedAt = nil }},
		{name: "resolved before created", change: func(s *Search) { resolved := s.CreatedAt.Add(-time.Second); s.ResolvedAt = &resolved }},
		{name: "created missing", change: func(s *Search) { s.CreatedAt = time.Time{} }},
		{name: "short TTL", change: func(s *Search) { s.ExpiresAt = s.CreatedAt.Add(29 * time.Second) }},
		{name: "long TTL", change: func(s *Search) { s.ExpiresAt = s.CreatedAt.Add(601 * time.Second) }},
		{name: "fractional TTL", change: func(s *Search) { s.ExpiresAt = s.ExpiresAt.Add(time.Millisecond) }},
		{name: "cancelled at expiry", change: func(s *Search) {
			*s = archiveTestSearch("cancelled")
			resolved := s.ExpiresAt
			s.ResolvedAt = &resolved
		}},
		{name: "cancelled has allocation", change: func(s *Search) { *s = archiveTestSearch("cancelled"); s.AllocationID = "allocation-old-001" }},
		{name: "cancelled has reservation", change: func(s *Search) {
			*s = archiveTestSearch("cancelled")
			r := archiveTestReservation("completed")
			s.Reservation = &r
		}},
		{name: "expired too early", change: func(s *Search) {
			*s = archiveTestSearch("expired")
			resolved := s.ExpiresAt.Add(-time.Millisecond)
			s.ResolvedAt = &resolved
		}},
		{name: "bound without allocation", change: func(s *Search) { *s = archiveTestSearch("bound"); s.AllocationID = "" }},
		{name: "bound without reservation", change: func(s *Search) { *s = archiveTestSearch("bound"); s.Reservation = nil }},
		{name: "bound allocation mismatch", change: func(s *Search) { *s = archiveTestSearch("bound"); s.AllocationID = "allocation-other-001" }},
		{name: "bound room is not terminal", change: func(s *Search) { *s = archiveTestSearch("bound"); s.Reservation.State = "prepared" }},
		{name: "bound completion has failure code", change: func(s *Search) {
			*s = archiveTestSearch("bound")
			s.Reservation.FailureCode = "host_process_terminated"
		}},
		{name: "bound commit timestamp differs", change: func(s *Search) {
			*s = archiveTestSearch("bound")
			s.Reservation.CreatedAt = s.Reservation.CreatedAt.Add(time.Millisecond)
		}},
		{name: "bound application mismatch", change: func(s *Search) { *s = archiveTestSearch("bound"); s.Reservation.ApplicationID = "another-history-app" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := archiveTestSearch("bound")
			tc.change(&s)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				respond(w, SearchStatus{Search: s})
			}))
			defer server.Close()
			c := mustTerminalArchiveClient(t, terminalArchiveConfig(server.URL))
			_, err := c.SearchStatus(context.Background(), "search-old-0001", "player-authenticated")
			if tc.valid {
				if err != nil {
					t.Fatalf("valid terminal search rejected: %v", err)
				}
			} else {
				statusError(t, err, 502)
			}
		})
	}
}

func TestTerminalArchiveRejectsMalformedEnvelopesUnknownFieldsAndDuplicateProperties(t *testing.T) {
	validReservation, err := json.Marshal(ReservationStatus{Reservation: archiveTestReservation("completed")})
	if err != nil {
		t.Fatal(err)
	}
	validSearch, err := json.Marshal(SearchStatus{Search: archiveTestSearch("bound")})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data []byte
		path string
	}{
		{name: "reservation unknown ticket field", data: rawArchiveEnvelope(`{"reservation":{"reservationId":"reservation-old-001","allocationId":"allocation-old-001","roomId":"room-old-001","applicationId":"history-app","placementId":"placement-before-rollout","revisionId":"revision-before-rollout","region":"eu-west","state":"completed","cancellationRequested":false,"createdAt":"2025-08-07T06:05:04Z","updatedAt":"2025-08-07T06:06:04Z","ticket":"gft1.fake"}}`), path: "reservation"},
		{name: "reservation duplicate state", data: rawArchiveEnvelope(`{"reservation":{"reservationId":"reservation-old-001","allocationId":"allocation-old-001","roomId":"room-old-001","applicationId":"history-app","placementId":"placement-before-rollout","revisionId":"revision-before-rollout","region":"eu-west","state":"completed","STATE":"technical_aborted","cancellationRequested":false,"createdAt":"2025-08-07T06:05:04Z","updatedAt":"2025-08-07T06:06:04Z"}}`), path: "reservation"},
		{name: "reservation missing required cancellation bit", data: rawArchiveEnvelope(`{"reservation":{"reservationId":"reservation-old-001","allocationId":"allocation-old-001","roomId":"room-old-001","applicationId":"history-app","placementId":"placement-before-rollout","revisionId":"revision-before-rollout","region":"eu-west","state":"completed","createdAt":"2025-08-07T06:05:04Z","updatedAt":"2025-08-07T06:06:04Z"}}`), path: "reservation"},
		{name: "search unknown ticket field", data: rawArchiveEnvelope(`{"search":{"searchId":"search-old-0001","state":"bound","region":"eu-west","compatibility":"dm-compat-v3","createdAt":"2025-08-07T06:00:00Z","expiresAt":"2025-08-07T06:02:00Z","resolvedAt":"2025-08-07T06:00:10Z","allocationId":"allocation-search-001","reservation":{"reservationId":"reservation-old-001","allocationId":"allocation-search-001","roomId":"room-old-001","applicationId":"history-app","placementId":"placement-before-rollout","revisionId":"revision-before-rollout","region":"eu-west","state":"completed","cancellationRequested":false,"createdAt":"2025-08-07T06:00:10Z","updatedAt":"2025-08-07T06:01:10Z"},"ticket":"private-ticket"}}`), path: "search"},
		{name: "missing request id", data: []byte(`{"data":{"reservation":{}}}`), path: "reservation"},
		{name: "envelope unknown field", data: []byte(`{"data":{"reservation":{}},"requestId":"r","debug":"private"}`), path: "reservation"},
		{name: "envelope duplicate data", data: []byte(`{"data":{"reservation":{}},"DATA":{"reservation":{}},"requestId":"r"}`), path: "reservation"},
		{name: "reservation fixture control", data: rawArchiveEnvelope(string(validReservation)), path: "reservation-valid-control"},
		{name: "search fixture control", data: rawArchiveEnvelope(string(validSearch)), path: "search-valid-control"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { rawArchiveResponse(w, tc.data) }))
			defer server.Close()
			c := mustTerminalArchiveClient(t, terminalArchiveConfig(server.URL))
			var callErr error
			switch tc.path {
			case "search":
				_, callErr = c.SearchStatus(context.Background(), "search-old-0001", "player-authenticated")
			case "search-valid-control":
				_, callErr = c.SearchStatus(context.Background(), "search-old-0001", "player-authenticated")
			case "reservation-valid-control":
				_, callErr = c.Status(context.Background(), "allocation-old-001", "player-authenticated")
			default:
				_, callErr = c.Status(context.Background(), "allocation-old-001", "player-authenticated")
			}
			if strings.HasSuffix(tc.path, "valid-control") {
				if callErr != nil {
					t.Fatalf("valid control rejected: %v", callErr)
				}
				return
			}
			statusError(t, callErr, 502)
		})
	}
}

func TestTerminalArchiveRejectsInvalidIDsAndUsersBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		respond(w, ReservationStatus{Reservation: archiveTestReservation("completed")})
	}))
	defer server.Close()
	c := mustTerminalArchiveClient(t, terminalArchiveConfig(server.URL))
	for _, id := range []string{"", "../allocation", "allocation/other", strings.Repeat("a", 129)} {
		_, err := c.Status(context.Background(), id, "trusted-user")
		statusError(t, err, 422)
	}
	for _, id := range []string{"", "short", "search/path", strings.Repeat("s", 129)} {
		_, err := c.SearchStatus(context.Background(), id, "trusted-user")
		statusError(t, err, 422)
	}
	for _, user := range []string{"", " player ", strings.Repeat("u", 257), "bad\nuser"} {
		_, err := c.Status(context.Background(), "allocation-old-001", user)
		statusError(t, err, 422)
		_, err = c.SearchStatus(context.Background(), "search-old-0001", user)
		statusError(t, err, 422)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid IDs/user caused %d network requests", calls.Load())
	}
}

func TestTerminalArchiveTransportIgnoresProxyAndDoesNotFollowRedirectsOrLeakOversizedBodies(t *testing.T) {
	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("https_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	valid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respond(w, ReservationStatus{Reservation: archiveTestReservation("completed")})
	}))
	defer valid.Close()
	c := mustTerminalArchiveClient(t, terminalArchiveConfig(valid.URL))
	if _, err := c.Status(context.Background(), "allocation-old-001", "trusted-user"); err != nil {
		t.Fatalf("direct loopback call under poison proxy env: %v", err)
	}
	if proxyCalls.Load() != 0 {
		t.Fatalf("archive credential went through ambient proxy %d time(s)", proxyCalls.Load())
	}

	var redirectedCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedCalls.Add(1)
		respond(w, ReservationStatus{Reservation: archiveTestReservation("completed")})
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	redirectClient := mustTerminalArchiveClient(t, terminalArchiveConfig(redirect.URL))
	_, err := redirectClient.Status(context.Background(), "allocation-old-001", "trusted-user")
	statusError(t, err, 503)
	if redirectedCalls.Load() != 0 || strings.Contains(err.Error(), target.URL) || strings.Contains(err.Error(), terminalArchiveKey()) {
		t.Fatalf("redirect was followed or error leaked private details: calls=%d err=%v", redirectedCalls.Load(), err)
	}

	const privateBody = "oversized-private-upstream-response"
	oversized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"reservation":{}} ,"requestId":"`+strings.Repeat("x", archiveResponseLimit)+privateBody+`"}`)
	}))
	defer oversized.Close()
	oversizedClient := mustTerminalArchiveClient(t, terminalArchiveConfig(oversized.URL))
	_, err = oversizedClient.Status(context.Background(), "allocation-old-001", "trusted-user")
	statusError(t, err, 502)
	if strings.Contains(err.Error(), privateBody) || strings.Contains(err.Error(), terminalArchiveKey()) {
		t.Fatalf("oversize error leaked response or credential: %v", err)
	}

	failure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, privateBody)
	}))
	defer failure.Close()
	failureClient := mustTerminalArchiveClient(t, terminalArchiveConfig(failure.URL))
	_, err = failureClient.Status(context.Background(), "allocation-old-001", "trusted-user")
	statusError(t, err, 503)
	if strings.Contains(err.Error(), privateBody) {
		t.Fatalf("upstream body leaked from sanitized error: %v", err)
	}
}

func TestTerminalArchiveContextCancellationReturnsSanitizedError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "private transport response")
	}))
	defer server.Close()
	c := mustTerminalArchiveClient(t, terminalArchiveConfig(server.URL))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Status(ctx, "allocation-old-001", "trusted-user")
	statusError(t, err, 503)
	if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "private") {
		t.Fatalf("transport error leaked detail: %v", err)
	}
}

func TestTerminalArchiveNoCurrentOrMutationMethods(t *testing.T) {
	// Compile-time shape assertion: this client intentionally offers only the
	// terminal reads and Close; do not make it implement Backend.
	var _ interface {
		Status(context.Context, string, string) (ReservationStatus, error)
		SearchStatus(context.Context, string, string) (SearchStatus, error)
		Close()
	} = (*TerminalArchiveClient)(nil)
	if _, ok := any((*TerminalArchiveClient)(nil)).(Backend); ok {
		t.Fatal("terminal archive client accidentally gained active/mutation backend authority")
	}
}

func TestTerminalArchiveInvalidEnvelopeErrorsStayStatusOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rawArchiveResponse(w, rawArchiveEnvelope(`{"reservation":{"state":"technical_aborted","failureCode":"private-secret"}}`))
	}))
	defer server.Close()
	c := mustTerminalArchiveClient(t, terminalArchiveConfig(server.URL))
	_, err := c.Status(context.Background(), "allocation-old-001", "trusted-user")
	statusError(t, err, 502)
	if err.Error() != "gamefleet request failed" || strings.Contains(err.Error(), "private-secret") || strings.Contains(err.Error(), terminalArchiveKey()) {
		t.Fatalf("validation error was not sanitized: %v", err)
	}
}

func TestTerminalArchiveOversizedInputIsBounded(t *testing.T) {
	// Keep one explicit size boundary test alongside response-limit coverage.
	user := strings.Repeat("u", 257)
	c, err := NewTerminalArchiveClient(terminalArchiveConfig("http://127.0.0.1:17682"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Status(context.Background(), "allocation-old-001", user)
	statusError(t, err, 422)
}

func TestTerminalArchiveMalformedResponseDoesNotExposeWireBody(t *testing.T) {
	const wireSecret = "this-body-must-not-escape"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rawArchiveResponse(w, rawArchiveEnvelope(fmt.Sprintf(`{"reservation":{"debug":"%s"}}`, wireSecret)))
	}))
	defer server.Close()
	c := mustTerminalArchiveClient(t, terminalArchiveConfig(server.URL))
	_, err := c.Status(context.Background(), "allocation-old-001", "trusted-user")
	statusError(t, err, 502)
	if strings.Contains(err.Error(), wireSecret) {
		t.Fatalf("malformed response body leaked through error: %v", err)
	}
}
