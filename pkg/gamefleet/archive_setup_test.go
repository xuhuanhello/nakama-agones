package gamefleet

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/heroiclabs/nakama-common/runtime"
)

func clearArchiveEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range archiveEnvironmentNames {
		t.Setenv(name, "")
	}
}

func setArchiveEnvironment(t *testing.T, cfg TerminalArchiveConfig, path string) {
	t.Setenv("GAMEFLEET_ARCHIVE_CA_FILE", cfg.TLS.CAFile)
	t.Setenv("GAMEFLEET_ARCHIVE_CERT_FILE", cfg.TLS.CertFile)
	t.Setenv("GAMEFLEET_ARCHIVE_TLS_KEY_FILE", cfg.TLS.KeyFile)
	t.Helper()
	for name, value := range map[string]string{
		"GAMEFLEET_ARCHIVE_URL": cfg.URL, "GAMEFLEET_ARCHIVE_KEY_FILE": path,
		"GAMEFLEET_ARCHIVE_APPLICATION_ID": cfg.ApplicationID, "GAMEFLEET_ARCHIVE_IDENTITY_ISSUER": cfg.IdentityIssuer,
		"GAMEFLEET_ARCHIVE_REGION": cfg.Region, "GAMEFLEET_ARCHIVE_COMPATIBILITY": cfg.Compatibility,
		"GAMEFLEET_ARCHIVE_SERVICE_ID": cfg.ServiceID,
	} {
		t.Setenv(name, value)
	}
}

func TestArchiveEnvironmentIsExplicitAndIndependent(t *testing.T) {
	clearArchiveEnvironment(t)
	t.Setenv("GAMEFLEET_BUSINESS_KEY_FILE", "/private/primary-key-marker")
	t.Setenv("GAMEFLEET_APPLICATION_ID", "primary-app-marker")
	t.Setenv("GAMEFLEET_REGION", "primary-region-marker")
	t.Setenv("GAMEFLEET_COMPATIBILITY", "primary-build-marker")
	if cfg, err := ArchiveConfigFromEnv(); err != nil || cfg != nil {
		t.Fatal("blank archive configuration must remain disabled", err)
	}
	for _, name := range archiveEnvironmentNames {
		t.Run(name, func(t *testing.T) {
			clearArchiveEnvironment(t)
			t.Setenv(name, "unreported-private-marker")
			cfg, err := ArchiveConfigFromEnv()
			if err == nil || cfg != nil || strings.Contains(err.Error(), "unreported-private-marker") {
				t.Fatal("partial archive configuration must fail without inheriting primary scope")
			}
		})
	}
}

