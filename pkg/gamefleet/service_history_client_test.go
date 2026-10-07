package gamefleet

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	historyClientTestService = "history-service-0001"
	historyClientTestApp     = "history-app-0001"
	historyClientTestIssuer  = "issuer-west-001"
	historyClientTestUser    = "platform:history-player-001"
	historyClientTestAlloc   = "allocation-history-001"
	historyClientTestSearch  = "search-history-0001"
)

func historyClientTestKey() string {
	return HistoryServiceKeyPrefix + base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("h", 32)))
}

func historyClientTestConfig(origin string) ServiceHistoryConfig {
	return ServiceHistoryConfig{TLS: fixtureTLS,
		URL: origin, Key: historyClientTestKey(), ServiceID: historyClientTestService,
		ApplicationID: historyClientTestApp, IdentityIssuer: historyClientTestIssuer,
		Region: "us-west", Compatibility: "dm-v1",
	}
}

func mustServiceHistoryClient(t *testing.T, cfg ServiceHistoryConfig) *ServiceHistoryClient {
	t.Helper()
	c, err := NewServiceHistoryClient(cfg)
	if err != nil {
		t.Fatal("create service history client")
	}
	t.Cleanup(c.Close)
	return c
}

func serviceHistoryReservation(state string) Reservation {
	created := time.Date(2026, 9, 30, 18, 0, 0, 0, time.UTC)
	r := Reservation{
		ReservationID: "reservation-history-001", AllocationID: historyClientTestAlloc, RoomID: "room-history-001",
		ApplicationID: historyClientTestApp, PlacementID: "old-placement-001", RevisionID: "old-revision-001",
		Region: "us-west", State: state, CreatedAt: created, UpdatedAt: created.Add(time.Minute),
	}
	if state == "technical_aborted" {
		r.FailureCode = "host_process_terminated"
	}
	return r
}

func serviceHistorySearch(state, id string) Search {
	created := time.Date(2026, 9, 30, 18, 0, 0, 0, time.UTC)
	search := Search{
		ID: id, State: state, Region: "us-west", Compatibility: "dm-v1",
		CreatedAt: created, ExpiresAt: created.Add(300 * time.Second),
	}
	switch state {
	case "cancelled":
		resolved := created.Add(time.Minute)
		search.ResolvedAt = &resolved
	case "expired":
		resolved := search.ExpiresAt
		search.ResolvedAt = &resolved
	case "bound":
		resolved := created.Add(time.Minute)
		r := serviceHistoryReservation("completed")
		r.CreatedAt, r.UpdatedAt = resolved, resolved.Add(time.Minute)
		search.ResolvedAt, search.AllocationID, search.Reservation = &resolved, r.AllocationID, &r
	}
	return search
}

func serviceHistoryTicket(previous int64) Assignment {
	return Assignment{
		Ticket: Ticket{
			ID: "ticket-history-001", State: "issued", Token: "gft1.YQ." + strings.Repeat("t", 86),
			Generation: previous + 1, ExpiresAt: time.Date(2026, 9, 30, 19, 0, 0, 0, time.UTC),
		},
		Endpoint: &Endpoint{Address: "203.0.113.7", Ports: []Port{{Name: "game", Protocol: "UDP", Port: 20000}}},
	}
}

func serviceHistoryRespond(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "requestId": "history-test-request-001"})
}

func serviceHistoryRespondRaw(w http.ResponseWriter, status int, contentType string, raw []byte) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func serviceHistoryErrorStatus(t *testing.T, err error, status int, code string) {
	t.Helper()
	var got *ServiceHistoryError
	if !errors.As(err, &got) || got.Status != status || got.Code != code {
		t.Fatalf("service history error class mismatch: got error=%v want status/code=%d/%q", err != nil, status, code)
	}
	if err.Error() != "gamefleet service history request failed" {
		t.Fatal("service history error string exposed wire data")
	}
}

