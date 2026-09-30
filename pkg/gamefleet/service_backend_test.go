package gamefleet

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/heroiclabs/nakama-common/runtime"
)

const (
	serviceBackendTestUser = "platform:service-backend-player-001"
	serviceBackendTestOld  = "search_old_0001"
	serviceBackendTestNew  = "search_new_0001"
)

type serviceBackendRequest struct {
	method string
	path   string
	query  string
	auth   string
	body   []byte
}

type serviceBackendRecorder struct {
	mu       sync.Mutex
	requests []serviceBackendRequest
}

func (r *serviceBackendRecorder) add(req serviceBackendRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	req.body = append([]byte(nil), req.body...)
	r.requests = append(r.requests, req)
}

func (r *serviceBackendRecorder) snapshot() []serviceBackendRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]serviceBackendRequest, len(r.requests))
	copy(out, r.requests)
	for i := range out {
		out[i].body = append([]byte(nil), out[i].body...)
	}
	return out
}

func serviceBackendClients(t *testing.T, searchURL, historyURL string) (*ServiceSearchClient, *ServiceHistoryClient) {
	t.Helper()
	searchConfig := serviceSearchTestConfig(searchURL)
	historyConfig := ServiceHistoryConfig{
		URL: historyURL, Key: searchConfig.Key, ServiceID: searchConfig.ServiceID,
		ApplicationID: searchConfig.ApplicationID, IdentityIssuer: searchConfig.IdentityIssuer,
		Region: searchConfig.Region, Compatibility: searchConfig.Compatibility,
	}
	search, err := NewServiceSearchClient(searchConfig)
	if err != nil {
		t.Fatal("create scoped service search client")
	}
	history, err := NewServiceHistoryClient(historyConfig)
	if err != nil {
		search.Close()
		t.Fatal("create scoped service history client")
	}
	t.Cleanup(search.Close)
	t.Cleanup(history.Close)
	return search, history
}

func serviceBackendTestError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":{"code":"private-wire-code","message":"private wire detail"},"requestId":"private-request-id"}`)
}

func serviceBackendTestAssertError(t *testing.T, err error, status int) {
	t.Helper()
	var got *Error
	if !errors.As(err, &got) || got.Status != status {
		t.Fatalf("service backend error status mismatch: got=%v want=%d", err, status)
	}
	if err.Error() != "gamefleet request failed" || strings.Contains(err.Error(), "private") {
		t.Fatalf("service backend exposed upstream error: %q", err.Error())
	}
}

func serviceBackendTestDecodeBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode captured request: %v", err)
	}
	return body
}

func serviceBackendRoomPayload(allocationID string) string {
	cfg := serviceSearchTestConfig("http://127.0.0.1:12345")
	raw, _ := json.Marshal(statusRequest{
		profileRequest: profileRequest{Version: RoomVersion, Compatibility: cfg.Compatibility, Region: cfg.Region},
		AllocationID:   allocationID,
	})
	return string(raw)
}

func serviceBackendCurrentPayload() string {
	cfg := serviceSearchTestConfig("http://127.0.0.1:12345")
	raw, _ := json.Marshal(profileRequest{Version: RoomVersion, Compatibility: cfg.Compatibility, Region: cfg.Region})
	return string(raw)
}

func serviceBackendSearchStatusPayload(searchID string) string {
	cfg := serviceSearchTestConfig("http://127.0.0.1:12345")
	raw, _ := json.Marshal(searchParticipantRequest{
		profileRequest: profileRequest{Version: SearchVersion, Compatibility: cfg.Compatibility, Region: cfg.Region},
		SearchID:       searchID,
	})
	return string(raw)
}

