package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func controlFixture(t *testing.T, upstream http.HandlerFunc) (*ControlServer, *httptest.Server) {
	t.Helper()
	remote := httptest.NewServer(upstream)
	t.Cleanup(remote.Close)
	s, err := NewControlServer(ControlConfig{SocketPath: "/run/fleet-console-control/test.sock", FleetURL: remote.URL, CredentialsFile: "/root/fixture.json"})
	if err != nil {
		t.Fatal(err)
	}
	s.credential = func() (string, error) { return "fixture-admin-secret", nil }
	return s, remote
}

func controlRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, "http://"+controlHost+path, strings.NewReader(body))
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func TestControlBrokerFixedActionsAndSafeResponses(t *testing.T) {
	var calls atomic.Int32
	s, _ := controlFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer fixture-admin-secret" {
			t.Error("broker did not authenticate to runtime")
		}
		switch r.URL.Path {
		case "/agones/fleet/v1/admin/status":
			if r.Method != "GET" {
				t.Error("unexpected status method")
			}
			io.WriteString(w, `{"revision":1,"workers":{},"allocations":{},"private":"must-not-leak"}`)
		case "/agones/fleet/v1/admin/drain":
			var input map[string]string
			if json.NewDecoder(r.Body).Decode(&input) != nil || len(input) != 1 || input["worker_id"] != strings.Repeat("a", 32) {
				t.Error("unexpected drain payload")
			}
			w.WriteHeader(202)
			io.WriteString(w, `{"accepted":true,"private":"must-not-leak"}`)
		case "/agones/fleet/v1/admin/retry-creation":
			raw, _ := io.ReadAll(r.Body)
			if string(raw) != "{}" {
				t.Error("unexpected retry body")
			}
			w.WriteHeader(202)
			io.WriteString(w, `{"accepted":true}`)
		default:
			t.Error("broker forwarded an arbitrary path")
			w.WriteHeader(500)
		}
	})
	for _, item := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/v1/capabilities", "", 200}, {"POST", "/v1/drain", `{"worker_id":"` + strings.Repeat("a", 32) + `"}`, 202}, {"POST", "/v1/retry-creation", "{}", 202},
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, controlRequest(item.method, item.path, item.body))
		if w.Code != item.status || strings.Contains(w.Body.String(), "must-not-leak") || strings.Contains(w.Body.String(), "fixture-admin-secret") {
			t.Fatalf("unsafe control response status=%d", w.Code)
		}
	}
	if calls.Load() != 3 {
		t.Fatal("missing fixed action calls")
	}
	for _, item := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/v1/proxy", "{}", 404}, {"POST", "/v1/allocate", "{}", 404}, {"POST", "/v1/drain/extra", "{}", 404},
		{"POST", "/v1/%64rain", "{}", 404}, {"POST", "/v1/drain?url=http://other/", "{}", 404},
		{"GET", "/v1/drain", "", 405}, {"POST", "/v1/capabilities", "{}", 405},
		{"POST", "/v1/drain", `{"worker_id":"../status"}`, 400}, {"POST", "/v1/drain", `{"worker_id":"` + strings.Repeat("a", 32) + `","path":"/other"}`, 400},
		{"POST", "/v1/retry-creation", `{"shell":"whoami"}`, 400}, {"POST", "/v1/retry-creation", strings.Repeat(" ", requestLimit+1), 413},
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, controlRequest(item.method, item.path, item.body))
		if w.Code != item.status {
			t.Errorf("%s %s status=%d", item.method, item.path, w.Code)
		}
	}
	if calls.Load() != 3 {
		t.Fatal("rejected input reached runtime")
	}
}