func serviceHistoryCheckBody(t *testing.T, raw []byte, want any) {
	t.Helper()
	var got any
	if json.Unmarshal(raw, &got) != nil {
		t.Error("client sent invalid JSON")
		return
	}
	wantBytes, err := json.Marshal(want)
	if err != nil {
		t.Fatal("marshal expected request")
	}
	var expected any
	if json.Unmarshal(wantBytes, &expected) != nil || !reflect.DeepEqual(got, expected) {
		t.Errorf("request body mismatch: got %v want %v", got, expected)
	}
}

func TestServiceHistoryConfigAndScopeRequireExactGfsvcIdentityAndAllOperations(t *testing.T) {
	server := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/business/v1/history/service" || r.URL.RawQuery != "" ||
			r.Header.Get("Authorization") != "Bearer "+historyClientTestKey() || r.Header.Get("Accept") != "application/json" ||
			r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" || r.Header.Get("Content-Type") != "" {
			t.Error("history scope check used unexpected request metadata")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) != 0 {
			t.Error("history scope check sent a body")
		}
		serviceHistoryRespond(w, http.StatusOK, map[string]any{
			"serviceId": historyClientTestService, "applicationId": historyClientTestApp,
			"identityIssuer": historyClientTestIssuer, "operations": []string{"read", "cancel", "assignment", "resume"},
		})
	}))
	defer server.Close()
	c := mustServiceHistoryClient(t, historyClientTestConfig(server.URL))
	if err := c.CheckScope(context.Background()); err != nil {
		t.Fatalf("valid history service identity rejected: %v", err)
	}

	badKey := historyClientTestConfig(server.URL)
	badKey.Key = "gfbiz_" + strings.Repeat("a", 43)
	if client, err := NewServiceHistoryClient(badKey); err == nil {
		client.Close()
		t.Fatal("ordinary caller key class was accepted by history client")
	}

	for _, tc := range []struct {
		name       string
		service    string
		app        string
		issuer     string
		operations []string
		status     int
	}{
		{name: "wrong-service", service: "other-history-service", app: historyClientTestApp, issuer: historyClientTestIssuer, operations: []string{"read", "cancel", "assignment", "resume"}, status: 502},
		{name: "wrong-application", service: historyClientTestService, app: "other-history-app", issuer: historyClientTestIssuer, operations: []string{"read", "cancel", "assignment", "resume"}, status: 502},
		{name: "wrong-issuer", service: historyClientTestService, app: historyClientTestApp, issuer: "other-issuer-001", operations: []string{"read", "cancel", "assignment", "resume"}, status: 502},
		{name: "missing-read", service: historyClientTestService, app: historyClientTestApp, issuer: historyClientTestIssuer, operations: []string{"cancel", "assignment", "resume"}, status: 403},
		{name: "missing-cancel", service: historyClientTestService, app: historyClientTestApp, issuer: historyClientTestIssuer, operations: []string{"read", "assignment", "resume"}, status: 403},
		{name: "missing-assignment", service: historyClientTestService, app: historyClientTestApp, issuer: historyClientTestIssuer, operations: []string{"read", "cancel", "resume"}, status: 403},
		{name: "missing-resume", service: historyClientTestService, app: historyClientTestApp, issuer: historyClientTestIssuer, operations: []string{"read", "cancel", "assignment"}, status: 403},
		{name: "duplicate-operation", service: historyClientTestService, app: historyClientTestApp, issuer: historyClientTestIssuer, operations: []string{"read", "read", "cancel", "assignment", "resume"}, status: 502},
		{name: "unknown-operation", service: historyClientTestService, app: historyClientTestApp, issuer: historyClientTestIssuer, operations: []string{"read", "cancel", "assignment", "resume", "reserve"}, status: 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			badServer := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				serviceHistoryRespond(w, http.StatusOK, map[string]any{
					"serviceId": tc.service, "applicationId": tc.app, "identityIssuer": tc.issuer, "operations": tc.operations,
				})
			}))
			defer badServer.Close()
			client := mustServiceHistoryClient(t, historyClientTestConfig(badServer.URL))
			serviceHistoryErrorStatus(t, client.CheckScope(context.Background()), tc.status, "")
		})
	}
}