func TestServiceBackendRequiresOneExactServiceIdentityAndProfile(t *testing.T) {
	first := httptest.NewServer(http.NotFoundHandler())
	defer first.Close()
	second := httptest.NewServer(http.NotFoundHandler())
	defer second.Close()

	search, history := serviceBackendClients(t, first.URL, first.URL)
	if _, err := newServiceBackend(search, history); err != nil {
		t.Fatalf("same exact service scope rejected: %v", err)
	}
	if _, err := newServiceBackend(nil, history); err == nil {
		t.Fatal("nil search client accepted")
	}
	if _, err := newServiceBackend(search, nil); err == nil {
		t.Fatal("nil history client accepted")
	}

	base := serviceSearchTestConfig(first.URL)
	variants := map[string]func(*ServiceHistoryConfig){
		"url": func(c *ServiceHistoryConfig) { c.URL = second.URL },
		"key": func(c *ServiceHistoryConfig) {
			c.Key = HistoryServiceKeyPrefix + base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("q", 32)))
		},
		"service":       func(c *ServiceHistoryConfig) { c.ServiceID = "different-service-001" },
		"application":   func(c *ServiceHistoryConfig) { c.ApplicationID = "different-app-0001" },
		"issuer":        func(c *ServiceHistoryConfig) { c.IdentityIssuer = "issuer-east-001" },
		"region":        func(c *ServiceHistoryConfig) { c.Region = "eu-west" },
		"compatibility": func(c *ServiceHistoryConfig) { c.Compatibility = "dm-v2" },
	}
	for name, change := range variants {
		t.Run(name, func(t *testing.T) {
			cfg := ServiceHistoryConfig{
				URL: base.URL, Key: base.Key, ServiceID: base.ServiceID, ApplicationID: base.ApplicationID,
				IdentityIssuer: base.IdentityIssuer, Region: base.Region, Compatibility: base.Compatibility,
			}
			change(&cfg)
			other, err := NewServiceHistoryClient(cfg)
			if err != nil {
				t.Fatal("create valid but mismatched history client")
			}
			defer other.Close()
			if _, err := newServiceBackend(search, other); err == nil {
				t.Fatal("different service identity/profile combined")
			}
		})
	}
}

func TestServiceBackendKeepsSearchAndHistoryOperationsOnTheirOwnRoutes(t *testing.T) {
	recorder := &serviceBackendRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		recorder.add(serviceBackendRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			auth: r.Header.Get("Authorization"), body: raw})
		serviceBackendTestError(w, http.StatusInternalServerError)
	}))
	defer server.Close()
	search, history := serviceBackendClients(t, server.URL, server.URL)
	backend, err := newServiceBackend(search, history)
	if err != nil {
		t.Fatal("create service backend")
	}

	ctx := bridgeCtx(serviceBackendTestUser)
	members := []SearchMatchMember{
		{ParticipantID: "platform:player-b", SearchID: "search_player_b", NakamaTicket: "nakama-ticket-b"},
		{ParticipantID: "platform:player-a", SearchID: "search_player_a", NakamaTicket: "nakama-ticket-a"},
	}
	originalMembers := append([]SearchMatchMember(nil), members...)
	for _, invoke := range []func() error{
		func() error { _, e := backend.BeginSearch(ctx, serviceBackendTestUser, "request-begin-001"); return e },
		func() error { _, e := backend.BeginSearch(ctx, serviceBackendTestUser, "request-begin-001"); return e },
		func() error { _, e := backend.MatchSearches(ctx, "match-key-0001", members); return e },
		func() error { _, e := backend.MatchSearches(ctx, "match-key-0001", members); return e },
		func() error { _, e := backend.Current(ctx, serviceBackendTestUser); return e },
		func() error { _, e := backend.Status(ctx, "allocation-service-001", serviceBackendTestUser); return e },
		func() error {
			_, e := backend.SearchStatus(ctx, serviceBackendTestOld, serviceBackendTestUser)
			return e
		},
		func() error {
			_, e := backend.CancelSearch(ctx, serviceBackendTestOld, serviceBackendTestUser)
			return e
		},
		func() error {
			_, e := backend.Issue(ctx, "allocation-service-001", serviceBackendTestUser, "ticket-request-01", 0, false)
			return e
		},
		func() error { _, e := backend.Cancel(ctx, "allocation-service-001"); return e },
	} {
		serviceBackendTestAssertError(t, invoke(), http.StatusInternalServerError)
	}
	if !reflect.DeepEqual(members, originalMembers) {
		t.Fatal("service MatchSearches mutated the caller's frozen member pair")
	}

	reqs := recorder.snapshot()
	wantPaths := []string{
		"/business/v1/service-searches", "/business/v1/service-searches",
		"/business/v1/service-searches/match", "/business/v1/service-searches/match",
		"/business/v1/history/reservations/current", "/business/v1/history/reservations/allocation-service-001/status",
		"/business/v1/history/searches/search_old_0001/status", "/business/v1/history/searches/search_old_0001/cancel",
		"/business/v1/history/reservations/allocation-service-001/assignment", "/business/v1/history/reservations/allocation-service-001/cancel",
	}
	if len(reqs) != len(wantPaths) {
		t.Fatalf("request count %d, want %d", len(reqs), len(wantPaths))
	}
	for i, req := range reqs {
		if req.method != http.MethodPost || req.path != wantPaths[i] || req.query != "" ||
			req.auth != "Bearer "+serviceSearchTestKey() {
			t.Fatalf("request %d crossed route or auth boundary: method=%s path=%s query=%q", i, req.method, req.path, req.query)
		}
		if strings.Contains(req.path, "/business/v1/searches/") || strings.Contains(req.path, "/business/v1/reservations") {
			t.Fatalf("service backend called an ordinary-caller route: %s", req.path)
		}
	}
	if !reflect.DeepEqual(reqs[0].body, reqs[1].body) {
		t.Fatal("repeating BeginSearch changed the original request or source")
	}
	if !reflect.DeepEqual(reqs[2].body, reqs[3].body) {
		t.Fatal("repeating MatchSearches changed the frozen request pair")
	}
	begin := serviceBackendTestDecodeBody(t, reqs[0].body)
	if begin["version"] != ServiceSearchVersion || begin["participantId"] != serviceBackendTestUser || begin["requestId"] != "request-begin-001" {
		t.Fatalf("BeginSearch did not bind the supplied participant and request: %v", begin)
	}
	match := serviceBackendTestDecodeBody(t, reqs[2].body)
	if match["version"] != ServiceSearchVersion || match["idempotencyKey"] != "match-key-0001" || match["members"] == nil {
		t.Fatalf("MatchSearches request omitted its fixed pair: %v", match)
	}
	for _, i := range []int{4, 5, 6, 7, 8, 9} {
		body := serviceBackendTestDecodeBody(t, reqs[i].body)
		if body["participantId"] != serviceBackendTestUser {
			t.Fatalf("history operation %s did not carry its caller participant: %v", reqs[i].path, body)
		}
	}
}

