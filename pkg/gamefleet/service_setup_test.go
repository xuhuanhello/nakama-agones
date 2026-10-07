package gamefleet

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func clearServiceEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range serviceEnvironmentNames {
		t.Setenv(name, "")
	}
	clearArchiveEnvironment(t)
}

func setServiceEnvironment(t *testing.T, cfg ServiceSearchConfig, keyFile string) {
	t.Setenv("GAMEFLEET_SERVICE_CA_FILE", cfg.TLS.CAFile)
	t.Setenv("GAMEFLEET_SERVICE_CERT_FILE", cfg.TLS.CertFile)
	t.Setenv("GAMEFLEET_SERVICE_TLS_KEY_FILE", cfg.TLS.KeyFile)
	t.Helper()
	for name, value := range map[string]string{
		"GAMEFLEET_SERVICE_URL": cfg.URL, "GAMEFLEET_SERVICE_KEY_FILE": keyFile,
		"GAMEFLEET_SERVICE_ID": cfg.ServiceID, "GAMEFLEET_SERVICE_APPLICATION_ID": cfg.ApplicationID,
		"GAMEFLEET_SERVICE_IDENTITY_ISSUER": cfg.IdentityIssuer, "GAMEFLEET_SERVICE_REGION": cfg.Region,
		"GAMEFLEET_SERVICE_COMPATIBILITY": cfg.Compatibility,
	} {
		t.Setenv(name, value)
	}
}

func serviceSetupConfig(origin string) ServiceSearchConfig {
	cfg := serviceSearchTestConfig(origin)
	cfg.Region, cfg.Compatibility = bridgeTestConfig().Region, bridgeTestConfig().Compatibility
	return cfg
}

func writeServiceKey(t *testing.T, dir, name, value string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(value), mode); err != nil {
		t.Fatal("write test key file")
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal("set test key file mode")
	}
	return path
}

func TestServiceConfigFromEnvRequiresExplicitIndependentScope(t *testing.T) {
	clearServiceEnvironment(t)
	t.Setenv("GAMEFLEET_BUSINESS_KEY_FILE", "/private/legacy-business-key-marker")
	t.Setenv("GAMEFLEET_APPLICATION_ID", "legacy-app-marker")
	t.Setenv("GAMEFLEET_PLACEMENT_ID", "legacy-placement-marker")
	t.Setenv("GAMEFLEET_REVISION_ID", "legacy-revision-marker")
	t.Setenv("GAMEFLEET_REGION", "legacy-region-marker")
	t.Setenv("GAMEFLEET_COMPATIBILITY", "legacy-compat-marker")
	if cfg, err := ServiceConfigFromEnv(); err == nil || cfg != (ServiceSearchConfig{}) || strings.Contains(err.Error(), "legacy-") {
		t.Fatal("service mode inherited or exposed legacy configuration")
	}

	cfg := serviceSetupConfig("http://127.0.0.1:18683")
	dir := t.TempDir()
	keyPath := writeServiceKey(t, dir, "service.key", cfg.Key+"\n", 0600)
	for _, missing := range serviceEnvironmentNames {
		t.Run("missing/"+missing, func(t *testing.T) {
			clearServiceEnvironment(t)
			setServiceEnvironment(t, cfg, keyPath)
			t.Setenv(missing, "")
			if got, err := ServiceConfigFromEnv(); err == nil || got != (ServiceSearchConfig{}) || strings.Contains(err.Error(), cfg.Key) || strings.Contains(err.Error(), keyPath) {
				t.Fatal("partial service configuration did not fail safely")
			}
		})
	}

	clearServiceEnvironment(t)
	setServiceEnvironment(t, cfg, keyPath)
	got, err := ServiceConfigFromEnv()
	if err != nil || got != cfg {
		t.Fatal("complete explicit service scope was not loaded", err)
	}
	if err := os.Chmod(keyPath, 0400); err != nil {
		t.Fatal("set readonly key mode")
	}
	if got, err = ServiceConfigFromEnv(); err != nil || got != cfg {
		t.Fatal("0400 key file was rejected", err)
	}
}

