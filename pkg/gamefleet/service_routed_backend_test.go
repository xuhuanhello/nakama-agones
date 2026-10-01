package gamefleet

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type routedBackendObservedRequest struct {
	method string
	path   string
	query  string
	auth   string
	body   []byte
}

type routedBackendRequestLog struct {
	mu       sync.Mutex
	requests []routedBackendObservedRequest
}

func (l *routedBackendRequestLog) add(r routedBackendObservedRequest) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r.body = append([]byte(nil), r.body...)
	l.requests = append(l.requests, r)
}

func (l *routedBackendRequestLog) snapshot() []routedBackendObservedRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := append([]routedBackendObservedRequest(nil), l.requests...)
	for i := range out {
		out[i].body = append([]byte(nil), out[i].body...)
	}
	return out
}

func newRoutedBackendClients(t *testing.T, cfg ServiceSearchConfig) (*ServiceSearchClient, *ServiceHistoryClient, *RoutedServiceSearchClient) {
	t.Helper()
	search, err := NewServiceSearchClient(cfg)
	if err != nil {
		t.Fatal("create v1 search client")
	}
	history, err := NewServiceHistoryClient(ServiceHistoryConfig{
		URL: cfg.URL, Key: cfg.Key, ServiceID: cfg.ServiceID, ApplicationID: cfg.ApplicationID,
		IdentityIssuer: cfg.IdentityIssuer, Region: cfg.Region, Compatibility: cfg.Compatibility,
	})
	if err != nil {
		search.Close()
		t.Fatal("create history client")
	}
	routed, err := NewRoutedServiceSearchClient(cfg)
	if err != nil {
		search.Close()
		history.Close()
		t.Fatal("create v2 routed client")
	}
	t.Cleanup(search.Close)
	t.Cleanup(history.Close)
	t.Cleanup(routed.Close)
	return search, history, routed
}

func TestServiceBackendRequiresRoutedClientWithExactServiceIdentity(t *testing.T) {
	first := httptest.NewServer(http.NotFoundHandler())
	defer first.Close()
	second := httptest.NewServer(http.NotFoundHandler())
	defer second.Close()

	base := serviceSearchTestConfig(first.URL)
	search, history, routed := newRoutedBackendClients(t, base)
	if _, err := newServiceBackend(search, history, routed); err != nil {
		t.Fatalf("matching routed client rejected: %v", err)
	}
	if _, err := newServiceBackend(search, history, nil); err == nil {
		t.Fatal("explicit nil routed client accepted")
	}
	if _, err := newServiceBackend(search, history, routed, routed); err == nil {
		t.Fatal("multiple routed clients accepted")
	}

	variants := map[string]func(*ServiceSearchConfig){
		"url": func(c *ServiceSearchConfig) { c.URL = second.URL },
		"key": func(c *ServiceSearchConfig) {
			c.Key = HistoryServiceKeyPrefix + base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("r", 32)))
		},
		"service":       func(c *ServiceSearchConfig) { c.ServiceID = "another-service-001" },
		"application":   func(c *ServiceSearchConfig) { c.ApplicationID = "another-app-0001" },
		"issuer":        func(c *ServiceSearchConfig) { c.IdentityIssuer = "issuer-east-001" },
		"region":        func(c *ServiceSearchConfig) { c.Region = "eu-west" },
		"compatibility": func(c *ServiceSearchConfig) { c.Compatibility = "dm-v2" },
	}
	for name, mutate := range variants {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			candidate, err := NewRoutedServiceSearchClient(cfg)
			if err != nil {
				t.Fatal("create valid mismatched routed client")
			}
			defer candidate.Close()
			if _, err := newServiceBackend(search, history, candidate); err == nil {
				t.Fatal("routed client with a different exact identity/profile was accepted")
			}
		})
	}
}

func routedBackendSearch(id string, cfg ServiceSearchConfig, state string) Search {
	created := time.Date(2026, 9, 30, 18, 0, 0, 0, time.UTC)
	s := Search{ID: id, State: state, Region: cfg.Region, Compatibility: cfg.Compatibility,
		CreatedAt: created, ExpiresAt: created.Add(300 * time.Second)}
	if state == "cancelled" {
		resolved := created.Add(time.Minute)
		s.ResolvedAt = &resolved
	}
	return s
}