func TestArchiveEnvironmentPrivateFileAndExactScope(t *testing.T) {
	clearArchiveEnvironment(t)
	cfg := terminalArchiveConfig("http://127.0.0.1:18683")
	path := filepath.Join(t.TempDir(), "private-archive-key-marker")
	setArchiveEnvironment(t, cfg, path)
	assertDenied := func() {
		t.Helper()
		out, err := ArchiveConfigFromEnv()
		if err == nil || out != nil || strings.Contains(err.Error(), path) || strings.Contains(err.Error(), cfg.Key) {
			t.Fatal("unsafe archive key file accepted or exposed")
		}
	}
	assertDenied()
	if err := os.WriteFile(path, []byte(cfg.Key+"\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := ArchiveConfigFromEnv()
	if err != nil || out == nil || !reflect.DeepEqual(*out, cfg) {
		t.Fatal("explicit archive scope not preserved", err)
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	assertDenied()
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := path + ".link"
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GAMEFLEET_ARCHIVE_KEY_FILE", link)
	assertDenied()
	t.Setenv("GAMEFLEET_ARCHIVE_KEY_FILE", path)
	if err = os.WriteFile(path, []byte(strings.Repeat("x", 4098)), 0600); err != nil {
		t.Fatal(err)
	}
	assertDenied()
}

type archiveSetupInitializer struct {
	bridgeInitializer
	shutdown func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule)
}

func (i *archiveSetupInitializer) RegisterShutdown(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule)) error {
	i.shutdown = fn
	return nil
}

type archiveSetupLogger struct{ runtime.Logger }

func (archiveSetupLogger) Info(string, ...interface{}) {}

func setupPrimaryEnvironment(t *testing.T, origin string) {
	t.Helper()
	cfg := clientConfig(origin)
	path := filepath.Join(t.TempDir(), "primary.key")
	if err := os.WriteFile(path, []byte(cfg.Key+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"GAMEFLEET_BUSINESS_CA_FILE": cfg.TLS.CAFile, "GAMEFLEET_BUSINESS_CERT_FILE": cfg.TLS.CertFile, "GAMEFLEET_BUSINESS_TLS_KEY_FILE": cfg.TLS.KeyFile, "GAMEFLEET_BUSINESS_URL": cfg.URL, "GAMEFLEET_BUSINESS_KEY_FILE": path,
		"GAMEFLEET_APPLICATION_ID": cfg.ApplicationID, "GAMEFLEET_PLACEMENT_ID": cfg.PlacementID,
		"GAMEFLEET_REVISION_ID": cfg.RevisionID, "GAMEFLEET_REGION": cfg.Region,
		"GAMEFLEET_COMPATIBILITY": cfg.Compatibility,
	} {
		t.Setenv(name, value)
	}
}

func TestArchiveStartupPreflightPinsIdentityBeforeHooksAndUsesIndependentCredential(t *testing.T) {
	for _, mismatch := range []string{"service", "application", "issuer", "none"} {
		t.Run(mismatch, func(t *testing.T) {
			clearArchiveEnvironment(t)
			archiveCfg := terminalArchiveConfig("")
			// Current profile is unchanged while the archive source has an independent app/issuer.
			archiveCfg.Region, archiveCfg.Compatibility = "us-west", "dm-v1"
			seen := make([]string, 0)
			var seenMu sync.Mutex
			server := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seenMu.Lock()
				seen = append(seen, r.URL.Path)
				seenMu.Unlock()
				switch r.URL.Path {
				case "/business/v1/caller":
					if r.Header.Get("Authorization") != "Bearer "+clientConfig("").Key {
						t.Error("primary preflight used wrong credential")
					}
					respond(w, Scope{CallerID: "caller-one", ApplicationID: "app-one", PlacementID: "placement-one", RevisionID: "revision-one", Region: "us-west", Operations: []string{"reserve", "read", "cancel", "assignment", "resume"}})
				case "/business/v1/history/service":
					if r.Header.Get("Authorization") != "Bearer "+archiveCfg.Key {
						t.Error("archive preflight reused primary credential")
					}
					service, application, issuer := archiveCfg.ServiceID, archiveCfg.ApplicationID, archiveCfg.IdentityIssuer
					switch mismatch {
					case "service":
						service = "other-valid-service"
					case "application":
						application = "other-valid-app"
					case "issuer":
						issuer = "other-valid-issuer"
					}
					respond(w, map[string]any{"serviceId": service, "applicationId": application, "identityIssuer": issuer, "operations": []string{"read"}})
				case "/business/v1/reservations/allocation-old-001/status":
					if r.Header.Get("Authorization") != "Bearer "+clientConfig("").Key {
						t.Error("primary status used wrong credential")
					}
					w.WriteHeader(http.StatusForbidden)
				case "/business/v1/history/archive/reservations/allocation-old-001/status":
					if r.Header.Get("Authorization") != "Bearer "+archiveCfg.Key {
						t.Error("terminal status used wrong credential")
					}
					reservation := archiveTestReservation("completed")
					reservation.Region = archiveCfg.Region
					respond(w, ReservationStatus{Reservation: reservation})
				default:
					t.Error("unexpected setup or write operation", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			setupPrimaryEnvironment(t, server.URL)
			archiveCfg.URL = server.URL
			path := filepath.Join(t.TempDir(), "archive.key")
			if err := os.WriteFile(path, []byte(archiveCfg.Key+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			setArchiveEnvironment(t, archiveCfg, path)
			initializer := &archiveSetupInitializer{}
			err := RegisterFromEnv(context.Background(), archiveSetupLogger{}, nil, nil, initializer)
			if mismatch != "none" {
				if err == nil || len(initializer.registered) != 0 || initializer.shutdown != nil || err.Error() != "GameFleet archive scope preflight failed" {
					t.Fatal("wrong historical identity registered hooks or leaked diagnostics", err)
				}
				return
			}
			if err != nil || len(initializer.registered) != 10 || initializer.shutdown == nil {
				t.Fatal("valid optional archive setup failed", err)
			}
			t.Cleanup(func() { initializer.shutdown(context.Background(), nil, nil, nil) })
			body := `{"version":"gamefleet.player-room.v1","compatibility":"dm-v1","region":"us-west","allocationId":"allocation-old-001"}`
			result, err := initializer.rpcs[StatusRPC](bridgeCtx("player-one"), nil, nil, nil, body)
			if err != nil || !strings.Contains(result, `"state":"completed"`) {
				t.Fatal("registered bridge did not recover the exact terminal record", err)
			}
			want := []string{"/business/v1/caller", "/business/v1/history/service", "/business/v1/reservations/allocation-old-001/status", "/business/v1/history/archive/reservations/allocation-old-001/status"}
			seenMu.Lock()
			defer seenMu.Unlock()
			if !reflect.DeepEqual(seen, want) {
				t.Fatalf("setup/query paths = %v", seen)
			}
		})
	}
}