func TestServiceHistoryClientUsesOnlyScopedHistoryRoutesAndCorrectVersions(t *testing.T) {
	var calls atomic.Int32
	server := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+historyClientTestKey() ||
			r.Header.Get("Accept") != "application/json" || r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" {
			t.Error("history request carried unexpected ambient authority")
		}
		if r.Method == http.MethodGet {
			if r.Header.Get("Content-Type") != "" {
				t.Error("history metadata GET set a content type")
			}
		} else if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Error("history POST method or content type mismatch")
		}
		raw, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/business/v1/history/service":
			if r.Method != http.MethodGet || len(raw) != 0 {
				t.Error("scope metadata route mismatch")
			}
			serviceHistoryRespond(w, http.StatusOK, map[string]any{
				"serviceId": historyClientTestService, "applicationId": historyClientTestApp,
				"identityIssuer": historyClientTestIssuer, "operations": []string{"read", "cancel", "assignment", "resume"},
			})
		case "/business/v1/history/reservations/current":
			serviceHistoryCheckBody(t, raw, historyParticipantBody(RoomVersion, historyClientTestUser))
			serviceHistoryRespond(w, http.StatusOK, map[string]any{"current": Current{Reservation: serviceHistoryReservation("prepared"), ConnectionGeneration: 1}})
		case "/business/v1/history/reservations/" + historyClientTestAlloc + "/status":
			serviceHistoryCheckBody(t, raw, historyParticipantBody(RoomVersion, historyClientTestUser))
			serviceHistoryRespond(w, http.StatusOK, ReservationStatus{Reservation: serviceHistoryReservation("prepared")})
		case "/business/v1/history/reservations/" + historyClientTestAlloc + "/cancel":
			serviceHistoryCheckBody(t, raw, historyParticipantBody(RoomVersion, historyClientTestUser))
			reservation := serviceHistoryReservation("prepared")
			reservation.CancellationRequested = true
			serviceHistoryRespond(w, http.StatusOK, ReservationResult{Reservation: reservation, Replay: false})
		case "/business/v1/history/reservations/" + historyClientTestAlloc + "/assignment":
			serviceHistoryCheckBody(t, raw, struct {
				Version                      string `json:"version"`
				IdempotencyKey               string `json:"idempotencyKey"`
				ParticipantID                string `json:"participantId"`
				PreviousConnectionGeneration int64  `json:"previousConnectionGeneration"`
			}{TicketVersion, "history-join-key-001", historyClientTestUser, 0})
			serviceHistoryRespond(w, http.StatusOK, AssignmentResult{Assignment: serviceHistoryTicket(0), Replay: false})
		case "/business/v1/history/reservations/" + historyClientTestAlloc + "/resume":
			serviceHistoryCheckBody(t, raw, struct {
				Version                      string `json:"version"`
				IdempotencyKey               string `json:"idempotencyKey"`
				ParticipantID                string `json:"participantId"`
				PreviousConnectionGeneration int64  `json:"previousConnectionGeneration"`
			}{TicketVersion, "history-resume-key-001", historyClientTestUser, 1})
			serviceHistoryRespond(w, http.StatusOK, AssignmentResult{Assignment: serviceHistoryTicket(1), Replay: true})
		case "/business/v1/history/searches/" + historyClientTestSearch + "/status":
			serviceHistoryCheckBody(t, raw, historyParticipantBody(SearchVersion, historyClientTestUser))
			serviceHistoryRespond(w, http.StatusOK, SearchStatus{Search: serviceHistorySearch("pending", historyClientTestSearch)})
		case "/business/v1/history/searches/" + historyClientTestSearch + "/cancel":
			serviceHistoryCheckBody(t, raw, historyParticipantBody(SearchVersion, historyClientTestUser))
			serviceHistoryRespond(w, http.StatusOK, SearchResult{Search: serviceHistorySearch("cancelled", historyClientTestSearch), Replay: false})
		default:
			t.Errorf("history client called non-history or unapproved route %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c := mustServiceHistoryClient(t, historyClientTestConfig(server.URL))
	ctx := context.Background()
	if err := c.CheckScope(ctx); err != nil {
		t.Fatalf("scope check failed: %v", err)
	}
	current, err := c.Current(ctx, historyClientTestUser)
	if err != nil || current.Current == nil || current.Current.Reservation.PlacementID != "old-placement-001" ||
		current.Current.Reservation.RevisionID != "old-revision-001" {
		t.Fatal("Current rejected an original historical placement or revision")
	}
	status, err := c.Status(ctx, historyClientTestAlloc, historyClientTestUser)
	if err != nil || status.Reservation.AllocationID != historyClientTestAlloc {
		t.Fatal("history reservation status was invalid")
	}
	cancelled, err := c.Cancel(ctx, historyClientTestAlloc, historyClientTestUser)
	if err != nil || cancelled.Reservation.AllocationID != historyClientTestAlloc {
		t.Fatal("history room cancellation intent response was invalid")
	}
	joined, err := c.Issue(ctx, historyClientTestAlloc, historyClientTestUser, "history-join-key-001", 0, false)
	if err != nil || joined.Assignment.Ticket.Generation != 1 || joined.Replay {
		t.Fatal("history assignment response was invalid")
	}
	resumed, err := c.Issue(ctx, historyClientTestAlloc, historyClientTestUser, "history-resume-key-001", 1, true)
	if err != nil || resumed.Assignment.Ticket.Generation != 2 || !resumed.Replay {
		t.Fatal("history resume response was invalid")
	}
	searchStatus, err := c.SearchStatus(ctx, historyClientTestSearch, historyClientTestUser)
	if err != nil || searchStatus.Search.State != "pending" {
		t.Fatal("valid 300-second historical pending search was rejected")
	}
	cancelledSearch, err := c.CancelSearch(ctx, historyClientTestSearch, historyClientTestUser)
	if err != nil || cancelledSearch.Search.State != "cancelled" || cancelledSearch.Search.ID != historyClientTestSearch {
		t.Fatal("history search cancellation response was invalid")
	}
	if calls.Load() != 8 {
		t.Fatalf("history client made %d requests, expected one each for eight allowed routes", calls.Load())
	}
}

