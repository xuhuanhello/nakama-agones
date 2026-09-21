package console

import (
	"context"
	"encoding/json"
	"github.com/xuhuanhello/nakama-agones/internal/state"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testUpstream(t *testing.T, h http.HandlerFunc) (upstream, string) {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte("first-token"), 0600); err != nil {
		t.Fatal(err)
	}
	u, e := newUpstream(s.URL, "", p, true)
	if e != nil {
		t.Fatal(e)
	}
	return u, p
}
func TestProjectionOmitsCredentialsAndPreservesPlayers(t *testing.T) {
	s := state.New()
	w := strings.Repeat("a", 32)
	s.Workers[w] = &state.Worker{ID: w, State: "ready", Region: "us-west", MaxRooms: 2, PlayerCount: 1}
	s.Allocations["a"] = &state.Allocation{ID: "a", RoomID: "room-1", WorkerID: w, State: "active", RequestKey: "private-request-key", UserIDs: []string{"user-1", "user-2"}, Sessions: []state.Session{{ID: "private-reservation", UserID: "user-1", Connected: true, EverConnected: true, ReconnectUntil: 123}}}
	s.Commands["secret-command"] = &state.Command{ID: "secret-command"}
	out := map[string]any{}
	projectFleet(out, s)
	b, _ := json.Marshal(out)
	for _, s := range []string{"private-request-key", "private-reservation", "secret-command", "request_key", "reservation_id"} {
		if strings.Contains(string(b), s) {
			t.Fatalf("unsafe projection: %s", s)
		}
	}
	for _, s := range []string{"user-1", "user-2", "room-1", "occupied_rooms", "reconnect_until"} {
		if !strings.Contains(string(b), s) {
			t.Fatalf("missing %s", s)
		}
	}
	if n := out["workers"].([]any)[0].(map[string]any)["occupied_rooms"]; n != 1 {
		t.Fatalf("rooms=%v", n)
	}
}
func TestTokenRotationAndRedirectRefusal(t *testing.T) {
	var got []string
	u, p := testUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		w.Write([]byte(`{}`))
	})
	var v any
	if e := u.get(context.Background(), "/one", &v); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(p, []byte("second-token\n"), 0600)
	if e := u.get(context.Background(), "/two", &v); e != nil {
		t.Fatal(e)
	}
	if got[0] != "Bearer first-token" || got[1] != "Bearer second-token" {
		t.Fatal("token not reloaded")
	}
	hit := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer target.Close()
	redirect, _ := testUpstream(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) })
	if e := redirect.get(context.Background(), "/", &v); e == nil || hit {
		t.Fatal("credential-bearing redirect followed")
	}
}
func TestUpstreamErrorAndSizeAreBounded(t *testing.T) {
	u, _ := testUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/large" {
			w.Write([]byte(strings.Repeat("x", 100)))
			return
		}
		w.WriteHeader(500)
		w.Write([]byte("password=private"))
	})
	if _, e := u.request(context.Background(), "GET", "/error", nil, 10); e == nil || strings.Contains(e.Error(), "private") {
		t.Fatal("upstream error leaked")
	}
	if _, e := u.request(context.Background(), "GET", "/large", nil, 10); e == nil || e.Error() != "upstream_response_limit" {
		t.Fatal("missing body limit")
	}
}
func TestRejectUnsafeSourceURLs(t *testing.T) {
	for _, s := range []string{"http://example.com", "https://user:secret@example.com", "https://example.com/proxy", "https://example.com/?token=x", "file:///etc/passwd"} {
		if _, e := newUpstream(s, "", "/tmp/token", true); e == nil {
			t.Fatalf("accepted %s", s)
		}
	}
}
func TestQuantityAndRedaction(t *testing.T) {
	for s, want := range map[string]float64{"500m": .5, "150000000n": .15, "512Mi": 536870912, "2Gi": 2147483648, "2e3": 2000} {
		if got := quantity(s); math.Abs(got-want) > math.Max(1, math.Abs(want))*1e-12 {
			t.Fatalf("%s got%v want%v", s, got, want)
		}
	}
	for _, s := range []string{"Authorization: Bearer abc", "{\"password\":\"abc\"}", "postgres://user:pw@db:5432/x", "access_token=secret", "bootstrap_token: x"} {
		if !strings.HasPrefix(safeText(s), "[REDACTED:") {
			t.Fatalf("not redacted %q", s)
		}
	}
	normal := "DM_FLEET_READY worker=abcd rooms=2"
	if safeText(normal) != normal {
		t.Fatal("normal diagnostic removed")
	}
}
func TestLogsLiveAllowlistRedactionAndHistorySelector(t *testing.T) {
	var query string
	u, _ := testUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "query_range"):
			query = r.URL.Query().Get("query")
			w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[{"values":[["1700000000110000000","later"],["1700000000100000000","earlier"]]}]}}`))
		case strings.HasSuffix(r.URL.Path, "/log"):
			w.Write([]byte("2026-09-22T00:00:00Z DM_FLEET_READY\n2026-09-22T00:00:01Z Authorization: Bearer private\n"))
		default:
			w.Write([]byte(`{"metadata":{"labels":{"app.kubernetes.io/managed-by":"nakama-agones"}},"spec":{"containers":[{"name":"game"}],"initContainers":[{"name":"agones-gameserver-sidecar"}]}}`))
		}
	})
	d := &DataSource{cfg: SourceConfig{LogRetentionDays: 7}, regions: []regionSource{{cfg: RegionConfig{Name: "us-west", Namespace: "agones-games", SystemNamespace: "agones-system", LogNamespace: "agones-observability", LogService: "fleet-loki"}, up: u}}}
	q := url.Values{"region": {"us-west"}, "pod": {"nag-" + strings.Repeat("b", 32)}, "container": {"game"}, "mode": {"live"}}
	v, e := d.Logs(context.Background(), q)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), "private") || !strings.Contains(string(b), "DM_FLEET_READY") {
		t.Fatal(string(b))
	}
	q.Set("mode", "history")
	q.Set("search", `"} |= "attack`)
	history, e := d.Logs(context.Background(), q)
	if e != nil {
		t.Fatal(e)
	}
	entries := history.(map[string]any)["entries"].([]logEntry)
	if len(entries) != 2 || entries[0].Text != "earlier" || entries[1].Text != "later" {
		t.Fatal("fractional timestamps sorted incorrectly")
	}
	if !strings.Contains(query, `cluster="us-west"`) || !strings.Contains(query, `|= "\"} |= \"attack"`) {
		t.Fatalf("unsafe selector %s", query)
	}
	q.Set("namespace", "default")
	if _, e = d.Logs(context.Background(), q); e == nil {
		t.Fatal("namespace escaped")
	}
	q.Del("namespace")
	q.Set("pod", "../../secrets")
	if _, e = d.Logs(context.Background(), q); e == nil {
		t.Fatal("path escaped")
	}
	q.Set("pod", "nag-"+strings.Repeat("b", 32))
	q.Set("limit", "100000")
	if _, e = d.Logs(context.Background(), q); e == nil {
		t.Fatal("unbounded logs")
	}
	q.Del("limit")
	q.Set("extra", "/secrets")
	if _, e = d.Logs(context.Background(), q); e == nil {
		t.Fatal("accepted proxy parameter")
	}
}
func TestRegionMetricsFailureRemainsMissing(t *testing.T) {
	u, _ := testUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "metrics.k8s.io") {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/api/v1/nodes" {
			w.Write([]byte(`{"items":[{"metadata":{"name":"game-1","labels":{"nakama-agones.io/role":"game"}},"status":{"capacity":{"cpu":"2","memory":"2Gi"},"allocatable":{"cpu":"1700m","memory":"1400Mi"},"conditions":[{"type":"Ready","status":"True"}]}}]}`))
			return
		}
		w.Write([]byte(`{"items":[]}`))
	})
	r := regionSource{RegionConfig{Name: "us-west", Namespace: "agones-games", SystemNamespace: "agones-system"}, u}
	v := r.snapshot(context.Background()).(map[string]any)
	if v["metrics_ok"] != false || v["ok"] != true {
		t.Fatal(v)
	}
	n := v["nodes"].([]any)[0].(map[string]any)
	if _, ok := n["cpu_millicores"]; ok {
		t.Fatal("missing metrics became zero")
	}
	if n["cpu_allocatable_millicores"] != 1700.0 {
		t.Fatal(n)
	}
}
func TestSnapshotCollapsesRefreshAndReportsFailure(t *testing.T) {
	calls := 0
	u, _ := testUpstream(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(503) })
	d := &DataSource{fleet: u, cfg: SourceConfig{LogRetentionDays: 7}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	v, e := d.Snapshot(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if v.(map[string]any)["fleet"].(map[string]any)["ok"] != false {
		t.Fatal("failed fleet looked healthy")
	}
	d.Snapshot(ctx)
	if calls != 1 {
		t.Fatalf("cache calls=%d", calls)
	}
}

func TestUnmeasuredWorkerMetricsAreUnknown(t *testing.T) {
	s := state.New()
	s.Workers["worker"] = &state.Worker{ID: strings.Repeat("a", 32), State: "launching"}
	out := map[string]any{}
	projectFleet(out, s)
	if out["workers"].([]any)[0].(map[string]any)["metrics"] != nil {
		t.Fatal("unmeasured metrics must be null")
	}
}

func TestReadonlySnapshotRejectsManagementAndStaleness(t *testing.T) {
	d := &DataSource{cfg: SourceConfig{FleetSnapshotFile: "/private/state.json"}}
	if e := d.Drain(context.Background(), strings.Repeat("a", 32)); e == nil || e.Error() != "management_disabled" {
		t.Fatal("read-only drain accepted")
	}
	if e := d.RetryCreation(context.Background()); e == nil {
		t.Fatal("read-only retry accepted")
	}
	p := filepath.Join(t.TempDir(), "fleet.json")
	os.WriteFile(p, []byte(`{"ok":true,"rooms":[],"workers":[]}`), 0640)
	if _, e := readFleetSnapshot(p); e != nil {
		t.Fatal(e)
	}
	old := time.Now().Add(-30 * time.Second)
	os.Chtimes(p, old, old)
	if _, e := readFleetSnapshot(p); e == nil || e.Error() != "fleet_snapshot_stale" {
		t.Fatal("stale state accepted")
	}
	os.Remove(p)
	os.Symlink("/etc/passwd", p)
	if _, e := readFleetSnapshot(p); e == nil {
		t.Fatal("symlink accepted")
	}
}
