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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	serviceSearchTestID     = "history-service-001"
	serviceSearchTestApp    = "history-app-001"
	serviceSearchTestIssuer = "issuer-west-001"
	serviceSearchTestUser   = "platform:user-verified-001"
)

func serviceSearchTestKey() string {
	return HistoryServiceKeyPrefix + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xa6}, 32))
}

func serviceSearchTestConfig(origin string) ServiceSearchConfig {
	return ServiceSearchConfig{
		URL: origin, Key: serviceSearchTestKey(), ServiceID: serviceSearchTestID,
		ApplicationID: serviceSearchTestApp, IdentityIssuer: serviceSearchTestIssuer,
		Region: "us-west", Compatibility: "dm-v1",
	}
}

func mustServiceSearchClient(t *testing.T, cfg ServiceSearchConfig) *ServiceSearchClient {
	t.Helper()
	c, err := NewServiceSearchClient(cfg)
	if err != nil {
		t.Fatal("create service search client")
	}
	t.Cleanup(c.Close)
	return c
}

func serviceSearchTestReservation(state string) Reservation {
	created := time.Date(2026, 9, 30, 17, 10, 0, 0, time.UTC)
	r := Reservation{
		ReservationID: "reservation-service-001", AllocationID: "allocation-service-001", RoomID: "room-service-001",
		ApplicationID: serviceSearchTestApp, PlacementID: "historical-placement-01", RevisionID: "historical-revision-01",
		Region: "us-west", State: state, CreatedAt: created, UpdatedAt: created.Add(time.Minute),
	}
	if state == "technical_aborted" {
		r.FailureCode = "host_process_terminated"
	}
	return r
}

func serviceSearchTestPending(id string) Search {
	created := time.Date(2026, 9, 30, 17, 0, 0, 0, time.UTC)
	return Search{
		ID: id, State: "pending", Region: "us-west", Compatibility: "dm-v1",
		CreatedAt: created, ExpiresAt: created.Add(120 * time.Second),
	}
}

func serviceSearchTestBoundSearch(id string) Search {
	created := time.Date(2026, 9, 30, 17, 0, 0, 0, time.UTC)
	resolved := created.Add(10 * time.Second)
	r := serviceSearchTestReservation("reserved")
	r.CreatedAt = resolved
	r.UpdatedAt = resolved.Add(time.Minute)
	return Search{
		ID: id, State: "bound", Region: "us-west", Compatibility: "dm-v1",
		CreatedAt: created, ExpiresAt: created.Add(120 * time.Second), ResolvedAt: &resolved,
		AllocationID: r.AllocationID, Reservation: &r,
	}
}

func serviceSearchRespond(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "requestId": "service-search-request-001"})
}

func serviceSearchRespondError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":     map[string]string{"code": code, "message": "safe test message"},
		"requestId": "service-search-error-001",
	})
}

func serviceSearchRaw(w http.ResponseWriter, status int, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func serviceSearchAssertError(t *testing.T, err error, status int, code string) {
	t.Helper()
	var got *ServiceSearchError
	if !errors.As(err, &got) || got.Status != status || got.Code != code {
		t.Fatalf("service search error class mismatch: got generic=%v status/code want=%d/%q", err != nil, status, code)
	}
	if err.Error() != "gamefleet service search request failed" {
		t.Fatal("service search error string is not sanitized")
	}
}

func TestServiceSearchClientConfigRequiresLoopbackAndExactGfsvcScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	if c, err := NewServiceSearchClient(serviceSearchTestConfig(server.URL)); err != nil {
		t.Fatal("valid loopback service search config rejected")
	} else {
		c.Close()
	}
	if c, err := NewServiceSearchClient(serviceSearchTestConfig("http://[::1]:18683")); err != nil {
		t.Fatal("canonical IPv6 loopback origin rejected")
	} else {
		c.Close()
	}

	for _, origin := range []string{
		"", "https://127.0.0.1:18683", "http://localhost:18683", "http://192.0.2.10:18683",
		"http://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:99999", "http://127.0.0.1:018683",
		"http://user:pass@127.0.0.1:18683", "http://127.0.0.1:18683/business", "http://127.0.0.1:18683?x=1",
		"http://127.0.0.1:18683?", "http://127.0.0.1:18683/#fragment", "http://[::1%25lo0]:18683",
		"http://127.0.0.1:18683/%2f", "http://127.0.0.01:18683",
	} {
		t.Run("origin", func(t *testing.T) {
			if c, err := NewServiceSearchClient(serviceSearchTestConfig(origin)); err == nil {
				c.Close()
				t.Fatal("noncanonical or nonloopback origin accepted")
			}
		})
	}

	for name, mutate := range map[string]func(*ServiceSearchConfig){
		"caller-key-class":    func(c *ServiceSearchConfig) { c.Key = "gfbiz_" + strings.Repeat("a", 43) },
		"wrong-gfsvc-prefix":  func(c *ServiceSearchConfig) { c.Key = "gfsvc_" + strings.Repeat("a", 42) },
		"noncanonical-gfsvc":  func(c *ServiceSearchConfig) { c.Key = HistoryServiceKeyPrefix + strings.Repeat("a", 42) + "=" },
		"missing-service":     func(c *ServiceSearchConfig) { c.ServiceID = "" },
		"bad-service":         func(c *ServiceSearchConfig) { c.ServiceID = "../service" },
		"missing-application": func(c *ServiceSearchConfig) { c.ApplicationID = "" },
		"bad-application":     func(c *ServiceSearchConfig) { c.ApplicationID = "app/one" },
		"missing-issuer":      func(c *ServiceSearchConfig) { c.IdentityIssuer = "" },
		"bad-issuer":          func(c *ServiceSearchConfig) { c.IdentityIssuer = "issuer with space" },
		"missing-region":      func(c *ServiceSearchConfig) { c.Region = "" },
		"bad-compatibility":   func(c *ServiceSearchConfig) { c.Compatibility = "dm-v1\n" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := serviceSearchTestConfig(server.URL)
			mutate(&cfg)
			if c, err := NewServiceSearchClient(cfg); err == nil {
				c.Close()
				t.Fatal("invalid key class or service scope accepted")
			}
		})
	}
}

