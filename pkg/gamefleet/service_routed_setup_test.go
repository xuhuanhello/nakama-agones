package gamefleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
)

type routedSetupRequest struct {
	method string
	path   string
	query  string
	auth   string
	body   []byte
}

type routedSetupFixture struct {
	cfg         ServiceSearchConfig
	server      *httptest.Server
	mu          sync.Mutex
	requests    []routedSetupRequest
	closedConns atomic.Int32
}

func newRoutedSetupFixture(t *testing.T, cfg ServiceSearchConfig) *routedSetupFixture {
	t.Helper()
	f := &routedSetupFixture{cfg: cfg}
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(f.serveHTTP))
	f.server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			f.closedConns.Add(1)
		}
	}
	f.server.Start()
	f.cfg.URL = f.server.URL
	return f
}

func (f *routedSetupFixture) add(r routedSetupRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.body = append([]byte(nil), r.body...)
	f.requests = append(f.requests, r)
}

func (f *routedSetupFixture) snapshot() []routedSetupRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]routedSetupRequest(nil), f.requests...)
	for i := range out {
		out[i].body = append([]byte(nil), out[i].body...)
	}
	return out
}

func (f *routedSetupFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.cfg.Key || r.URL.RawQuery != "" {
		http.Error(w, "denied", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/business/v1/history/service" {
		f.add(routedSetupRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization")})
		serviceSearchRespond(w, http.StatusOK, map[string]any{
			"serviceId": f.cfg.ServiceID, "applicationId": f.cfg.ApplicationID, "identityIssuer": f.cfg.IdentityIssuer,
			"operations": []string{"read", "cancel", "assignment", "resume"},
		})
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.add(routedSetupRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization"), body: raw})
	if r.Method != http.MethodPost {
		serviceBackendTestError(w, http.StatusNotFound)
		return
	}
	switch r.URL.Path {
	case "/business/v1/service-searches":
		var body struct {
			Version       string `json:"version"`
			ParticipantID string `json:"participantId"`
			RequestID     string `json:"requestId"`
			Region        string `json:"region"`
			Compatibility string `json:"compatibility"`
		}
		if json.Unmarshal(raw, &body) != nil || body.Version != ServiceSearchVersion || body.ParticipantID != "player-one" ||
			body.RequestID == "" || body.Region != f.cfg.Region || body.Compatibility != f.cfg.Compatibility {
			serviceBackendTestError(w, http.StatusBadRequest)
			return
		}
		serviceSearchRespond(w, http.StatusCreated, SearchResult{Search: routedSetupSearch("search_v1_setup_001", f.cfg, "pending"), Replay: false})
	case "/business/v2/service-searches":
		var body struct {
			Version       string `json:"version"`
			ParticipantID string `json:"participantId"`
			RequestID     string `json:"requestId"`
			Region        string `json:"region"`
			Compatibility string `json:"compatibility"`
		}
		if json.Unmarshal(raw, &body) != nil || body.Version != ServiceSearchRoutedVersion || body.ParticipantID != "player-one" ||
			body.RequestID == "" || body.Region != f.cfg.Region || body.Compatibility != f.cfg.Compatibility {
			serviceBackendTestError(w, http.StatusBadRequest)
			return
		}
		serviceSearchRespond(w, http.StatusCreated, RoutedSearchResult{
			Version: ServiceSearchRoutedVersion, Search: routedSetupSearch("search_v2_setup_001", f.cfg, "pending"),
			MatchPoolID: routedServiceSearchTestPool(), Replay: false,
		})
	case "/business/v1/service-searches/match":
		var body map[string]json.RawMessage
		if json.Unmarshal(raw, &body) != nil || body["version"] == nil || body["members"] == nil {
			serviceBackendTestError(w, http.StatusBadRequest)
			return
		}
		var version string
		if json.Unmarshal(body["version"], &version) != nil || version != ServiceSearchVersion || body["matchPoolId"] != nil {
			serviceBackendTestError(w, http.StatusBadRequest)
			return
		}
		reservation := serviceSearchTestReservation("reserved")
		reservation.Region = f.cfg.Region
		serviceSearchRespond(w, http.StatusAccepted, ReservationResult{Reservation: reservation, Replay: false})
	case "/business/v1/history/reservations/current":
		serviceHistoryRespond(w, http.StatusOK, CurrentResult{Current: nil})
	case "/business/v1/history/searches/search_history_status_001/status":
		serviceHistoryRespond(w, http.StatusOK, SearchStatus{Search: routedSetupSearch("search_history_status_001", f.cfg, "pending")})
	case "/business/v1/history/searches/search_history_cancel_001/cancel":
		serviceHistoryRespond(w, http.StatusOK, SearchResult{Search: routedSetupSearch("search_history_cancel_001", f.cfg, "cancelled"), Replay: false})
	default:
		if id, ok := routedSetupStatusID(r.URL.Path, "/business/v2/service-searches/"); ok {
			serviceSearchRespond(w, http.StatusOK, RoutedSearchStatus{
				Version: ServiceSearchRoutedVersion, Search: routedSetupSearch(id, f.cfg, "pending"),
				MatchPoolID: routedServiceSearchTestPool(),
			})
			return
		}
		if id, ok := routedSetupStatusID(r.URL.Path, "/business/v1/service-searches/"); ok {
			serviceSearchRespond(w, http.StatusOK, SearchStatus{Search: routedSetupSearch(id, f.cfg, "pending")})
			return
		}
		serviceBackendTestError(w, http.StatusNotFound)
	}
}

func routedSetupStatusID(path, prefix string) (string, bool) {
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, "/status") {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(path, prefix), "/status")
	return id, attemptID.MatchString(id)
}

func routedSetupSearch(id string, cfg ServiceSearchConfig, state string) Search {
	created := time.Date(2026, 9, 30, 18, 0, 0, 0, time.UTC)
	s := Search{ID: id, State: state, Region: cfg.Region, Compatibility: cfg.Compatibility,
		CreatedAt: created, ExpiresAt: created.Add(300 * time.Second)}
	if state == "cancelled" {
		resolved := created.Add(time.Minute)
		s.ResolvedAt = &resolved
	}
	return s
}

func routedSetupProfilePayload(t *testing.T, version string, cfg ServiceSearchConfig) string {
	t.Helper()
	raw, err := json.Marshal(profileRequest{Version: version, Region: cfg.Region, Compatibility: cfg.Compatibility})
	if err != nil {
		t.Fatal("marshal test profile")
	}
	return string(raw)
}

func routedSetupParticipantPayload(t *testing.T, version, id string, cfg ServiceSearchConfig) string {
	t.Helper()
	raw, err := json.Marshal(searchParticipantRequest{
		profileRequest: profileRequest{Version: version, Region: cfg.Region, Compatibility: cfg.Compatibility}, SearchID: id,
	})
	if err != nil {
		t.Fatal("marshal test search participant request")
	}
	return string(raw)
}

func clearRoutedSetupEnv(t *testing.T) {
	t.Helper()
	clearServiceEnvironment(t)
	t.Setenv("GAMEFLEET_SERVICE_SEARCH_PROTOCOL", "")
}

func waitRoutedSetupClosed(t *testing.T, f *routedSetupFixture, count int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for f.closedConns.Load() < count && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.closedConns.Load(); got < count {
		t.Fatalf("shutdown closed %d idle connections, want at least %d", got, count)
	}
}

func TestRegisterServiceSearchProtocolRejectsUnknownBeforeNetwork(t *testing.T) {
	clearRoutedSetupEnv(t)
	cfg := serviceSetupConfig("")
	f := newRoutedSetupFixture(t, cfg)
	defer f.server.Close()
	keyFile := writeServiceKey(t, t.TempDir(), "service.key", cfg.Key, 0600)
	setServiceEnvironment(t, f.cfg, keyFile)
	t.Setenv("GAMEFLEET_SERVICE_SEARCH_PROTOCOL", "v3")
	i := &archiveSetupInitializer{}
	if err := RegisterServiceFromEnv(context.Background(), &serviceSetupLogger{}, nil, nil, i); err == nil {
		t.Fatal("unknown service search protocol was accepted")
	}
	if len(f.snapshot()) != 0 || len(i.registered) != 0 || i.shutdown != nil {
		t.Fatal("unknown protocol performed network I/O or registered hooks")
	}
}

func TestRegisterServiceSearchProtocolV2RoutesBeginAdmissionAndMatchWithoutLeakingPool(t *testing.T) {
	clearRoutedSetupEnv(t)
	cfg := serviceSetupConfig("")
	f := newRoutedSetupFixture(t, cfg)
	defer f.server.Close()
	keyFile := writeServiceKey(t, t.TempDir(), "service.key", cfg.Key, 0600)
	setServiceEnvironment(t, f.cfg, keyFile)
	t.Setenv("GAMEFLEET_SERVICE_SEARCH_PROTOCOL", "v2")
	initializer := &archiveSetupInitializer{}
	if err := RegisterServiceFromEnv(context.Background(), &serviceSetupLogger{}, nil, nil, initializer); err != nil {
		t.Fatalf("register v2 service bridge: %v", err)
	}
	wantRegistrations := []string{"before:MatchmakerAdd", "matched", "rpc:" + CurrentRPC, "rpc:" + StatusRPC,
		"rpc:" + AssignmentRPC, "rpc:" + ResumeRPC, "rpc:" + CancelRPC, "rpc:" + SearchBeginRPC,
		"rpc:" + SearchStatusRPC, "rpc:" + SearchCancelRPC}
	if !reflect.DeepEqual(initializer.registered, wantRegistrations) || initializer.before == nil || initializer.matched == nil ||
		initializer.rpcs == nil || initializer.shutdown == nil {
		t.Fatal("v2 setup did not register the established bridge and shutdown hooks")
	}

	ctx := bridgeCtx("player-one")
	beginRaw, err := initializer.rpcs[SearchBeginRPC](ctx, nil, nil, nil,
		bridgeSearchBeginPayloadForConfig("request_begin_001", f.cfg))
	if err != nil {
		t.Fatalf("v2 Begin RPC failed: %v", err)
	}
	var begin map[string]json.RawMessage
	if json.Unmarshal([]byte(beginRaw), &begin) != nil || len(begin) != 2 || begin["search"] == nil || begin["replay"] == nil ||
		begin["version"] != nil || begin["matchPoolId"] != nil || strings.Contains(beginRaw, "gfsp_") {
		t.Fatalf("player-facing Begin RPC exposed private route fields: %s", beginRaw)
	}

	pool := routedServiceSearchTestPool()
	in := matchmakerEnvelope(RoomVersion, f.cfg.Compatibility, f.cfg.Region, nil)
	add := in.GetMatchmakerAdd()
	add.Query = "client supplied query"
	add.StringProperties["gamefleet_search_id"] = "search_player_a"
	add.StringProperties[routedPoolProperty] = "spoofed-pool"
	add.StringProperties[routedQueueProperty] = "spoofed-queue"
	add.NumericProperties[routedPoolProperty] = 1
	queued, err := initializer.before(ctx, nil, nil, nil, in)
	if err != nil {
		t.Fatalf("v2 MatchmakerAdd admission failed: %v", err)
	}
	if queued == in || queued.GetMatchmakerAdd().Query != "+properties."+routedPoolProperty+":"+pool+" +properties."+routedQueueProperty+":"+routedQueueProtocol ||
		queued.GetMatchmakerAdd().StringProperties[routedPoolProperty] != pool ||
		queued.GetMatchmakerAdd().StringProperties[routedQueueProperty] != routedQueueProtocol ||
		queued.GetMatchmakerAdd().NumericProperties[routedPoolProperty] != 0 ||
		queued.GetMatchmakerAdd().StringProperties["custom"] != "kept" {
		t.Fatal("v2 admission did not replace all client-controlled route fields with the checked route")
	}
	if add.Query != "client supplied query" || add.StringProperties[routedPoolProperty] != "spoofed-pool" || add.NumericProperties[routedPoolProperty] != 1 {
		t.Fatal("v2 admission mutated the caller's input envelope")
	}

	if raw, callErr := initializer.rpcs[CurrentRPC](ctx, nil, nil, nil, routedSetupProfilePayload(t, RoomVersion, f.cfg)); callErr != nil || !strings.Contains(raw, `"current":null`) {
		t.Fatalf("Current did not remain on History: %s %v", raw, callErr)
	}
	if _, err := initializer.rpcs[SearchStatusRPC](ctx, nil, nil, nil,
		routedSetupParticipantPayload(t, SearchVersion, "search_history_status_001", f.cfg)); err != nil {
		t.Fatalf("SearchStatus did not remain on History: %v", err)
	}
	if _, err := initializer.rpcs[SearchCancelRPC](ctx, nil, nil, nil,
		routedSetupParticipantPayload(t, SearchVersion, "search_history_cancel_001", f.cfg)); err != nil {
		t.Fatalf("SearchCancel did not remain on History: %v", err)
	}

	matchCfg := Config{Region: f.cfg.Region, Compatibility: f.cfg.Compatibility}
	entries := []runtime.MatchmakerEntry{bridgeMatchEntry("player-a", "ticket-a", matchCfg), bridgeMatchEntry("player-b", "ticket-b", matchCfg)}
	for _, entry := range entries {
		props := entry.GetProperties()
		props[routedPoolProperty] = pool
		props[routedQueueProperty] = routedQueueProtocol
	}
	if _, err := initializer.matched(ctx, nil, nil, nil, entries); err != nil {
		t.Fatalf("v2 matched callback did not complete the established v1 match route: %v", err)
	}

	requests := f.snapshot()
	wantPaths := []string{
		"/business/v1/history/service", "/business/v1/history/service",
		"/business/v2/service-searches", "/business/v2/service-searches/search_player_a/status",
		"/business/v1/history/reservations/current",
		"/business/v1/history/searches/search_history_status_001/status",
		"/business/v1/history/searches/search_history_cancel_001/cancel",
		"/business/v2/service-searches/search_player_a/status",
		"/business/v2/service-searches/search_player_b/status",
		"/business/v1/service-searches/match",
	}
	if len(requests) != len(wantPaths) {
		t.Fatalf("v2 setup made %d requests, expected %d", len(requests), len(wantPaths))
	}
	for i, req := range requests {
		if req.path != wantPaths[i] || req.query != "" || req.auth != "Bearer "+f.cfg.Key {
			t.Fatalf("v2 setup request %d crossed route/auth boundary: method=%s path=%s", i, req.method, req.path)
		}
	}
	var matchBody map[string]json.RawMessage
	if json.Unmarshal(requests[len(requests)-1].body, &matchBody) != nil || matchBody["matchPoolId"] != nil {
		t.Fatal("Nakama matched callback leaked private pool into the established Match request")
	}
	var matchVersion string
	if json.Unmarshal(matchBody["version"], &matchVersion) != nil || matchVersion != ServiceSearchVersion {
		t.Fatal("Nakama matched callback did not use the established v1 Match protocol")
	}

	initializer.shutdown(context.Background(), nil, (*sql.DB)(nil), nil)
	waitRoutedSetupClosed(t, f, 3)
}

func TestRegisterServiceSearchProtocolDefaultAndV1KeepLegacyProfileBeyondV2Limit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol string
	}{
		{name: "unset defaults to v1", protocol: ""},
		{name: "explicit v1", protocol: "v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearRoutedSetupEnv(t)
			cfg := serviceSetupConfig("")
			cfg.Region = strings.Repeat("r", 65)
			f := newRoutedSetupFixture(t, cfg)
			defer f.server.Close()
			keyFile := writeServiceKey(t, t.TempDir(), "service.key", cfg.Key, 0600)
			setServiceEnvironment(t, f.cfg, keyFile)
			t.Setenv("GAMEFLEET_SERVICE_SEARCH_PROTOCOL", tc.protocol)
			i := &archiveSetupInitializer{}
			if err := RegisterServiceFromEnv(context.Background(), &serviceSetupLogger{}, nil, nil, i); err != nil {
				t.Fatalf("legacy setup rejected a profile valid for v1: %v", err)
			}
			ctx := bridgeCtx("player-one")
			if _, err := i.rpcs[SearchBeginRPC](ctx, nil, nil, nil, bridgeSearchBeginPayloadForConfig("request_v1_001", f.cfg)); err != nil {
				t.Fatalf("v1 Begin RPC failed for 65-byte region: %v", err)
			}
			in := matchmakerEnvelope(RoomVersion, f.cfg.Compatibility, f.cfg.Region, nil)
			in.GetMatchmakerAdd().StringProperties["gamefleet_search_id"] = "search_player_a"
			in.GetMatchmakerAdd().Query = "properties.custom:preserved"
			queued, err := i.before(ctx, nil, nil, nil, in)
			if err != nil || queued != in || queued.GetMatchmakerAdd().Query != "properties.custom:preserved" {
				t.Fatalf("v1 admission changed established client query/properties: err=%v", err)
			}
			requests := f.snapshot()
			wantPaths := []string{
				"/business/v1/history/service", "/business/v1/history/service",
				"/business/v1/service-searches", "/business/v1/service-searches/search_player_a/status",
			}
			if len(requests) != len(wantPaths) {
				t.Fatalf("default/v1 setup made %d requests, expected %d", len(requests), len(wantPaths))
			}
			for index, req := range requests {
				if req.path != wantPaths[index] {
					t.Fatalf("default/v1 setup crossed protocol route at request %d: %s", index, req.path)
				}
			}
			var beginBody map[string]json.RawMessage
			if json.Unmarshal(requests[2].body, &beginBody) != nil {
				t.Fatal("v1 Begin request was not JSON")
			}
			var version, region string
			if json.Unmarshal(beginBody["version"], &version) != nil || version != ServiceSearchVersion ||
				json.Unmarshal(beginBody["region"], &region) != nil || region != f.cfg.Region {
				t.Fatal("default/v1 setup changed the legacy protocol or long profile")
			}
			var statusBody map[string]json.RawMessage
			if json.Unmarshal(requests[3].body, &statusBody) != nil || json.Unmarshal(statusBody["version"], &version) != nil || version != ServiceSearchVersion {
				t.Fatal("default/v1 MatchmakerAdd status did not use the legacy route protocol")
			}
			i.shutdown(context.Background(), nil, (*sql.DB)(nil), nil)
			waitRoutedSetupClosed(t, f, 2)
		})
	}
}