func TestRoutedBackendBeginsOnV2WhileHistoryAndMatchStayOnExistingRoutes(t *testing.T) {
	cfg := serviceSearchTestConfig("")
	log := &routedBackendRequestLog{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		log.add(routedBackendObservedRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			auth: r.Header.Get("Authorization"), body: raw})
		switch r.URL.Path {
		case "/business/v1/history/service":
			serviceHistoryRespond(w, http.StatusOK, map[string]any{
				"serviceId": cfg.ServiceID, "applicationId": cfg.ApplicationID, "identityIssuer": cfg.IdentityIssuer,
				"operations": []string{"read", "cancel", "assignment", "resume"},
			})
		case "/business/v2/service-searches":
			serviceSearchRespond(w, http.StatusCreated, RoutedSearchResult{
				Version: ServiceSearchRoutedVersion, Search: routedBackendSearch("search_routed_001", cfg, "pending"),
				MatchPoolID: routedServiceSearchTestPool(), Replay: false,
			})
		case "/business/v1/history/reservations/current":
			serviceHistoryRespond(w, http.StatusOK, CurrentResult{Current: nil})
		case "/business/v1/history/searches/search_history_001/status":
			serviceHistoryRespond(w, http.StatusOK, SearchStatus{Search: routedBackendSearch("search_history_001", cfg, "pending")})
		case "/business/v1/history/searches/search_history_001/cancel":
			serviceHistoryRespond(w, http.StatusOK, SearchResult{Search: routedBackendSearch("search_history_001", cfg, "cancelled"), Replay: false})
		case "/business/v1/service-searches/match":
			reservation := serviceSearchTestReservation("reserved")
			reservation.Region = cfg.Region
			serviceSearchRespond(w, http.StatusAccepted, ReservationResult{Reservation: reservation, Replay: false})
		default:
			serviceBackendTestError(w, http.StatusNotFound)
		}
	}))
	defer server.Close()
	cfg.URL = server.URL
	search, history, routed := newRoutedBackendClients(t, cfg)
	backend, err := newServiceBackend(search, history, routed)
	if err != nil {
		t.Fatal("create routed service backend")
	}
	initializer := &bridgeInitializer{}
	if err := registerServiceBridge(initializer, backend, search, routed); err != nil {
		t.Fatal("register routed service bridge")
	}

	ctx := bridgeCtx("player-one")
	beginPayload, err := json.Marshal(searchBeginRequest{
		profileRequest: profileRequest{Version: SearchVersion, Region: cfg.Region, Compatibility: cfg.Compatibility},
		RequestID:      "begin_request_001",
	})
	if err != nil {
		t.Fatal("marshal player begin request")
	}
	beginRaw, err := initializer.rpcs[SearchBeginRPC](ctx, nil, nil, nil, string(beginPayload))
	if err != nil {
		t.Fatalf("v2-backed player Begin RPC failed: %v", err)
	}
	var playerBegin map[string]json.RawMessage
	if json.Unmarshal([]byte(beginRaw), &playerBegin) != nil || len(playerBegin) != 2 || playerBegin["search"] == nil || playerBegin["replay"] == nil {
		t.Fatalf("player Begin response did not retain the legacy two-field shape: %s", beginRaw)
	}
	if playerBegin["version"] != nil || playerBegin["matchPoolId"] != nil || strings.Contains(beginRaw, "gfsp_") {
		t.Fatalf("private v2 routing data escaped through player Begin RPC: %s", beginRaw)
	}

	profile := func(version string) string {
		raw, marshalErr := json.Marshal(profileRequest{Version: version, Region: cfg.Region, Compatibility: cfg.Compatibility})
		if marshalErr != nil {
			t.Fatal("marshal player profile")
		}
		return string(raw)
	}
	if raw, callErr := initializer.rpcs[CurrentRPC](ctx, nil, nil, nil, profile(RoomVersion)); callErr != nil || !strings.Contains(raw, `"current":null`) {
		t.Fatalf("Current did not continue through History: %s %v", raw, callErr)
	}
	participantPayload := func(id string) string {
		raw, marshalErr := json.Marshal(searchParticipantRequest{
			profileRequest: profileRequest{Version: SearchVersion, Region: cfg.Region, Compatibility: cfg.Compatibility},
			SearchID:       id,
		})
		if marshalErr != nil {
			t.Fatal("marshal history search request")
		}
		return string(raw)
	}
	if _, err := initializer.rpcs[SearchStatusRPC](ctx, nil, nil, nil, participantPayload("search_history_001")); err != nil {
		t.Fatalf("SearchStatus did not continue through History: %v", err)
	}
	if _, err := initializer.rpcs[SearchCancelRPC](ctx, nil, nil, nil, participantPayload("search_history_001")); err != nil {
		t.Fatalf("SearchCancel did not continue through History: %v", err)
	}
	if _, err := backend.MatchSearches(ctx, "match_key_0001", []SearchMatchMember{
		{ParticipantID: "player-b", SearchID: "search_player_b", NakamaTicket: "ticket-b"},
		{ParticipantID: "player-a", SearchID: "search_player_a", NakamaTicket: "ticket-a"},
	}); err != nil {
		t.Fatalf("MatchSearches did not continue through the existing v1 route: %v", err)
	}

	wantPaths := []string{
		"/business/v2/service-searches", "/business/v1/history/reservations/current",
		"/business/v1/history/searches/search_history_001/status", "/business/v1/history/searches/search_history_001/cancel",
		"/business/v1/service-searches/match",
	}
	requests := log.snapshot()
	if len(requests) != len(wantPaths) {
		gotPaths := make([]string, len(requests))
		for i := range requests {
			gotPaths[i] = requests[i].path
		}
		t.Fatalf("unexpected routed backend request count: got %d want %d; paths=%v", len(requests), len(wantPaths), gotPaths)
	}
	for i, req := range requests {
		if req.path != wantPaths[i] || req.query != "" || req.auth != "Bearer "+cfg.Key {
			t.Fatalf("request %d crossed protocol/auth boundary: method=%s path=%s", i, req.method, req.path)
		}
	}
	var v2Begin map[string]json.RawMessage
	if json.Unmarshal(requests[0].body, &v2Begin) != nil {
		t.Fatal("v2 Begin request was not JSON")
	}
	var wireVersion string
	if json.Unmarshal(v2Begin["version"], &wireVersion) != nil || wireVersion != ServiceSearchRoutedVersion ||
		v2Begin["matchPoolId"] != nil || v2Begin["callerId"] != nil {
		keys := make([]string, 0, len(v2Begin))
		for key := range v2Begin {
			keys = append(keys, key)
		}
		t.Fatalf("v2 Begin request did not use the private versioned player request shape: version=%q keys=%v", wireVersion, keys)
	}
	var matchBody map[string]json.RawMessage
	if json.Unmarshal(requests[len(requests)-1].body, &matchBody) != nil ||
		matchBody["version"] == nil || matchBody["matchPoolId"] != nil {
		t.Fatal("match request did not stay on the established v1 wire shape")
	}
	var matchVersion string
	if json.Unmarshal(matchBody["version"], &matchVersion) != nil || matchVersion != ServiceSearchVersion {
		t.Fatal("match request did not use the existing v1 protocol")
	}
}