func TestServiceSearchCheckScopeUsesExactMetadataAndReadOperation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/business/v1/history/service" || r.URL.RawQuery != "" ||
			r.Header.Get("Authorization") != "Bearer "+serviceSearchTestKey() || r.Header.Get("Cookie") != "" ||
			r.Header.Get("Origin") != "" || r.Header.Get("Content-Type") != "" || r.Header.Get("Accept") != "application/json" {
			t.Error("scope preflight sent unexpected request metadata")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) != 0 {
			t.Error("scope preflight unexpectedly sent a request body")
		}
		serviceSearchRespond(w, http.StatusOK, map[string]any{
			"serviceId": serviceSearchTestID, "applicationId": serviceSearchTestApp,
			"identityIssuer": serviceSearchTestIssuer, "operations": []string{"read", "cancel"},
		})
	}))
	defer server.Close()
	c := mustServiceSearchClient(t, serviceSearchTestConfig(server.URL))
	if err := c.CheckScope(context.Background()); err != nil {
		t.Fatalf("valid exact service metadata rejected: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("scope preflight made unexpected request count")
	}

	for _, tc := range []struct {
		name       string
		service    string
		app        string
		issuer     string
		operations []string
		status     int
	}{
		{name: "other-service", service: "history-service-other", app: serviceSearchTestApp, issuer: serviceSearchTestIssuer, operations: []string{"read"}, status: 502},
		{name: "other-app", service: serviceSearchTestID, app: "history-app-other", issuer: serviceSearchTestIssuer, operations: []string{"read"}, status: 502},
		{name: "other-issuer", service: serviceSearchTestID, app: serviceSearchTestApp, issuer: "issuer-other-001", operations: []string{"read"}, status: 502},
		{name: "missing-read", service: serviceSearchTestID, app: serviceSearchTestApp, issuer: serviceSearchTestIssuer, operations: []string{"cancel"}, status: 403},
		{name: "duplicate-operation", service: serviceSearchTestID, app: serviceSearchTestApp, issuer: serviceSearchTestIssuer, operations: []string{"read", "read"}, status: 502},
		{name: "unknown-operation", service: serviceSearchTestID, app: serviceSearchTestApp, issuer: serviceSearchTestIssuer, operations: []string{"read", "reserve"}, status: 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				serviceSearchRespond(w, http.StatusOK, map[string]any{
					"serviceId": tc.service, "applicationId": tc.app, "identityIssuer": tc.issuer,
					"operations": tc.operations,
				})
			}))
			defer server.Close()
			client := mustServiceSearchClient(t, serviceSearchTestConfig(server.URL))
			serviceSearchAssertError(t, client.CheckScope(context.Background()), tc.status, "")
		})
	}
}