func TestServiceBackendCancelUsesRuntimeIdentityAndRejectsForgedPayload(t *testing.T) {
	recorder := &serviceBackendRecorder{}
	reservation := serviceSearchTestReservation("reserved")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		recorder.add(serviceBackendRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			auth: r.Header.Get("Authorization"), body: raw})
		switch r.URL.Path {
		case "/business/v1/history/reservations/current":
			serviceHistoryRespond(w, http.StatusOK, map[string]any{"current": map[string]any{"reservation": reservation, "connectionGeneration": 2}})
		case "/business/v1/history/reservations/allocation-service-001/cancel":
			serviceHistoryRespond(w, http.StatusOK, ReservationResult{Reservation: reservation})
		default:
			serviceBackendTestError(w, http.StatusNotFound)
		}
	}))
	defer server.Close()
	search, history := serviceBackendClients(t, server.URL, server.URL)
	backend, err := newServiceBackend(search, history)
	if err != nil {
		t.Fatal("create service backend")
	}
	if _, err := backend.Cancel(context.Background(), reservation.AllocationID); err == nil {
		t.Fatal("anonymous direct cancel unexpectedly succeeded")
	} else {
		serviceBackendTestAssertError(t, err, http.StatusForbidden)
	}
	if len(recorder.snapshot()) != 0 {
		t.Fatal("anonymous cancel reached the platform")
	}

	initializer := &bridgeInitializer{}
	if err := registerServiceBridge(initializer, backend, search); err != nil {
		t.Fatal("register service bridge")
	}
	valid := serviceBackendRoomPayload(reservation.AllocationID)
	forged := strings.TrimSuffix(valid, "}") + `,"participantId":"victim-player"}`
	_, err = initializer.rpcs[CancelRPC](bridgeCtx(serviceBackendTestUser), nil, nil, nil, forged)
	assertBridgeError(t, err, 3, "invalid payload")
	if len(recorder.snapshot()) != 0 {
		t.Fatal("forged participant payload reached the platform")
	}

	raw, err := initializer.rpcs[CancelRPC](bridgeCtx(serviceBackendTestUser), nil, nil, nil, valid)
	if err != nil || !strings.Contains(raw, reservation.AllocationID) {
		t.Fatalf("authenticated cancel failed: response=%q err=%v", raw, err)
	}
	reqs := recorder.snapshot()
	if len(reqs) != 2 || reqs[0].path != "/business/v1/history/reservations/current" ||
		reqs[1].path != "/business/v1/history/reservations/allocation-service-001/cancel" {
		t.Fatalf("authenticated cancel used unexpected history routes: %+v", reqs)
	}
	for _, req := range reqs {
		body := serviceBackendTestDecodeBody(t, req.body)
		if body["participantId"] != serviceBackendTestUser {
			t.Fatalf("cancel trusted request payload instead of runtime identity: %v", body)
		}
	}
}