func TestControlBrokerRejectsRedirectsAndMapsFailures(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer target.Close()
	var status atomic.Int32
	status.Store(302)
	s, _ := controlFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(int(status.Load()))
		io.WriteString(w, "secret-body")
	})
	for _, item := range []struct {
		upstream, status int
		code             string
	}{{302, 502, "control_unavailable"}, {403, 502, "control_access_denied"}, {404, 404, "worker_not_found"}, {409, 409, "worker_not_drainable"}} {
		status.Store(int32(item.upstream))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, controlRequest("POST", "/v1/drain", `{"worker_id":"`+strings.Repeat("b", 32)+`"}`))
		if w.Code != item.status || !strings.Contains(w.Body.String(), item.code) || strings.Contains(w.Body.String(), "secret-body") {
			t.Fatal("unsafe control upstream failure")
		}
	}
	if redirected.Load() != 0 {
		t.Fatal("broker followed a redirect")
	}
}

func TestControlCredentialsReloadAndFileRestrictions(t *testing.T) {
	file := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(file, []byte(`{"admin_token":"first-secret","other":"private"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if token, err := readControlCredential(file, os.Geteuid()); err != nil || token != "first-secret" {
		t.Fatal("credential was not read")
	}
	if err := os.WriteFile(file, []byte(`{"admin_token":"second-secret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if token, err := readControlCredential(file, os.Geteuid()); err != nil || token != "second-secret" {
		t.Fatal("rotated credential was not read")
	}
	link := file + ".link"
	if os.Symlink(file, link) != nil {
		t.Fatal("cannot prepare symlink fixture")
	}
	if _, err := readControlCredential(link, os.Geteuid()); err == nil {
		t.Fatal("symlink credential accepted")
	}
	if err := os.Chmod(file, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := readControlCredential(file, os.Geteuid()); err == nil {
		t.Fatal("group-readable root credential accepted")
	}
	os.Chmod(file, 0600)
	for _, data := range []string{`{"admin_token":"one","admin_token":"two"}`, `{"admin_token":"line\nbreak"}`, `{"admin_token":null}`, strings.Repeat("x", (1<<20)+1)} {
		os.WriteFile(file, []byte(data), 0600)
		if _, err := readControlCredential(file, os.Geteuid()); err == nil || strings.Contains(err.Error(), data) {
			t.Fatal("unsafe credential input accepted or echoed")
		}
	}
}

func TestControlURLAndSocketProfile(t *testing.T) {
	base := ControlConfig{SocketPath: "/run/fleet-console-control/control.sock", FleetURL: "http://127.0.0.1:7350", CredentialsFile: "/root/runtime.json"}
	if err := base.validate(); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"http://10.0.0.2:7350", "http://localhost:7350", "http://127.0.0.1:0", "http://127.0.0.1:99999", "http://user:pass@127.0.0.1:7350", "http://127.0.0.1:7350/path", "http://127.0.0.1:7350?target=other", "ftp://127.0.0.1"} {
		cfg := base
		cfg.FleetURL = value
		if cfg.validate() == nil {
			t.Errorf("unsafe broker target accepted %s", value)
		}
	}
	for _, value := range []string{"relative.sock", "/run/../tmp/a.sock", strings.Repeat("/x", 60)} {
		cfg := base
		cfg.SocketPath = value
		if cfg.validate() == nil {
			t.Error("unsafe socket path accepted")
		}
	}
}

func TestUnixControlIntegrationOwnershipAndRecovery(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "fc-control-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "control.sock")
	os.Chmod(dir, 0750)
	listener, err := listenControlSocket(path, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := verifyControlSocket(path, os.Geteuid()); err != nil {
		t.Fatal(err)
	}
	if second, err := listenControlSocket(path, os.Geteuid()); err == nil {
		second.Close()
		t.Fatal("active socket was replaced")
	}
	broker, _ := controlFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, `{"revision":1,"workers":{},"allocations":{}}`)
		} else {
			w.WriteHeader(202)
			io.WriteString(w, `{"accepted":true}`)
		}
	})
	server := &http.Server{Handler: broker}
	defer server.Close()
	go server.Serve(listener)
	client, err := newControlClient(path)
	if err != nil {
		t.Fatal(err)
	}
	client.owner = os.Geteuid()
	if err = client.capabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = client.action(context.Background(), "drain", map[string]string{"worker_id": strings.Repeat("c", 32)}); err != nil {
		t.Fatal(err)
	}
	if err = client.action(context.Background(), "delete", struct{}{}); err == nil {
		t.Fatal("arbitrary client action accepted")
	}
	snapshot := filepath.Join(dir, "fleet.json")
	if err := os.WriteFile(snapshot, []byte(`{"ok":true,"revision":1,"rooms":[],"workers":[]}`), 0640); err != nil {
		t.Fatal(err)
	}
	d := &DataSource{cfg: SourceConfig{FleetSnapshotFile: snapshot, AllowManagement: true}, control: client}
	value, err := d.Snapshot(context.Background())
	if err != nil || value.(map[string]any)["fleet"].(map[string]any)["can_manage"] != true {
		t.Fatal("healthy snapshot and broker did not enable the restricted actions")
	}
	if err := d.Drain(context.Background(), strings.Repeat("c", 32)); err != nil || d.cached != nil {
		t.Fatal("accepted broker action did not invalidate the cached observation")
	}
	if err := d.RetryCreation(context.Background()); err != nil {
		t.Fatal("restricted retry was not accepted")
	}
	os.Chmod(dir, 0770)
	if verifyControlSocket(path, os.Geteuid()) == nil {
		t.Fatal("group-writable parent allowed socket spoofing")
	}
	os.Chmod(dir, 0750)
	os.Chmod(path, 0666)
	if verifyControlSocket(path, os.Geteuid()) == nil {
		t.Fatal("world-writable socket accepted")
	}
	server.Close()
	listener.Close()
	regular := filepath.Join(dir, "regular.sock")
	os.WriteFile(regular, []byte("do not delete"), 0600)
	if _, err = listenControlSocket(regular, os.Geteuid()); err == nil {
		t.Fatal("regular file was replaced")
	}
}