func TestServiceSearchBeginAndStatusUseVersionParticipantRegionAndCompatibility(t *testing.T) {
	const searchID = "search-service-0001"
	var beginBodies [][]byte
	var beginBodiesMu sync.Mutex
	var statusCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+serviceSearchTestKey() ||
			r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" ||
			r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" {
			t.Error("service search request metadata mismatch")
		}
		raw, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/business/v1/service-searches":
			beginBodiesMu.Lock()
			beginBodies = append(beginBodies, append([]byte(nil), raw...))
			beginCount := len(beginBodies)
			beginBodiesMu.Unlock()
			var body struct {
				Version       string `json:"version"`
				ParticipantID string `json:"participantId"`
				RequestID     string `json:"requestId"`
				Region        string `json:"region"`
				Compatibility string `json:"compatibility"`
			}
			if strictObject(raw, &body) != nil || body.Version != ServiceSearchVersion || body.ParticipantID != serviceSearchTestUser ||
				body.RequestID != "stable-search-request-001" || body.Region != "us-west" || body.Compatibility != "dm-v1" {
				t.Error("begin payload did not match the closed service protocol")
			}
			if beginCount == 1 {
				serviceSearchRespond(w, http.StatusCreated, SearchResult{Search: serviceSearchTestPending(searchID), Replay: false})
			} else {
				serviceSearchRespond(w, http.StatusOK, SearchResult{Search: serviceSearchTestPending(searchID), Replay: true})
			}
		case "/business/v1/service-searches/" + searchID + "/status":
			statusCalls.Add(1)
			var body struct {
				Version       string `json:"version"`
				ParticipantID string `json:"participantId"`
			}
			if strictObject(raw, &body) != nil || body.Version != ServiceSearchVersion || body.ParticipantID != serviceSearchTestUser {
				t.Error("status payload did not bind protocol version and participant")
			}
			serviceSearchRespond(w, http.StatusOK, SearchStatus{Search: serviceSearchTestPending(searchID)})
		default:
			t.Error("service search client called an unapproved route")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c := mustServiceSearchClient(t, serviceSearchTestConfig(server.URL))
	first, err := c.BeginSearch(context.Background(), serviceSearchTestUser, "stable-search-request-001")
	if err != nil || first.Replay || first.Search.ID != searchID || first.Search.State != "pending" {
		t.Fatal("initial service search begin response was invalid")
	}
	replay, err := c.BeginSearch(context.Background(), serviceSearchTestUser, "stable-search-request-001")
	if err != nil || !replay.Replay || replay.Search.ID != searchID {
		t.Fatal("exact begin replay response was invalid")
	}
	status, err := c.SearchStatus(context.Background(), searchID, serviceSearchTestUser)
	if err != nil || status.Search.ID != searchID || status.Search.State != "pending" {
		t.Fatal("exact participant-bound search status was invalid")
	}
	beginBodiesMu.Lock()
	beginCount := len(beginBodies)
	beginBodiesEqual := beginCount == 2 && bytes.Equal(beginBodies[0], beginBodies[1])
	beginBodiesMu.Unlock()
	if !beginBodiesEqual || statusCalls.Load() != 1 {
		t.Fatal("begin retry body or status count changed")
	}
}

func TestServiceSearchMatchSortsPrivateCopyAndRetriesFrozenBodyOnlyForCapacityCode(t *testing.T) {
	original := []SearchMatchMember{
		{ParticipantID: "player-z", SearchID: "search-player-0002", NakamaTicket: "nakama-ticket-z"},
		{ParticipantID: "player-a", SearchID: "search-player-0001", NakamaTicket: "nakama-ticket-a"},
	}
	entered := make(chan []byte, 1)
	release := make(chan struct{})
	var received [][]byte
	var receivedMu sync.Mutex
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/business/v1/service-searches/match" || r.URL.RawQuery != "" ||
			r.Header.Get("Authorization") != "Bearer "+serviceSearchTestKey() || r.Header.Get("Content-Type") != "application/json" ||
			r.Header.Get("Accept") != "application/json" || r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" {
			t.Error("match request metadata mismatch")
		}
		raw, _ := io.ReadAll(r.Body)
		receivedMu.Lock()
		received = append(received, append([]byte(nil), raw...))
		receivedMu.Unlock()
		if calls.Add(1) == 1 {
			entered <- append([]byte(nil), raw...)
			<-release
			serviceSearchRespondError(w, http.StatusConflict, "service_search_match_capacity_unavailable")
			return
		}
		serviceSearchRespond(w, http.StatusAccepted, ReservationResult{Reservation: serviceSearchTestReservation("reserved"), Replay: false})
	}))
	defer server.Close()
	c := mustServiceSearchClient(t, serviceSearchTestConfig(server.URL))
	type matchCall struct {
		result ReservationResult
		err    error
	}
	completed := make(chan matchCall, 1)
	go func() {
		result, err := c.MatchSearches(context.Background(), "stable-match-request-001", original)
		completed <- matchCall{result: result, err: err}
	}()
	firstBody := <-entered
	// Mutating caller-owned input after the first request starts must not alter
	// the privately frozen body used by its capacity retry.
	original[0].NakamaTicket = "mutated-after-request"
	close(release)
	got := <-completed
	if got.err != nil || got.result.Replay || got.result.Reservation.AllocationID != "allocation-service-001" {
		t.Fatal("capacity retry did not return the accepted original match")
	}
	receivedMu.Lock()
	receivedCopy := append([][]byte(nil), received...)
	receivedMu.Unlock()
	if len(receivedCopy) != 2 || !bytes.Equal(receivedCopy[0], receivedCopy[1]) || !bytes.Equal(firstBody, receivedCopy[0]) {
		t.Fatal("capacity retry changed the frozen request body")
	}
	var wire struct {
		Version        string              `json:"version"`
		IdempotencyKey string              `json:"idempotencyKey"`
		Region         string              `json:"region"`
		Compatibility  string              `json:"compatibility"`
		Members        []SearchMatchMember `json:"members"`
	}
	if strictObject(receivedCopy[0], &wire) != nil || wire.Version != ServiceSearchVersion ||
		wire.IdempotencyKey != "stable-match-request-001" || wire.Region != "us-west" || wire.Compatibility != "dm-v1" ||
		!reflect.DeepEqual(wire.Members, []SearchMatchMember{
			{ParticipantID: "player-a", SearchID: "search-player-0001", NakamaTicket: "nakama-ticket-a"},
			{ParticipantID: "player-z", SearchID: "search-player-0002", NakamaTicket: "nakama-ticket-z"},
		}) {
		t.Fatal("match body did not contain the exact sorted service request")
	}
	if original[0].ParticipantID != "player-z" || original[1].ParticipantID != "player-a" || original[1].NakamaTicket != "nakama-ticket-a" {
		t.Fatal("client reordered or changed caller-owned input")
	}
}