func TestRegisterServiceSearchProtocolV2RejectsProfileAboveByteLimitBeforeNetwork(t *testing.T) {
	clearRoutedSetupEnv(t)
	cfg := serviceSetupConfig("")
	cfg.Compatibility = strings.Repeat("c", 65)
	f := newRoutedSetupFixture(t, cfg)
	defer f.server.Close()
	keyFile := writeServiceKey(t, t.TempDir(), "service.key", cfg.Key, 0600)
	setServiceEnvironment(t, f.cfg, keyFile)
	t.Setenv("GAMEFLEET_SERVICE_SEARCH_PROTOCOL", "v2")
	i := &archiveSetupInitializer{}
	if err := RegisterServiceFromEnv(context.Background(), &serviceSetupLogger{}, nil, nil, i); err == nil {
		t.Fatal("v2 setup accepted a compatibility string beyond its UTF-8 byte limit")
	}
	if len(f.snapshot()) != 0 || len(i.registered) != 0 || i.shutdown != nil {
		t.Fatal("invalid v2 profile reached the service or registered hooks")
	}
}

func bridgeSearchBeginPayloadForConfig(requestID string, cfg ServiceSearchConfig) string {
	raw, _ := json.Marshal(searchBeginRequest{
		profileRequest: profileRequest{Version: SearchVersion, Compatibility: cfg.Compatibility, Region: cfg.Region},
		RequestID:      requestID,
	})
	return string(raw)
}

var _ runtime.MatchmakerEntry = bridgeEntry{}
