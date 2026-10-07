package fleetmanager

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/xuhuanhello/nakama-agones/pkg/gamefleet"
)

type pilotInitializer struct {
	playerHookInitializer
	shutdown func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule)
}

func (i *pilotInitializer) RegisterShutdown(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule)) error {
	i.shutdown = fn
	return nil
}

type pilotLogger struct {
	runtime.Logger
	messages []string
}

func (l *pilotLogger) Info(format string, args ...interface{}) {
	l.messages = append(l.messages, format)
}

func pilotEnv(t *testing.T, origin string) {
	t.Helper()
	t.Setenv("NAKAMA_FLEET_BACKEND", "gamefleet")
	// An impossible legacy DB string must never be read by the pilot branch.
	t.Setenv("AGONES_FLEET_DATABASE_URL", "not-a-database-connection")
	for key, value := range map[string]string{"GAMEFLEET_BUSINESS_URL": origin, "GAMEFLEET_APPLICATION_ID": "app-one", "GAMEFLEET_PLACEMENT_ID": "placement-one", "GAMEFLEET_REVISION_ID": "revision-one", "GAMEFLEET_REGION": "us-west", "GAMEFLEET_COMPATIBILITY": "dm-v1"} {
		t.Setenv(key, value)
	}
	path := filepath.Join(t.TempDir(), "business.key")
	if err := os.WriteFile(path, []byte("gfbiz_"+strings.Repeat("a", 43)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GAMEFLEET_BUSINESS_KEY_FILE", path)
}

func TestGameFleetModeBypassesLegacyAndRegistersOnlyPlayerHooks(t *testing.T) {
	var calls atomic.Int64
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/business/v1/caller" || r.Method != "GET" {
			t.Errorf("unexpected preflight: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": gamefleet.Scope{CallerID: "caller-one", ApplicationID: "app-one", PlacementID: "placement-one", RevisionID: "revision-one", Region: "us-west", Operations: []string{"read", "reserve", "cancel", "assignment", "resume"}}, "requestId": "setup-test"})
	}))
	defer s.Close()
	pilotEnv(t, s.URL)
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.TLS.Certificates[0].Certificate[0]})
	key, err := x509.MarshalPKCS8PrivateKey(s.TLS.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	for suffix, data := range map[string][]byte{"CA_FILE": cert, "CERT_FILE": cert, "TLS_KEY_FILE": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})} {
		p := filepath.Join(t.TempDir(), suffix)
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GAMEFLEET_BUSINESS_"+suffix, p)
	}
	i := &pilotInitializer{}
	logger := &pilotLogger{}
	// All unimplemented Initializer methods panic via the nil embedded interface.
	// A successful setup proves no FleetManager/HTTP/admin registration was used.
	if err = RegisterFromEnv(context.Background(), logger, nil, nil, i); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || i.before == nil || i.matched == nil || i.shutdown == nil || len(i.rpcs) != 8 {
		t.Fatal("pilot hook/preflight contract missing")
	}
	for _, name := range []string{gamefleet.CurrentRPC, gamefleet.StatusRPC, gamefleet.AssignmentRPC, gamefleet.ResumeRPC, gamefleet.CancelRPC, gamefleet.SearchBeginRPC, gamefleet.SearchStatusRPC, gamefleet.SearchCancelRPC} {
		if i.rpcs[name] == nil {
			t.Fatalf("missing %s", name)
		}
	}
	if i.rpcs["agones_fleet_assignment_get_v1"] != nil {
		t.Fatal("both protocols registered")
	}
	i.shutdown(context.Background(), logger, nil, nil)
}

func TestGameFleetPreflightFailureRegistersNothing(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte("must-not-leak-secret"))
	}))
	defer s.Close()
	pilotEnv(t, s.URL)
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.TLS.Certificates[0].Certificate[0]})
	key, err := x509.MarshalPKCS8PrivateKey(s.TLS.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	for suffix, data := range map[string][]byte{"CA_FILE": cert, "CERT_FILE": cert, "TLS_KEY_FILE": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})} {
		p := filepath.Join(t.TempDir(), suffix)
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GAMEFLEET_BUSINESS_"+suffix, p)
	}
	i := &pilotInitializer{}
	err = RegisterFromEnv(context.Background(), &pilotLogger{}, nil, nil, i)
	if err == nil || strings.Contains(err.Error(), "must-not-leak-secret") || i.before != nil || i.matched != nil || len(i.rpcs) != 0 || i.shutdown != nil {
		t.Fatal("preflight did not fail closed", err)
	}
}

func TestFleetBackendDefaultAndUnknownModes(t *testing.T) {
	var err error
	t.Setenv("AGONES_FLEET_DATABASE_URL", "")
	t.Setenv("GAMEFLEET_BUSINESS_KEY_FILE", "/does-not-exist")
	for _, mode := range []string{"", "agones"} {
		t.Setenv("NAKAMA_FLEET_BACKEND", mode)
		err = RegisterFromEnv(context.Background(), nil, nil, nil, nil)
		if err == nil || err.Error() != "AGONES_FLEET_DATABASE_URL is required" {
			t.Fatalf("legacy mode %q changed: %v", mode, err)
		}
	}
	t.Setenv("NAKAMA_FLEET_BACKEND", "unexpected-secret")
	err = RegisterFromEnv(context.Background(), nil, nil, nil, nil)
	if err == nil || err.Error() != "unsupported NAKAMA_FLEET_BACKEND" {
		t.Fatal("unknown mode did not fail closed", err)
	}
	t.Setenv("NAKAMA_FLEET_BACKEND", "gamefleet")
	err = RegisterFromEnv(context.Background(), nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "GAMEFLEET_BUSINESS_KEY_FILE") {
		t.Fatal("pilot fell back to legacy", err)
	}
}