func TestServiceSearchMatchDoesNotRetryOtherStatusesCodesOrMalformedErrors(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		contentType string
		body        []byte
		code        string
	}{
		{name: "409-conflict", status: 409, body: []byte(`{"error":{"code":"service_search_match_conflict","message":"safe"},"requestId":"err-001"}`), code: "service_search_match_conflict"},
		{name: "409-terminal", status: 409, body: []byte(`{"error":{"code":"service_search_match_terminal","message":"safe"},"requestId":"err-001"}`), code: "service_search_match_terminal"},
		{name: "409-ambiguous-source", status: 409, body: []byte(`{"error":{"code":"service_search_source_ambiguous","message":"safe"},"requestId":"err-001"}`), code: "service_search_source_ambiguous"},
		{name: "409-unknown", status: 409, body: []byte(`{"error":{"code":"unknown_private_code","message":"safe"},"requestId":"err-001"}`), code: "unknown_private_code"},
		{name: "429-capacity-code", status: 429, body: []byte(`{"error":{"code":"service_search_match_capacity_unavailable","message":"safe"},"requestId":"err-001"}`), code: "service_search_match_capacity_unavailable"},
		{name: "503-capacity-code", status: 503, body: []byte(`{"error":{"code":"service_search_match_capacity_unavailable","message":"safe"},"requestId":"err-001"}`), code: "service_search_match_capacity_unavailable"},
		{name: "409-malformed", status: 409, body: []byte(`{"error":{"code":"service_search_match_capacity_unavailable"},"requestId":"err-001"}`), code: ""},
		{name: "409-wrong-content-type", status: 409, contentType: "text/plain", body: []byte(`{"error":{"code":"service_search_match_capacity_unavailable","message":"safe"},"requestId":"err-001"}`), code: ""},
		{name: "409-duplicate-code", status: 409, body: []byte(`{"error":{"code":"service_search_match_capacity_unavailable","CODE":"service_search_match_conflict","message":"safe"},"requestId":"err-001"}`), code: ""},
		{name: "409-unknown-field", status: 409, body: []byte(`{"error":{"code":"service_search_match_capacity_unavailable","message":"safe","details":"private"},"requestId":"err-001"}`), code: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				contentType := tc.contentType
				if contentType == "" {
					contentType = "application/json"
				}
				serviceSearchRaw(w, tc.status, contentType, tc.body)
			}))
			defer server.Close()
			c := mustServiceSearchClient(t, serviceSearchTestConfig(server.URL))
			_, err := c.MatchSearches(context.Background(), "stable-match-request-001", []SearchMatchMember{
				{ParticipantID: "player-a", SearchID: "search-player-0001", NakamaTicket: "ticket-a"},
				{ParticipantID: "player-b", SearchID: "search-player-0002", NakamaTicket: "ticket-b"},
			})
			serviceSearchAssertError(t, err, tc.status, tc.code)
			if calls.Load() != 1 {
				t.Fatal("non-capacity error was retried")
			}
			if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), serviceSearchTestKey()) {
				t.Fatal("HTTP error details leaked in error string")
			}
		})
	}
}

func TestServiceSearchMatchLostResponseRecoversOriginalReceiptWithSameKey(t *testing.T) {
	var calls atomic.Int32
	var bodies [][]byte
	var bodiesMu sync.Mutex
	original := serviceSearchTestReservation("prepared")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodiesMu.Lock()
		bodies = append(bodies, append([]byte(nil), raw...))
		bodiesMu.Unlock()
		if calls.Add(1) == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("loopback HTTP test server does not support disconnect")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Error("could not close first response")
				return
			}
			_ = conn.Close() // The simulated server committed before losing its response.
			return
		}
		serviceSearchRespond(w, http.StatusOK, ReservationResult{Reservation: original, Replay: true})
	}))
	defer server.Close()
	c := mustServiceSearchClient(t, serviceSearchTestConfig(server.URL))
	members := []SearchMatchMember{
		{ParticipantID: "player-a", SearchID: "search-player-0001", NakamaTicket: "ticket-a"},
		{ParticipantID: "player-b", SearchID: "search-player-0002", NakamaTicket: "ticket-b"},
	}
	_, firstErr := c.MatchSearches(context.Background(), "stable-match-request-001", members)
	var first *ServiceSearchError
	if !errors.As(firstErr, &first) || first.Status != 503 {
		t.Fatal("lost response was not returned as a sanitized transport error")
	}
	recovered, err := c.MatchSearches(context.Background(), "stable-match-request-001", members)
	if err != nil || !recovered.Replay || recovered.Reservation.AllocationID != original.AllocationID {
		t.Fatal("same-key retry did not recover the original committed allocation")
	}
	bodiesMu.Lock()
	bodiesCopy := append([][]byte(nil), bodies...)
	bodiesMu.Unlock()
	if calls.Load() != 2 || len(bodiesCopy) != 2 || !bytes.Equal(bodiesCopy[0], bodiesCopy[1]) {
		t.Fatal("lost-response recovery changed the immutable key or match body")
	}
}