func TestSnapshotManagementCapabilityDoesNotBreakReadStatus(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "fc-caps-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	snapshot := filepath.Join(dir, "fleet.json")
	os.WriteFile(snapshot, []byte(`{"ok":true,"revision":1,"rooms":[],"workers":[]}`), 0640)
	client, _ := newControlClient(filepath.Join(dir, "missing.sock"))
	d := &DataSource{cfg: SourceConfig{FleetSnapshotFile: snapshot, AllowManagement: true}, control: client}
	value, err := d.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fleet := value.(map[string]any)["fleet"].(map[string]any)
	if fleet["ok"] != true || fleet["can_manage"] != false || fleet["management_error"] != "control_unavailable" {
		t.Fatal("broker outage changed read health or enabled management")
	}
}

func TestManagementCannotUseWebProcessFleetCredential(t *testing.T) {
	base := SourceConfig{FleetURL: "http://127.0.0.1:7350", FleetTokenFile: "/not/read/token", AllowManagement: true, Regions: []RegionConfig{{Name: "unused"}}}
	if _, err := NewDataSource(base); err == nil || !strings.Contains(err.Error(), "restricted control socket") {
		t.Fatal("direct administrator-token management remained available")
	}
	base.ControlSocket = "/run/fleet-console-control/control.sock"
	if _, err := NewDataSource(base); err == nil || !strings.Contains(err.Error(), "credential-free snapshot") {
		t.Fatal("web process could combine a broker and full Fleet credential")
	}
}

func TestControlConcurrencyIsBounded(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	s, _ := controlFixture(t, func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		io.WriteString(w, `{"revision":1,"workers":{},"allocations":{}}`)
	})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.ServeHTTP(httptest.NewRecorder(), controlRequest("GET", "/v1/capabilities", ""))
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("broker did not dispatch bounded requests")
		}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, controlRequest("GET", "/v1/capabilities", ""))
	releaseOnce.Do(func() { close(release) })
	wg.Wait()
	if w.Code != 429 || !strings.Contains(w.Body.String(), "control_busy") {
		t.Fatal("broker concurrency was unbounded")
	}
}