func TestRoutedBridgeNeverFallsBackAfterV2BeginOrStatusFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusCode int
		malformed  bool
		wantStatus int
	}{
		{name: "forbidden", statusCode: http.StatusForbidden, wantStatus: http.StatusForbidden},
		{name: "missing", statusCode: http.StatusNotFound, wantStatus: http.StatusNotFound},
		{name: "unavailable", statusCode: http.StatusServiceUnavailable, wantStatus: http.StatusServiceUnavailable},
		{name: "malformed response", statusCode: http.StatusOK, malformed: true, wantStatus: http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				if r.URL.Path == "/business/v1/history/service" {
					serviceHistoryRespond(w, http.StatusOK, map[string]any{
						"serviceId": serviceSearchTestID, "applicationId": serviceSearchTestApp,
						"identityIssuer": serviceSearchTestIssuer, "operations": []string{"read", "cancel", "assignment", "resume"},
					})
					return
				}
				if tc.malformed {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.statusCode)
					_, _ = io.WriteString(w, `{"data":{"version":`)
					return
				}
				serviceBackendTestError(w, tc.statusCode)
			}))
			defer server.Close()
			cfg := serviceSearchTestConfig(server.URL)
			search, history, routed := newRoutedBackendClients(t, cfg)
			backend, err := newServiceBackend(search, history, routed)
			if err != nil {
				t.Fatal("create routed backend")
			}
			if _, err := backend.BeginSearch(context.Background(), "player-one", "request_begin_001"); err == nil {
				t.Fatal("failed v2 Begin was accepted")
			} else {
				serviceBackendTestAssertError(t, err, tc.wantStatus)
			}

			initializer := &bridgeInitializer{}
			if err := registerServiceBridge(initializer, backend, search, routed); err != nil {
				t.Fatal("register routed bridge")
			}
			envelope := matchmakerEnvelope(RoomVersion, cfg.Compatibility, cfg.Region, nil)
			envelope.GetMatchmakerAdd().StringProperties["gamefleet_search_id"] = "search_player_a"
			if _, err := initializer.before(bridgeCtx("player-one"), nil, nil, nil, envelope); err == nil {
				t.Fatal("failed v2 status was accepted")
			}

			mu.Lock()
			gotPaths := append([]string(nil), paths...)
			mu.Unlock()
			if len(gotPaths) != 2 || gotPaths[0] != "/business/v2/service-searches" ||
				gotPaths[1] != "/business/v2/service-searches/search_player_a/status" {
				t.Fatalf("failure path attempted another protocol or skipped v2 calls: %v", gotPaths)
			}
		})
	}
}