func TestServiceHistoryCurrentRequiresExplicitNullAndReturnsZeroOnMalformedCurrent(t *testing.T) {
	for _, tc := range []struct {
		name string
		data any
		want int
	}{
		{name: "explicit-null", data: map[string]any{"current": nil}, want: 200},
		{name: "missing-field", data: map[string]any{}, want: 502},
		{name: "terminal-current", data: map[string]any{"current": Current{Reservation: serviceHistoryReservation("completed"), ConnectionGeneration: 1}}, want: 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				serviceHistoryRespond(w, http.StatusOK, tc.data)
			}))
			defer server.Close()
			client := mustServiceHistoryClient(t, historyClientTestConfig(server.URL))
			got, err := client.Current(context.Background(), historyClientTestUser)
			if tc.want == 200 {
				if err != nil || got.Current != nil {
					t.Fatal("explicit current:null was not returned as an empty routed result")
				}
			} else {
				serviceHistoryErrorStatus(t, err, tc.want, "")
				if !reflect.DeepEqual(got, CurrentResult{}) {
					t.Fatal("invalid Current response returned data")
				}
			}
		})
	}
}

func TestServiceHistoryReservationStatusAcceptsOnlyKnownLifecycleStates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		code  string
		valid bool
	}{
		{name: "reserved", state: "reserved", valid: true},
		{name: "prepared", state: "prepared", valid: true},
		{name: "completed", state: "completed", valid: true},
		{name: "technical-abort", state: "technical_aborted", code: "host_process_terminated", valid: true},
		{name: "completed-with-failure", state: "completed", code: "host_process_terminated"},
		{name: "technical-abort-wrong-code", state: "technical_aborted", code: "other_failure"},
		{name: "unknown-state", state: "server-updating"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reservation := serviceHistoryReservation(tc.state)
				reservation.FailureCode = tc.code
				serviceHistoryRespond(w, http.StatusOK, ReservationStatus{Reservation: reservation})
			}))
			defer server.Close()
			client := mustServiceHistoryClient(t, historyClientTestConfig(server.URL))
			got, err := client.Status(context.Background(), historyClientTestAlloc, historyClientTestUser)
			if tc.valid {
				if err != nil || got.Reservation.State != tc.state {
					t.Fatal("valid historical reservation state was rejected")
				}
			} else {
				serviceHistoryErrorStatus(t, err, http.StatusBadGateway, "")
				if !reflect.DeepEqual(got, ReservationStatus{}) {
					t.Fatal("invalid historical reservation returned data")
				}
			}
		})
	}
}