func TestServiceConfigFromEnvRejectsUnsafeOrWrongClassKeyFiles(t *testing.T) {
	clearServiceEnvironment(t)
	cfg := serviceSetupConfig("http://127.0.0.1:18683")
	dir := t.TempDir()
	path := writeServiceKey(t, dir, "service.key", cfg.Key, 0600)
	setServiceEnvironment(t, cfg, path)
	deny := func() {
		t.Helper()
		got, err := ServiceConfigFromEnv()
		if err == nil || got != (ServiceSearchConfig{}) || strings.Contains(err.Error(), path) || strings.Contains(err.Error(), cfg.Key) {
			t.Fatal("unsafe service key was accepted or diagnostics exposed private data")
		}
	}
	for _, mode := range []os.FileMode{0644, 0500, 0200} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal("set unsafe mode")
		}
		deny()
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := path + ".link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal("create symlink test")
	}
	t.Setenv("GAMEFLEET_SERVICE_KEY_FILE", link)
	deny()
	t.Setenv("GAMEFLEET_SERVICE_KEY_FILE", path)
	for _, bad := range []string{
		"gfbiz_" + strings.Repeat("a", 43), cfg.Key + "\n\n", cfg.Key + "\r\n\n", strings.Repeat("x", 65),
	} {
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal("write invalid test key")
		}
		deny()
	}
	if err := os.WriteFile(path, []byte(cfg.Key+"\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := ServiceConfigFromEnv(); err != nil || got != cfg {
		t.Fatal("single CRLF ending should be accepted", err)
	}
}

type serviceSetupServer struct {
	server      *httptest.Server
	search      ServiceSearchConfig
	archive     *TerminalArchiveConfig
	metadata    atomic.Int32
	closedConns atomic.Int32
	unexpected  []string
	badScopeAt  int32
	badOpsAt    int32
}

func newServiceSetupServer(t *testing.T, search ServiceSearchConfig, archive *TerminalArchiveConfig) *serviceSetupServer {
	t.Helper()
	f := &serviceSetupServer{search: search, archive: archive}
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(f.serveHTTP))
	f.server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			f.closedConns.Add(1)
		}
	}
	startBusinessTestServer(f.server)
	f.search.URL = f.server.URL
	if f.archive != nil {
		copyOfArchive := *f.archive
		copyOfArchive.URL = f.server.URL
		f.archive = &copyOfArchive
	}
	return f
}