func TestServiceSearchStrictSuccessValidationAndNoNetworkForInvalidInput(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		serviceSearchRespond(w, http.StatusOK, SearchStatus{Search: serviceSearchTestPending("search-player-0001")})
	}))
	defer server.Close()
	c := mustServiceSearchClient(t, serviceSearchTestConfig(server.URL))
	for _, tc := range []struct {
		searchID    string
		participant string
	}{
		{"", serviceSearchTestUser},
		{"bad/path", serviceSearchTestUser},
		{"search-player-0001", ""},
		{"search-player-0001", "bad\nparticipant"},
	} {
		_, err := c.SearchStatus(context.Background(), tc.searchID, tc.participant)
		serviceSearchAssertError(t, err, 422, "")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid search identity reached network")
	}

	for _, tc := range []struct {
		name        string
		contentType string
		body        []byte
	}{
		{name: "missing-request-id", body: []byte(`{"data":{"search":{"searchId":"search-player-0001","state":"pending","region":"us-west","compatibility":"dm-v1","createdAt":"2026-09-30T17:00:00Z","expiresAt":"2026-09-30T17:02:00Z"}}}`)},
		{name: "unknown-envelope-field", body: []byte(`{"data":{"search":{}},"requestId":"r","debug":"private"}`)},
		{name: "duplicate-search-id-alias", body: []byte(`{"data":{"search":{"searchId":"search-player-0001","SEARCHID":"search-player-0001","state":"pending","region":"us-west","compatibility":"dm-v1","createdAt":"2026-09-30T17:00:00Z","expiresAt":"2026-09-30T17:02:00Z"}},"requestId":"r"}`)},
		{name: "wrong-id", body: []byte(`{"data":{"search":{"searchId":"search-player-0002","state":"pending","region":"us-west","compatibility":"dm-v1","createdAt":"2026-09-30T17:00:00Z","expiresAt":"2026-09-30T17:02:00Z"}},"requestId":"r"}`)},
		{name: "wrong-profile", body: []byte(`{"data":{"search":{"searchId":"search-player-0001","state":"pending","region":"eu-west","compatibility":"dm-v1","createdAt":"2026-09-30T17:00:00Z","expiresAt":"2026-09-30T17:02:00Z"}},"requestId":"r"}`)},
		{name: "missing-resolved-on-bound", body: []byte(`{"data":{"search":{"searchId":"search-player-0001","state":"bound","region":"us-west","compatibility":"dm-v1","createdAt":"2026-09-30T17:00:00Z","expiresAt":"2026-09-30T17:02:00Z","allocationId":"allocation-service-001","reservation":{}}},"requestId":"r"}`)},
		{name: "wrong-content-type", contentType: "text/plain", body: []byte(`{"data":{"search":{"searchId":"search-player-0001","state":"pending","region":"us-west","compatibility":"dm-v1","createdAt":"2026-09-30T17:00:00Z","expiresAt":"2026-09-30T17:02:00Z"}},"requestId":"r"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				contentType := tc.contentType
				if contentType == "" {
					contentType = "application/json"
				}
				serviceSearchRaw(w, http.StatusOK, contentType, tc.body)
			}))
			defer bad.Close()
			client := mustServiceSearchClient(t, serviceSearchTestConfig(bad.URL))
			got, err := client.SearchStatus(context.Background(), "search-player-0001", serviceSearchTestUser)
			serviceSearchAssertError(t, err, 502, "")
			if !reflect.DeepEqual(got, SearchStatus{}) {
				t.Fatal("invalid status response returned partially decoded search data")
			}
		})
	}

	valid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serviceSearchRespond(w, http.StatusOK, SearchStatus{Search: serviceSearchTestBoundSearch("search-player-0001")})
	}))
	defer valid.Close()
	client := mustServiceSearchClient(t, serviceSearchTestConfig(valid.URL))
	if _, err := client.SearchStatus(context.Background(), "search-player-0001", serviceSearchTestUser); err != nil {
		t.Fatal("valid bound search with exact reservation evidence rejected")
	}
}

func TestServiceSearchSemanticValidationFailuresReturnZeroValues(t *testing.T) {
	t.Run("begin", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			search := serviceSearchTestPending("search-service-valid-001")
			search.Region = "wrong-region"
			serviceSearchRespond(w, http.StatusCreated, SearchResult{Search: search, Replay: false})
		}))
		defer server.Close()
		client := mustServiceSearchClient(t, serviceSearchTestConfig(server.URL))
		got, err := client.BeginSearch(context.Background(), serviceSearchTestUser, "stable-search-request-001")
		serviceSearchAssertError(t, err, 502, "")
		if !reflect.DeepEqual(got, SearchResult{}) {
			t.Fatal("invalid begin response returned search data")
		}
	})

	t.Run("status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			serviceSearchRespond(w, http.StatusOK, SearchStatus{Search: serviceSearchTestPending("search-player-0002")})
		}))
		defer server.Close()
		client := mustServiceSearchClient(t, serviceSearchTestConfig(server.URL))
		got, err := client.SearchStatus(context.Background(), "search-player-0001", serviceSearchTestUser)
		serviceSearchAssertError(t, err, 502, "")
		if !reflect.DeepEqual(got, SearchStatus{}) {
			t.Fatal("invalid status response returned search data")
		}
	})

	t.Run("match", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reservation := serviceSearchTestReservation("reserved")
			reservation.ApplicationID = "wrong-application"
			serviceSearchRespond(w, http.StatusAccepted, ReservationResult{Reservation: reservation, Replay: false})
		}))
		defer server.Close()
		client := mustServiceSearchClient(t, serviceSearchTestConfig(server.URL))
		got, err := client.MatchSearches(context.Background(), "stable-match-request-001", []SearchMatchMember{
			{ParticipantID: "player-a", SearchID: "search-player-0001", NakamaTicket: "ticket-a"},
			{ParticipantID: "player-b", SearchID: "search-player-0002", NakamaTicket: "ticket-b"},
		})
		serviceSearchAssertError(t, err, 502, "")
		if !reflect.DeepEqual(got, ReservationResult{}) {
			t.Fatal("invalid match response returned reservation data")
		}
	})
}

func TestServiceSearchMatchRejectsMalformedReservationAndReplayStatusMismatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   []byte
	}{
		{"status-replay-mismatch", http.StatusAccepted, mustServiceSearchJSON(t, ReservationResult{Reservation: serviceSearchTestReservation("reserved"), Replay: true})},
		{"missing-required-replay", http.StatusAccepted, []byte(`{"data":{"reservation":{"reservationId":"reservation-service-001","allocationId":"allocation-service-001","roomId":"room-service-001","applicationId":"history-app-001","placementId":"historical-placement-01","revisionId":"historical-revision-01","region":"us-west","state":"reserved","cancellationRequested":false,"createdAt":"2026-09-30T17:10:00Z","updatedAt":"2026-09-30T17:11:00Z"}},"requestId":"r"}`)},
		{"missing-required-cancellation-bit", http.StatusAccepted, []byte(`{"data":{"reservation":{"reservationId":"reservation-service-001","allocationId":"allocation-service-001","roomId":"room-service-001","applicationId":"history-app-001","placementId":"historical-placement-01","revisionId":"historical-revision-01","region":"us-west","state":"reserved","createdAt":"2026-09-30T17:10:00Z","updatedAt":"2026-09-30T17:11:00Z"},"replay":false},"requestId":"r"}`)},
		{"completed-with-failure", http.StatusAccepted, mustServiceSearchJSON(t, ReservationResult{Reservation: func() Reservation {
			r := serviceSearchTestReservation("completed")
			r.FailureCode = "host_process_terminated"
			return r
		}(), Replay: false})},
		{"technical-abort-wrong-code", http.StatusOK, mustServiceSearchJSON(t, ReservationResult{Reservation: func() Reservation {
			r := serviceSearchTestReservation("technical_aborted")
			r.FailureCode = "unknown"
			return r
		}(), Replay: true})},
		{"other-application", http.StatusAccepted, mustServiceSearchJSON(t, ReservationResult{Reservation: func() Reservation {
			r := serviceSearchTestReservation("reserved")
			r.ApplicationID = "other-app-001"
			return r
		}(), Replay: false})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				serviceSearchRaw(w, tc.status, "application/json", tc.body)
			}))
			defer server.Close()
			client := mustServiceSearchClient(t, serviceSearchTestConfig(server.URL))
			got, err := client.MatchSearches(context.Background(), "stable-match-request-001", []SearchMatchMember{
				{ParticipantID: "player-a", SearchID: "search-player-0001", NakamaTicket: "ticket-a"},
				{ParticipantID: "player-b", SearchID: "search-player-0002", NakamaTicket: "ticket-b"},
			})
			serviceSearchAssertError(t, err, 502, "")
			if !reflect.DeepEqual(got, ReservationResult{}) {
				t.Fatal("invalid match response returned partially decoded reservation data")
			}
		})
	}

	for _, badMembers := range [][]SearchMatchMember{
		nil,
		{{ParticipantID: "player-a", SearchID: "search-player-0001", NakamaTicket: "ticket-a"}},
		{{ParticipantID: "player-a", SearchID: "search-player-0001", NakamaTicket: "ticket-a"}, {ParticipantID: "player-a", SearchID: "search-player-0002", NakamaTicket: "ticket-b"}},
		{{ParticipantID: "player-a", SearchID: "search-player-0001", NakamaTicket: "ticket-a"}, {ParticipantID: "player-b", SearchID: "search-player-0001", NakamaTicket: "ticket-b"}},
		{{ParticipantID: "player-a", SearchID: "search-player-0001", NakamaTicket: "ticket-a"}, {ParticipantID: "player-b", SearchID: "search-player-0002", NakamaTicket: "ticket-a"}},
	} {
		_, err := mustServiceSearchClient(t, serviceSearchTestConfig("http://127.0.0.1:18683")).MatchSearches(context.Background(), "stable-match-request-001", badMembers)
		serviceSearchAssertError(t, err, 422, "")
	}
}

func mustServiceSearchJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"data": value, "requestId": "test-request-001"})
	if err != nil {
		t.Fatal("marshal synthetic response")
	}
	return data
}

func TestServiceSearchTransportDisablesProxyAndRedirectsAndBoundsBodies(t *testing.T) {
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

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serviceSearchRespond(w, http.StatusOK, SearchStatus{Search: serviceSearchTestPending("search-player-0001")})
	}))
	defer good.Close()
	c := mustServiceSearchClient(t, serviceSearchTestConfig(good.URL))
	if _, err := c.SearchStatus(context.Background(), "search-player-0001", serviceSearchTestUser); err != nil {
		t.Fatal("loopback request under poison proxy environment failed")
	}
	if proxyCalls.Load() != 0 {
		t.Fatal("gfsvc credential was sent through environment proxy")
	}

	var redirectTargetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectTargetCalls.Add(1)
		serviceSearchRespond(w, http.StatusOK, SearchStatus{Search: serviceSearchTestPending("search-player-0001")})
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	redirectClient := mustServiceSearchClient(t, serviceSearchTestConfig(redirect.URL))
	_, err := redirectClient.SearchStatus(context.Background(), "search-player-0001", serviceSearchTestUser)
	serviceSearchAssertError(t, err, http.StatusTemporaryRedirect, "")
	if redirectTargetCalls.Load() != 0 || strings.Contains(err.Error(), target.URL) || strings.Contains(err.Error(), serviceSearchTestKey()) {
		t.Fatal("redirect was followed or endpoint/credential leaked")
	}

	const marker = "private-upstream-body-marker"
	oversized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serviceSearchRaw(w, http.StatusOK, "application/json", []byte(`{"data":{},"requestId":"`+strings.Repeat("x", serviceSearchBodyLimit)+marker+`"}`))
	}))
	defer oversized.Close()
	oversizedClient := mustServiceSearchClient(t, serviceSearchTestConfig(oversized.URL))
	_, err = oversizedClient.SearchStatus(context.Background(), "search-player-0001", serviceSearchTestUser)
	serviceSearchAssertError(t, err, 502, "")
	if strings.Contains(err.Error(), marker) {
		t.Fatal("oversized response body leaked in error string")
	}
}

func TestServiceSearchContextCancellationStopsCapacityRetry(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		serviceSearchRespondError(w, http.StatusConflict, "service_search_match_capacity_unavailable")
		select {
		case entered <- struct{}{}:
		default:
		}
	}))
	defer server.Close()
	c := mustServiceSearchClient(t, serviceSearchTestConfig(server.URL))
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := c.MatchSearches(ctx, "stable-match-request-001", []SearchMatchMember{
			{ParticipantID: "player-a", SearchID: "search-player-0001", NakamaTicket: "ticket-a"},
			{ParticipantID: "player-b", SearchID: "search-player-0002", NakamaTicket: "ticket-b"},
		})
		result <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-result:
		var serviceErr *ServiceSearchError
		if !errors.As(err, &serviceErr) {
			t.Fatal("cancelled capacity retry did not return a sanitized service error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("match retry ignored caller cancellation")
	}
	if calls.Load() != 1 {
		t.Fatal("caller cancellation allowed another capacity retry")
	}
}

func TestServiceSearchErrorEnvelopeMustBeClosedAndSanitized(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		code string
	}{
		{name: "valid", body: `{"error":{"code":"service_search_match_capacity_unavailable","message":"safe"},"requestId":"err-001"}`, code: "service_search_match_capacity_unavailable"},
		{name: "missing-request-id", body: `{"error":{"code":"service_search_match_capacity_unavailable","message":"safe"}}`},
		{name: "unknown-error-field", body: `{"error":{"code":"service_search_match_capacity_unavailable","message":"safe","private":"no"},"requestId":"err-001"}`},
		{name: "duplicate-envelope-field", body: `{"error":{"code":"service_search_match_capacity_unavailable","message":"safe"},"requestId":"err-001","REQUESTID":"other"}`},
		{name: "unicode-key", body: `{"error":{"code":"service_search_match_capacity_unavailable","message":"safe"},"requestId":"err-001","requeſtId":"other"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				serviceSearchRaw(w, http.StatusConflict, "application/json", []byte(tc.body))
			}))
			defer server.Close()
			c := mustServiceSearchClient(t, serviceSearchTestConfig(server.URL))
			_, err := c.MatchSearches(context.Background(), "stable-match-request-001", []SearchMatchMember{
				{ParticipantID: "player-a", SearchID: "search-player-0001", NakamaTicket: "ticket-a"},
				{ParticipantID: "player-b", SearchID: "search-player-0002", NakamaTicket: "ticket-b"},
			})
			serviceSearchAssertError(t, err, http.StatusConflict, tc.code)
			if strings.Contains(err.Error(), "safe") || strings.Contains(err.Error(), serviceSearchTestKey()) || strings.Contains(err.Error(), "err-001") ||
				(tc.code != "" && strings.Contains(err.Error(), tc.code)) {
				t.Fatal("error response content leaked through error string")
			}
		})
	}
}