func TestServiceHistoryStrictResponsesReturnZeroAndRejectMalformedSearchOrTickets(t *testing.T) {
	t.Run("reservation-missing-required-bit", func(t *testing.T) {
		server := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			serviceHistoryRespondRaw(w, http.StatusOK, "application/json", []byte(`{"data":{"reservation":{"reservationId":"reservation-history-001","allocationId":"allocation-history-001","roomId":"room-history-001","applicationId":"history-app-0001","placementId":"old-placement-001","revisionId":"old-revision-001","region":"us-west","state":"prepared","createdAt":"2026-09-30T18:00:00Z","updatedAt":"2026-09-30T18:01:00Z"}},"requestId":"history-test"}`))
		}))
		defer server.Close()
		client := mustServiceHistoryClient(t, historyClientTestConfig(server.URL))
		got, err := client.Status(context.Background(), historyClientTestAlloc, historyClientTestUser)
		serviceHistoryErrorStatus(t, err, http.StatusBadGateway, "")
		if !reflect.DeepEqual(got, ReservationStatus{}) {
			t.Fatal("malformed reservation response returned partial state")
		}
	})

	t.Run("reservation-duplicate-field", func(t *testing.T) {
		server := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			serviceHistoryRespondRaw(w, http.StatusOK, "application/json", []byte(`{"data":{"reservation":{"reservationId":"reservation-history-001","allocationId":"allocation-history-001","allocationId":"allocation-other-001","roomId":"room-history-001","applicationId":"history-app-0001","placementId":"old-placement-001","revisionId":"old-revision-001","region":"us-west","state":"prepared","cancellationRequested":false,"createdAt":"2026-09-30T18:00:00Z","updatedAt":"2026-09-30T18:01:00Z"}},"requestId":"history-test"}`))
		}))
		defer server.Close()
		client := mustServiceHistoryClient(t, historyClientTestConfig(server.URL))
		got, err := client.Status(context.Background(), historyClientTestAlloc, historyClientTestUser)
		serviceHistoryErrorStatus(t, err, http.StatusBadGateway, "")
		if !reflect.DeepEqual(got, ReservationStatus{}) {
			t.Fatal("duplicate reservation field returned state")
		}
	})

	for _, tc := range []struct {
		name   string
		mutate func(*Reservation)
	}{
		{name: "wrong-application", mutate: func(r *Reservation) { r.ApplicationID = "other-app-0001" }},
		{name: "wrong-region", mutate: func(r *Reservation) { r.Region = "eu-west" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reservation := serviceHistoryReservation("prepared")
				tc.mutate(&reservation)
				serviceHistoryRespond(w, http.StatusOK, ReservationStatus{Reservation: reservation})
			}))
			defer server.Close()
			client := mustServiceHistoryClient(t, historyClientTestConfig(server.URL))
			got, err := client.Status(context.Background(), historyClientTestAlloc, historyClientTestUser)
			serviceHistoryErrorStatus(t, err, http.StatusBadGateway, "")
			if !reflect.DeepEqual(got, ReservationStatus{}) {
				t.Fatal("out-of-scope reservation returned data")
			}
		})
	}

	t.Run("wrong-search-compatibility", func(t *testing.T) {
		server := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			search := serviceHistorySearch("pending", historyClientTestSearch)
			search.Compatibility = "other-release"
			serviceHistoryRespond(w, http.StatusOK, SearchStatus{Search: search})
		}))
		defer server.Close()
		client := mustServiceHistoryClient(t, historyClientTestConfig(server.URL))
		got, err := client.SearchStatus(context.Background(), historyClientTestSearch, historyClientTestUser)
		serviceHistoryErrorStatus(t, err, http.StatusBadGateway, "")
		if !reflect.DeepEqual(got, SearchStatus{}) {
			t.Fatal("out-of-scope search returned data")
		}
	})

	t.Run("ticket-token-invalid", func(t *testing.T) {
		server := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			assignment := serviceHistoryTicket(0)
			assignment.Ticket.Token = "private-invalid-token"
			serviceHistoryRespond(w, http.StatusOK, AssignmentResult{Assignment: assignment, Replay: false})
		}))
		defer server.Close()
		client := mustServiceHistoryClient(t, historyClientTestConfig(server.URL))
		got, err := client.Issue(context.Background(), historyClientTestAlloc, historyClientTestUser, "history-join-key-001", 0, false)
		serviceHistoryErrorStatus(t, err, http.StatusBadGateway, "")
		if !reflect.DeepEqual(got, AssignmentResult{}) {
			t.Fatal("invalid ticket response returned token or endpoint")
		}
	})

	t.Run("assignment-missing-replay", func(t *testing.T) {
		server := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			assignment := serviceHistoryTicket(0)
			serviceHistoryRespondRaw(w, http.StatusOK, "application/json", []byte(`{"data":{"assignment":{"ticket":{"id":"`+assignment.Ticket.ID+`","state":"issued","token":"`+assignment.Ticket.Token+`","generation":1,"expiresAt":"2026-09-30T19:00:00Z"},"endpoint":{"address":"203.0.113.7","ports":[{"name":"game","protocol":"UDP","port":20000}]}}},"requestId":"history-test"}`))
		}))
		defer server.Close()
		client := mustServiceHistoryClient(t, historyClientTestConfig(server.URL))
		got, err := client.Issue(context.Background(), historyClientTestAlloc, historyClientTestUser, "history-join-key-001", 0, false)
		serviceHistoryErrorStatus(t, err, http.StatusBadGateway, "")
		if !reflect.DeepEqual(got, AssignmentResult{}) {
			t.Fatal("assignment missing required replay returned a ticket")
		}
	})

	t.Run("cancel-search-pending", func(t *testing.T) {
		server := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			serviceHistoryRespond(w, http.StatusOK, SearchResult{Search: serviceHistorySearch("pending", historyClientTestSearch), Replay: true})
		}))
		defer server.Close()
		client := mustServiceHistoryClient(t, historyClientTestConfig(server.URL))
		got, err := client.CancelSearch(context.Background(), historyClientTestSearch, historyClientTestUser)
		serviceHistoryErrorStatus(t, err, http.StatusBadGateway, "")
		if !reflect.DeepEqual(got, SearchResult{}) {
			t.Fatal("pending search cancel response was returned")
		}
	})
}