func (f *serviceSetupServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	searchKey := f.search.Key
	archiveKey := ""
	if f.archive != nil {
		archiveKey = f.archive.Key
	}
	if (r.Header.Get("Authorization") != "Bearer "+searchKey) &&
		(r.Header.Get("Authorization") != "Bearer "+archiveKey || archiveKey == "") {
		http.Error(w, "denied", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/business/v1/history/service" {
		call := f.metadata.Add(1)
		serviceID, appID, issuer := f.search.ServiceID, f.search.ApplicationID, f.search.IdentityIssuer
		operations := []string{"read", "cancel", "assignment", "resume"}
		if archiveKey != "" && r.Header.Get("Authorization") == "Bearer "+archiveKey {
			serviceID, appID, issuer = f.archive.ServiceID, f.archive.ApplicationID, f.archive.IdentityIssuer
			operations = []string{"read"}
		}
		if call == f.badScopeAt {
			serviceID = "other-service-0001"
		}
		if call == f.badOpsAt {
			operations = []string{"read", "cancel", "assignment"}
		}
		serviceSearchRespond(w, http.StatusOK, map[string]any{
			"serviceId": serviceID, "applicationId": appID, "identityIssuer": issuer, "operations": operations,
		})
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if r.Body != nil && r.Method == http.MethodPost {
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if request["participantId"] != nil {
			var participant string
			if err := json.Unmarshal(request["participantId"], &participant); err != nil || participant != "player-one" {
				http.Error(w, "wrong participant", http.StatusForbidden)
				return
			}
		}
		// The server is a protocol fixture. Bodies are inspected in-memory and
		// never included in diagnostics.
		returnAfterDecode := f.serveRoute(w, r, request)
		if returnAfterDecode {
			return
		}
	}
	f.unexpected = append(f.unexpected, r.Method+" "+r.URL.Path)
	http.Error(w, "unexpected service route", http.StatusNotFound)
}

func (f *serviceSetupServer) serveRoute(w http.ResponseWriter, r *http.Request, body map[string]json.RawMessage) bool {
	profileMatches := func(version, region, compatibility string) bool {
		var v, reg, compat string
		_ = json.Unmarshal(body["version"], &v)
		_ = json.Unmarshal(body["region"], &reg)
		_ = json.Unmarshal(body["compatibility"], &compat)
		return v == version && reg == region && compat == compatibility
	}
	versionMatches := func(version string) bool {
		var v string
		_ = json.Unmarshal(body["version"], &v)
		return v == version
	}
	base := f.search
	switch r.URL.Path {
	case "/business/v1/service-searches":
		if !profileMatches(ServiceSearchVersion, base.Region, base.Compatibility) {
			http.Error(w, "bad profile", http.StatusBadRequest)
			return true
		}
		search := serviceSearchTestPending("search_new_001")
		search.Region, search.Compatibility = base.Region, base.Compatibility
		serviceSearchRespond(w, http.StatusCreated, SearchResult{Search: search, Replay: false})
		return true
	case "/business/v1/service-searches/search_player_a/status", "/business/v1/service-searches/search_player_b/status", "/business/v1/service-searches/search_player-a/status":
		if !versionMatches(ServiceSearchVersion) {
			http.Error(w, "bad profile", http.StatusBadRequest)
			return true
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/business/v1/service-searches/"), "/status")
		search := serviceSearchTestPending(id)
		search.Region, search.Compatibility = base.Region, base.Compatibility
		serviceSearchRespond(w, http.StatusOK, SearchStatus{Search: search})
		return true
	case "/business/v1/service-searches/match":
		if !profileMatches(ServiceSearchVersion, base.Region, base.Compatibility) {
			http.Error(w, "bad profile", http.StatusBadRequest)
			return true
		}
		reservation := serviceSearchTestReservation("reserved")
		reservation.Region = base.Region
		serviceSearchRespond(w, http.StatusAccepted, struct {
			Reservation Reservation `json:"reservation"`
			Replay      bool        `json:"replay"`
		}{reservation, false})
		return true
	case "/business/v1/history/reservations/current":
		if !versionMatches(RoomVersion) {
			http.Error(w, "bad profile", http.StatusBadRequest)
			return true
		}
		reservation := serviceSearchTestReservation("reserved")
		reservation.AllocationID, reservation.Region = "allocation_history_001", base.Region
		serviceSearchRespond(w, http.StatusOK, map[string]any{"current": Current{
			Reservation: reservation, ConnectionGeneration: 1,
		}})
		return true
	case "/business/v1/history/reservations/allocation_history_001/status":
		if !versionMatches(RoomVersion) {
			http.Error(w, "bad profile", http.StatusBadRequest)
			return true
		}
		reservation := serviceSearchTestReservation("completed")
		reservation.AllocationID, reservation.Region = "allocation_history_001", base.Region
		serviceSearchRespond(w, http.StatusOK, ReservationStatus{Reservation: reservation})
		return true
	case "/business/v1/history/reservations/allocation_history_001/cancel":
		if !versionMatches(RoomVersion) {
			http.Error(w, "bad profile", http.StatusBadRequest)
			return true
		}
		reservation := serviceSearchTestReservation("reserved")
		reservation.AllocationID, reservation.Region = "allocation_history_001", base.Region
		reservation.CancellationRequested = true
		serviceSearchRespond(w, http.StatusOK, struct {
			Reservation Reservation `json:"reservation"`
			Replay      bool        `json:"replay"`
		}{reservation, false})
		return true
	case "/business/v1/history/reservations/allocation_history_001/assignment", "/business/v1/history/reservations/allocation_history_001/resume":
		if !versionMatches(TicketVersion) {
			http.Error(w, "bad profile", http.StatusBadRequest)
			return true
		}
		var generation int64 = 1
		if strings.HasSuffix(r.URL.Path, "/resume") {
			generation = 2
		}
		assignment := serviceHistoryTicket(generation - 1)
		assignment.Ticket.Generation = generation
		serviceSearchRespond(w, http.StatusOK, struct {
			Assignment Assignment `json:"assignment"`
			Replay     bool       `json:"replay"`
		}{assignment, false})
		return true
	case "/business/v1/history/searches/search_cancel_001/status":
		if !versionMatches(SearchVersion) {
			http.Error(w, "bad profile", http.StatusBadRequest)
			return true
		}
		search := serviceHistorySearch("pending", "search_cancel_001")
		search.Region, search.Compatibility = base.Region, base.Compatibility
		serviceSearchRespond(w, http.StatusOK, SearchStatus{Search: search})
		return true
	case "/business/v1/history/searches/search_cancel_001/cancel":
		if !versionMatches(SearchVersion) {
			http.Error(w, "bad profile", http.StatusBadRequest)
			return true
		}
		search := serviceHistorySearch("cancelled", "search_cancel_001")
		search.Region, search.Compatibility = base.Region, base.Compatibility
		serviceSearchRespond(w, http.StatusOK, SearchResult{Search: search, Replay: false})
		return true
	}
	return false
}

func (f *serviceSetupServer) waitForClosed(t *testing.T, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for f.closedConns.Load() < want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.closedConns.Load(); got < want {
		t.Fatalf("shutdown closed %d idle client connections, want at least %d", got, want)
	}
}

type serviceSetupLogger struct {
	runtime.Logger
	messages []string
}

func (l *serviceSetupLogger) Info(message string, args ...interface{}) {
	l.messages = append(l.messages, fmt.Sprint(message, args))
}

func TestRegisterServiceFromEnvPreflightsBeforeHooksAndClosesOnMismatch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		badScopeAt int32
		badOpsAt   int32
		wantCalls  int32
	}{
		{name: "search identity mismatch", badScopeAt: 1, wantCalls: 1},
		{name: "history operations incomplete", badOpsAt: 2, wantCalls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearServiceEnvironment(t)
			cfg := serviceSetupConfig("")
			fixture := newServiceSetupServer(t, cfg, nil)
			defer fixture.server.Close()
			fixture.badScopeAt, fixture.badOpsAt = tc.badScopeAt, tc.badOpsAt
			keyFile := writeServiceKey(t, t.TempDir(), "service.key", cfg.Key, 0600)
			cfg.URL = fixture.server.URL
			setServiceEnvironment(t, cfg, keyFile)
			initializer := &archiveSetupInitializer{}
			err := RegisterServiceFromEnv(context.Background(), &serviceSetupLogger{}, nil, nil, initializer)
			if err == nil || len(initializer.registered) != 0 || initializer.shutdown != nil || fixture.metadata.Load() != tc.wantCalls {
				t.Fatal("failed service preflight registered hooks or continued unexpectedly")
			}
			fixture.waitForClosed(t, 1)
		})
	}
}