func TestServiceSearchClientDoesNotImplementCallerBackend(t *testing.T) {
	var _ interface {
		CheckScope(context.Context) error
		BeginSearch(context.Context, string, string) (SearchResult, error)
		SearchStatus(context.Context, string, string) (SearchStatus, error)
		MatchSearches(context.Context, string, []SearchMatchMember) (ReservationResult, error)
		Close()
	} = (*ServiceSearchClient)(nil)
	if _, ok := any((*ServiceSearchClient)(nil)).(Backend); ok {
		t.Fatal("service search client accidentally gained ordinary caller backend authority")
	}
}

func TestServiceSearchCapacityRetryDeadlineIsBounded(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		serviceSearchRespondError(w, http.StatusConflict, "service_search_match_capacity_unavailable")
	}))
	defer server.Close()
	c := mustServiceSearchClient(t, serviceSearchTestConfig(server.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.MatchSearches(ctx, "stable-match-request-001", []SearchMatchMember{
		{ParticipantID: "player-a", SearchID: "search-player-0001", NakamaTicket: "ticket-a"},
		{ParticipantID: "player-b", SearchID: "search-player-0002", NakamaTicket: "ticket-b"},
	})
	if err == nil || time.Since(start) > time.Second || calls.Load() > 2 {
		t.Fatal("capacity retry exceeded its caller deadline")
	}
}

func TestServiceSearchTechnicalAbortReservationValidationIsExact(t *testing.T) {
	for _, tc := range []struct {
		state   string
		failure string
		valid   bool
	}{
		{"reserved", "", true},
		{"prepared", "", true},
		{"completed", "", true},
		{"completed", "host_process_terminated", false},
		{"technical_aborted", "host_process_terminated", true},
		{"technical_aborted", "unknown", false},
		{"technical_aborted", "", false},
		{"updating", "", false},
	} {
		t.Run(tc.state+"/"+tc.failure, func(t *testing.T) {
			r := serviceSearchTestReservation(tc.state)
			r.FailureCode = tc.failure
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				status := http.StatusAccepted
				serviceSearchRespond(w, status, ReservationResult{Reservation: r, Replay: false})
			}))
			defer server.Close()
			c := mustServiceSearchClient(t, serviceSearchTestConfig(server.URL))
			_, err := c.MatchSearches(context.Background(), "stable-match-request-001", []SearchMatchMember{
				{ParticipantID: "player-a", SearchID: "search-player-0001", NakamaTicket: "ticket-a"},
				{ParticipantID: "player-b", SearchID: "search-player-0002", NakamaTicket: "ticket-b"},
			})
			if tc.valid && err != nil {
				t.Fatal("valid reservation lifecycle state rejected")
			}
			if !tc.valid {
				serviceSearchAssertError(t, err, 502, "")
			}
		})
	}
}

func TestServiceSearchNoErrorReportsWireBodies(t *testing.T) {
	const privateMarker = "never-return-this-response-content"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serviceSearchRaw(w, http.StatusConflict, "application/json", []byte(fmt.Sprintf(
			`{"error":{"code":"service_search_match_capacity_unavailable","message":"%s"},"requestId":"err-001"}`,
			privateMarker)))
	}))
	defer server.Close()
	c := mustServiceSearchClient(t, serviceSearchTestConfig(server.URL))
	_, err := c.MatchSearches(context.Background(), "stable-match-request-001", []SearchMatchMember{
		{ParticipantID: "player-a", SearchID: "search-player-0001", NakamaTicket: "ticket-a"},
		{ParticipantID: "player-b", SearchID: "search-player-0002", NakamaTicket: "ticket-b"},
	})
	if err == nil || strings.Contains(err.Error(), privateMarker) || strings.Contains(err.Error(), serviceSearchTestKey()) {
		t.Fatal("service search error included upstream body or credential")
	}
}