func TestServiceHistorySearchLifecycleValidationUsesHistoryTTLAndExactRoomEvidence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		edit  func(*Search)
		valid bool
	}{
		{name: "pending-three-hundred-second-ttl", state: "pending", valid: true},
		{name: "expired-at-deadline", state: "expired", valid: true},
		{name: "bound-old-placement", state: "bound", valid: true},
		{name: "pending-with-room", state: "pending", edit: func(s *Search) { s.AllocationID = historyClientTestAlloc }, valid: false},
		{name: "expired-before-deadline", state: "expired", edit: func(s *Search) { resolved := s.ExpiresAt.Add(-time.Second); s.ResolvedAt = &resolved }, valid: false},
		{name: "bound-allocation-mismatch", state: "bound", edit: func(s *Search) { s.Reservation.AllocationID = "other-allocation-001" }, valid: false},
		{name: "bound-resolution-mismatch", state: "bound", edit: func(s *Search) { s.Reservation.CreatedAt = s.ResolvedAt.Add(time.Second) }, valid: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			search := serviceHistorySearch(tc.state, historyClientTestSearch)
			if tc.edit != nil {
				tc.edit(&search)
			}
			server := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				serviceHistoryRespond(w, http.StatusOK, SearchStatus{Search: search})
			}))
			defer server.Close()
			client := mustServiceHistoryClient(t, historyClientTestConfig(server.URL))
			got, err := client.SearchStatus(context.Background(), historyClientTestSearch, historyClientTestUser)
			if tc.valid {
				if err != nil || got.Search.State != tc.state {
					t.Fatal("valid historical search lifecycle was rejected")
				}
			} else {
				serviceHistoryErrorStatus(t, err, http.StatusBadGateway, "")
				if !reflect.DeepEqual(got, SearchStatus{}) {
					t.Fatal("invalid historical search lifecycle returned partial data")
				}
			}
		})
	}
}