func TestRegisterServiceFromEnvOptionalArchiveFailuresRegisterNoHooks(t *testing.T) {
	t.Run("partial archive configuration", func(t *testing.T) {
		clearServiceEnvironment(t)
		cfg := serviceSetupConfig("")
		fixture := newServiceSetupServer(t, cfg, nil)
		defer fixture.server.Close()
		keyFile := writeServiceKey(t, t.TempDir(), "service.key", cfg.Key, 0600)
		cfg.URL = fixture.server.URL
		setServiceEnvironment(t, cfg, keyFile)
		t.Setenv("GAMEFLEET_ARCHIVE_URL", "http://127.0.0.1:18683")
		initializer := &archiveSetupInitializer{}
		if err := RegisterServiceFromEnv(context.Background(), &serviceSetupLogger{}, nil, nil, initializer); err == nil || len(initializer.registered) != 0 || initializer.shutdown != nil || fixture.metadata.Load() != 0 {
			t.Fatal("partial optional archive configuration reached preflight or registered hooks")
		}
	})

	t.Run("wrong archive identity", func(t *testing.T) {
		clearServiceEnvironment(t)
		cfg := serviceSetupConfig("")
		archive := terminalArchiveConfig("")
		fixture := newServiceSetupServer(t, cfg, &archive)
		defer fixture.server.Close()
		keyFile := writeServiceKey(t, t.TempDir(), "service.key", cfg.Key, 0600)
		cfg.URL = fixture.server.URL
		setServiceEnvironment(t, cfg, keyFile)
		archive.URL = fixture.server.URL
		archivePath := writeServiceKey(t, t.TempDir(), "archive.key", archive.Key, 0600)
		setArchiveEnvironment(t, archive, archivePath)
		fixture.badScopeAt = 3
		initializer := &archiveSetupInitializer{}
		if err := RegisterServiceFromEnv(context.Background(), &serviceSetupLogger{}, nil, nil, initializer); err == nil || len(initializer.registered) != 0 || initializer.shutdown != nil || fixture.metadata.Load() != 3 {
			t.Fatal("archive identity mismatch registered hooks")
		}
		fixture.waitForClosed(t, 2)
	})
}