func TestServiceBridgeUsesMappedSearchForAdmissionAndHistoryForRecovery(t *testing.T) {
	var mu sync.Mutex
	var seen []serviceBackendRequest
	oldSearch := serviceSearchTestPending(serviceBackendTestOld)
	newSearch := serviceSearchTestPending(serviceBackendTestNew)
	reservation := serviceSearchTestReservation("completed")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, serviceBackendRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			auth: r.Header.Get("Authorization"), body: append([]byte(nil), raw...)})
		mu.Unlock()
		switch r.URL.Path {
		case "/business/v1/service-searches/" + serviceBackendTestOld + "/status":
			serviceSearchRespondError(w, http.StatusNotFound, "mapped_search_not_found")
		case "/business/v1/service-searches/" + serviceBackendTestNew + "/status":
			serviceSearchRespond(w, http.StatusOK, SearchStatus{Search: newSearch})
		case "/business/v1/history/searches/" + serviceBackendTestOld + "/status":
			serviceHistoryRespond(w, http.StatusOK, SearchStatus{Search: oldSearch})
		case "/business/v1/history/reservations/current":
			serviceHistoryRespond(w, http.StatusOK, CurrentResult{Current: nil})
		case "/business/v1/history/reservations/allocation-service-001/status":
			serviceHistoryRespond(w, http.StatusOK, ReservationStatus{Reservation: reservation})
		case "/business/v1/service-searches/match":
			serviceSearchRespondError(w, http.StatusInternalServerError, "temporary_private_failure")
		default:
			serviceBackendTestError(w, http.StatusNotFound)
		}
	}))
	defer server.Close()
	search, history := serviceBackendClients(t, server.URL, server.URL)
	backend, err := newServiceBackend(search, history)
	if err != nil {
		t.Fatal("create service backend")
	}
	initializer := &bridgeInitializer{}
	if err := registerServiceBridge(initializer, backend, search); err != nil {
		t.Fatal("register service bridge")
	}
	ctx := bridgeCtx(serviceBackendTestUser)
	searchConfig := serviceSearchTestConfig(server.URL)
	cfg := Config{Region: searchConfig.Region, Compatibility: searchConfig.Compatibility}
	oldAdmission := matchmakerEnvelope(RoomVersion, searchConfig.Compatibility, searchConfig.Region, nil)
	oldAdmission.GetMatchmakerAdd().StringProperties["gamefleet_search_id"] = serviceBackendTestOld
	_, err = initializer.before(ctx, nil, nil, nil, oldAdmission)
	assertBridgeError(t, err, 5, "reservation unavailable")
	mu.Lock()
	if len(seen) != 1 || seen[0].path != "/business/v1/service-searches/"+serviceBackendTestOld+"/status" {
		mu.Unlock()
		t.Fatalf("MatchmakerAdd did not check the exact mapped-search route: %+v", seen)
	}
	mu.Unlock()

	newAdmission := matchmakerEnvelope(RoomVersion, searchConfig.Compatibility, searchConfig.Region, nil)
	newAdmission.GetMatchmakerAdd().StringProperties["gamefleet_search_id"] = serviceBackendTestNew
	queued, err := initializer.before(ctx, nil, nil, nil, newAdmission)
	if err != nil || queued != newAdmission {
		t.Fatalf("exact mapped pending search was not admitted: err=%v", err)
	}

	oldStatusPayload := serviceBackendSearchStatusPayload(serviceBackendTestOld)
	statusRaw, err := initializer.rpcs[SearchStatusRPC](ctx, nil, nil, nil, oldStatusPayload)
	var historical SearchStatus
	if err != nil || json.Unmarshal([]byte(statusRaw), &historical) != nil || historical.Search.ID != serviceBackendTestOld {
		t.Fatalf("History SearchStatus failed to read its original search: %s %v", statusRaw, err)
	}

	currentRaw, err := initializer.rpcs[CurrentRPC](ctx, nil, nil, nil, serviceBackendCurrentPayload())
	if err != nil || !strings.Contains(currentRaw, `"current":null`) {
		t.Fatalf("Current null response unexpected: %q %v", currentRaw, err)
	}
	statusRaw, err = initializer.rpcs[StatusRPC](ctx, nil, nil, nil, serviceBackendRoomPayload(reservation.AllocationID))
	var oldStatus ReservationStatus
	if err != nil || json.Unmarshal([]byte(statusRaw), &oldStatus) != nil || oldStatus.Reservation.AllocationID != reservation.AllocationID {
		t.Fatalf("historical allocation status failed after Current null: %s %v", statusRaw, err)
	}
	mu.Lock()
	for _, req := range seen {
		if req.path == "/business/v1/service-searches" || req.path == "/business/v1/service-searches/match" {
			mu.Unlock()
			t.Fatal("Current null or historical status triggered new search/match work")
		}
	}
	mu.Unlock()

	a := bridgeMatchEntry("player-a", "ticket-a", cfg).(bridgeEntry)
	b := bridgeMatchEntry("player-b", "ticket-b", cfg).(bridgeEntry)
	a.properties["gamefleet_search_id"] = "search_player_a"
	b.properties["gamefleet_search_id"] = "search_player_b"
	for range 2 {
		_, err = initializer.matched(context.Background(), nil, nil, nil, []runtime.MatchmakerEntry{b, a})
		assertBridgeError(t, err, 14, "gamefleet temporarily unavailable; retry the same request")
	}

	mu.Lock()
	requests := append([]serviceBackendRequest(nil), seen...)
	mu.Unlock()
	searchStatusRoutes := 0
	historyStatusRoutes := 0
	matchRoutes := 0
	currentRoutes := 0
	roomStatusRoutes := 0
	for _, req := range requests {
		switch {
		case strings.Contains(req.path, "/service-searches/") && strings.HasSuffix(req.path, "/status"):
			searchStatusRoutes++
		case strings.Contains(req.path, "/history/searches/") && strings.HasSuffix(req.path, "/status"):
			historyStatusRoutes++
		case req.path == "/business/v1/service-searches/match":
			matchRoutes++
		case req.path == "/business/v1/history/reservations/current":
			currentRoutes++
		case strings.HasSuffix(req.path, "/history/reservations/allocation-service-001/status"):
			roomStatusRoutes++
		}
	}
	if searchStatusRoutes != 2 || historyStatusRoutes != 1 || matchRoutes != 2 || currentRoutes != 1 || roomStatusRoutes != 1 {
		t.Fatalf("history fallback or service matching route boundary changed: mapped=%d historySearch=%d match=%d current=%d roomStatus=%d requests=%+v",
			searchStatusRoutes, historyStatusRoutes, matchRoutes, currentRoutes, roomStatusRoutes, requests)
	}
	if !reflect.DeepEqual(requests[len(requests)-1].body, requests[len(requests)-2].body) {
		t.Fatal("repeated matched callback changed its fixed pair request")
	}
	for _, req := range requests {
		if req.query != "" || req.auth != "Bearer "+serviceSearchTestKey() {
			t.Fatal("service/history bridge request carried unexpected query or authentication")
		}
	}
}

func TestServiceBackendCancelRejectsMissingRuntimeIdentity(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		serviceBackendTestError(w, http.StatusInternalServerError)
	}))
	defer server.Close()
	search, history := serviceBackendClients(t, server.URL, server.URL)
	backend, err := newServiceBackend(search, history)
	if err != nil {
		t.Fatal("create service backend")
	}
	_, err = backend.Cancel(context.Background(), "allocation-service-001")
	serviceBackendTestAssertError(t, err, http.StatusForbidden)
	if requests != 0 {
		t.Fatal("cancel without runtime identity made a request")
	}
}