func TestServiceHistoryIssueRequiresExactGenerationStateEndpointAndReplay(t *testing.T) {
	for _, tc := range []struct {
		name    string
		edit    func(*Assignment)
		replay  bool
		wantErr bool
	}{
		{name: "generation-mismatch", edit: func(a *Assignment) { a.Ticket.Generation = 2 }, replay: true, wantErr: true},
		{name: "endpoint-invalid", edit: func(a *Assignment) { a.Endpoint.Address = "127.0.0.1" }, wantErr: true},
		{name: "expired-without-replay", edit: func(a *Assignment) { a.Ticket.State = "expired"; a.Ticket.Token = ""; a.Endpoint = nil }, wantErr: true},
		{name: "exact-expired-replay", edit: func(a *Assignment) { a.Ticket.State = "expired"; a.Ticket.Token = ""; a.Endpoint = nil }, replay: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				assignment := serviceHistoryTicket(0)
				if tc.edit != nil {
					tc.edit(&assignment)
				}
				serviceHistoryRespond(w, http.StatusOK, AssignmentResult{Assignment: assignment, Replay: tc.replay})
			}))
			defer server.Close()
			client := mustServiceHistoryClient(t, historyClientTestConfig(server.URL))
			got, err := client.Issue(context.Background(), historyClientTestAlloc, historyClientTestUser, "history-join-key-001", 0, false)
			if tc.wantErr {
				serviceHistoryErrorStatus(t, err, http.StatusBadGateway, "")
				if !reflect.DeepEqual(got, AssignmentResult{}) {
					t.Fatal("invalid assignment returned token or endpoint")
				}
			} else if err != nil || got.Assignment.Ticket.State != "expired" || !got.Replay {
				t.Fatal("valid expired exact replay was rejected")
			}
		})
	}
}

func TestServiceHistoryErrorsAreSanitizedAndNeverRetried(t *testing.T) {
	const privateMarker = "private-upstream-message-and-request-id"
	var calls atomic.Int32
	server := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		serviceHistoryRespondRaw(w, http.StatusForbidden, "application/json", []byte(`{"error":{"code":"history_route_forbidden","message":"`+privateMarker+`"},"requestId":"`+privateMarker+`"}`))
	}))
	defer server.Close()
	c := mustServiceHistoryClient(t, historyClientTestConfig(server.URL))
	got, err := c.Status(context.Background(), historyClientTestAlloc, historyClientTestUser)
	serviceHistoryErrorStatus(t, err, http.StatusForbidden, "history_route_forbidden")
	if strings.Contains(err.Error(), privateMarker) || strings.Contains(err.Error(), historyClientTestKey()) ||
		!reflect.DeepEqual(got, ReservationStatus{}) || calls.Load() != 1 {
		t.Fatal("history error leaked wire data, returned partial state, or retried")
	}
}