func TestRegisterServiceFromEnvRegistersBoundHooksAndClosesAllClients(t *testing.T) {
	clearServiceEnvironment(t)
	cfg := serviceSetupConfig("")
	archive := terminalArchiveConfig("")
	fixture := newServiceSetupServer(t, cfg, &archive)
	defer fixture.server.Close()
	cfg.URL = fixture.server.URL
	archive.URL = fixture.server.URL
	keyFile := writeServiceKey(t, t.TempDir(), "service.key", cfg.Key+"\r\n", 0600)
	archivePath := writeServiceKey(t, t.TempDir(), "archive.key", archive.Key+"\n", 0400)
	setServiceEnvironment(t, cfg, keyFile)
	setArchiveEnvironment(t, archive, archivePath)
	logger := &serviceSetupLogger{}
	initializer := &archiveSetupInitializer{}
	if err := RegisterServiceFromEnv(context.Background(), logger, nil, nil, initializer); err != nil {
		t.Fatal("service bridge registration failed", err)
	}
	want := []string{"before:MatchmakerAdd", "matched", "rpc:" + CurrentRPC, "rpc:" + StatusRPC,
		"rpc:" + AssignmentRPC, "rpc:" + ResumeRPC, "rpc:" + CancelRPC, "rpc:" + SearchBeginRPC,
		"rpc:" + SearchStatusRPC, "rpc:" + SearchCancelRPC}
	if !reflect.DeepEqual(initializer.registered, want) || initializer.before == nil || initializer.matched == nil || len(initializer.rpcs) != 8 || initializer.shutdown == nil {
		t.Fatal("service mode did not register exactly the expected player bridge surfaces")
	}
	if len(logger.messages) != 1 || strings.Contains(logger.messages[0], cfg.Key) || strings.Contains(logger.messages[0], keyFile) || strings.Contains(logger.messages[0], archive.Key) || strings.Contains(logger.messages[0], archivePath) {
		t.Fatal("service logger exposed configuration data or omitted generic registration info")
	}

	ctx := bridgeCtx("player-one")
	envelope := matchmakerEnvelope(RoomVersion, cfg.Compatibility, cfg.Region, wrapperspb.Int32(2))
	if _, err := initializer.before(ctx, nil, nil, nil, envelope); err != nil {
		t.Fatal("service-backed MatchmakerAdd admission failed", err)
	}
	entries := []runtime.MatchmakerEntry{
		bridgeMatchEntry("player-one", "ticket-one", Config{TLS: fixtureTLS, Region: cfg.Region, Compatibility: cfg.Compatibility}),
		bridgeMatchEntry("player-two", "ticket-two", Config{TLS: fixtureTLS, Region: cfg.Region, Compatibility: cfg.Compatibility}),
	}
	if _, err := initializer.matched(ctx, nil, nil, nil, entries); err != nil {
		t.Fatal("mapped service match callback failed", err)
	}

	profile := bridgeProfilePayload()
	allocationID := "allocation_history_001"
	searchID := "search_cancel_001"
	requests := map[string]string{
		CurrentRPC:      profile,
		StatusRPC:       bridgeStatusPayload(allocationID),
		AssignmentRPC:   bridgeTicketPayload(allocationID, "assignment_key_001", 0),
		ResumeRPC:       bridgeTicketPayload(allocationID, "resume_key_001", 1),
		CancelRPC:       bridgeStatusPayload(allocationID),
		SearchBeginRPC:  bridgeSearchBeginPayload("begin_key_001"),
		SearchStatusRPC: bridgeSearchParticipantPayload(searchID),
		SearchCancelRPC: bridgeSearchParticipantPayload(searchID),
	}
	for _, rpc := range []string{CurrentRPC, StatusRPC, AssignmentRPC, ResumeRPC, CancelRPC, SearchBeginRPC, SearchStatusRPC, SearchCancelRPC} {
		if _, err := initializer.rpcs[rpc](ctx, nil, nil, nil, requests[rpc]); err != nil {
			t.Fatalf("service RPC %s failed: %v (unexpected routes: %v)", rpc, err, fixture.unexpected)
		}
	}
	if got, wantCalls := fixture.metadata.Load(), int32(3); got != wantCalls {
		t.Fatalf("service and history preflights did not share exact identity metadata: calls=%d want %d", got, wantCalls)
	}
	initializer.shutdown(context.Background(), nil, nil, nil)
	fixture.waitForClosed(t, 3)
}

func TestRegisterServiceFromEnvRequiresInitializerWithoutNetwork(t *testing.T) {
	clearServiceEnvironment(t)
	if err := RegisterServiceFromEnv(context.Background(), nil, nil, nil, nil); err == nil || err.Error() != "GameFleet service bridge requires an initializer" {
		t.Fatal("missing initializer was not rejected before network setup")
	}
}
