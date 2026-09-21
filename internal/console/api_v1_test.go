package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type queryBackend struct {
	value     any
	mutations int
	logsQuery url.Values
}

func (q *queryBackend) Snapshot(context.Context) (any, error) { return q.value, nil }
func (q *queryBackend) Logs(_ context.Context, v url.Values) (any, error) {
	q.logsQuery = v
	return map[string]any{"observed_at": "2026-09-22T00:00:00Z", "entries": []any{}, "truncated": false, "next_before": ""}, nil
}
func (q *queryBackend) Drain(context.Context, string) error { q.mutations++; return nil }
func (q *queryBackend) RetryCreation(context.Context) error { q.mutations++; return nil }

func queryFixture(t *testing.T) (*Server, *queryBackend, string) {
	t.Helper()
	cfg := testConfig(t)
	token, hash, err := GenerateReadAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ReadAPITokenHash = hash
	value := map[string]any{
		"observed_at": "2026-09-22T00:00:00Z", "log_retention_days": 7,
		"fleet": map[string]any{"ok": true, "can_manage": true, "revision": 1, "max_processes": 4, "rooms_per_process": 2,
			"workers": []any{map[string]any{"id": "worker-b", "region": "us-west", "state": "draining"}, map[string]any{"id": "worker-a", "region": "us-west", "state": "ready", "admin_token": "must-not-leak", "metrics": map[string]any{"frame_p99_ms": 10, "private": "must-not-leak"}}},
			"rooms":   []any{map[string]any{"id": "room-b", "allocation_id": "alloc-b", "region": "us-west", "worker_id": "worker-b", "state": "completed", "players": []any{}}, map[string]any{"id": "room-a", "allocation_id": "alloc-a", "region": "us-west", "worker_id": "worker-a", "state": "active", "players": []any{map[string]any{"user_id": "user-a", "connected": true, "admission_token": "must-not-leak"}}}}},
		"regions": []any{map[string]any{"name": "us-west", "ok": true, "metrics_ok": true,
			"nodes":  []any{map[string]any{"name": "game-1", "ready": true, "role": "game"}},
			"pods":   []any{map[string]any{"name": "nag-worker-a", "worker_id": "worker-a", "namespace": "agones-games", "node": "game-1", "containers": []string{"game"}}},
			"events": []any{map[string]any{"namespace": "agones-games", "object_name": "nag-worker-a", "type": "Warning", "reason": "BackOff", "message": "example", "time": "2026-09-22T00:00:00Z"}}}},
	}
	backend := &queryBackend{value: value}
	server, err := NewServer(cfg, backend, testFiles())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	return server, backend, token
}

func apiRequest(token, resource string) *http.Request {
	r := request("GET", apiV1Prefix+resource, "")
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}
func responseObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestReadTokenScopeCannotCreateOrUseBrowserSession(t *testing.T) {
	s, backend, token := queryFixture(t)
	cookie, csrf := authenticate(t, s)
	if perform(s, apiRequest(token, "overview")).Code != 200 {
		t.Fatal("read API token rejected")
	}
	for _, path := range []string{"session", "snapshot", "logs", "login", "logout", "drain", "retry-creation"} {
		method := "GET"
		if path == "login" || path == "logout" || path == "drain" || path == "retry-creation" {
			method = "POST"
		}
		r := request(method, apiPrefix+path, "{}")
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-CSRF-Token", csrf)
		r.AddCookie(cookie)
		if w := perform(s, r); w.Code != 403 || !strings.Contains(w.Body.String(), "api_token_scope") {
			t.Fatalf("token escaped read scope at %s", path)
		}
	}
	if backend.mutations != 0 {
		t.Fatal("API token reached write backend")
	}
	r := apiRequest("invalid", "rooms")
	r.AddCookie(cookie)
	if perform(s, r).Code != 401 {
		t.Fatal("invalid explicit token fell back to cookie")
	}
	r = apiRequest(token, "rooms")
	r.Header.Add("Authorization", "Bearer "+token)
	if perform(s, r).Code != 401 {
		t.Fatal("duplicate authorization accepted")
	}
	r = apiRequest(token, "rooms")
	r.Host = "other.local"
	if perform(s, r).Code != 421 {
		t.Fatal("API token bypassed exact Host")
	}
	r = apiRequest(token, "rooms?token="+url.QueryEscape(token))
	if perform(s, r).Code != 400 {
		t.Fatal("token query was accepted")
	}
	if w := perform(s, request("POST", apiV1Prefix+"rooms", "{}")); w.Code != 405 {
		t.Fatal("versioned API exposed a write method")
	}
}

func TestVersionedQueryFiltersPaginationAndProjection(t *testing.T) {
	s, _, token := queryFixture(t)
	w := perform(s, apiRequest(token, "rooms?limit=1"))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	value := responseObject(t, w.Body.Bytes())
	page := value["page"].(map[string]any)
	rows := value["data"].([]any)
	if value["api_version"] != "v1" || value["read_only"] != true || page["total"] != float64(2) || page["next_offset"] != float64(1) || rows[0].(map[string]any)["id"] != "room-a" || strings.Contains(w.Body.String(), "must-not-leak") {
		t.Fatal("incorrect bounded room projection")
	}
	for _, query := range []string{"rooms?user_id=user-a&state=active", "instances?node=game-1&worker_id=worker-a", "nodes?role=game&ready=true", "events?type=Warning&reason=BackOff"} {
		w := perform(s, apiRequest(token, query))
		value := responseObject(t, w.Body.Bytes())
		if w.Code != 200 || value["page"].(map[string]any)["total"] != float64(1) || strings.Contains(w.Body.String(), "must-not-leak") {
			t.Fatalf("query projection failed: %s", query)
		}
	}
	w = perform(s, apiRequest(token, "rooms?offset=99"))
	value = responseObject(t, w.Body.Bytes())
	if len(value["data"].([]any)) != 0 || value["page"].(map[string]any)["next_offset"] != nil {
		t.Fatal("pagination end is not explicit")
	}
	w = perform(s, apiRequest(token, "overview"))
	value = responseObject(t, w.Body.Bytes())
	fleet := value["data"].(map[string]any)["fleet"].(map[string]any)
	if fleet["can_manage"] != false || fleet["rooms_active"] != float64(1) || fleet["players_connected"] != float64(1) {
		t.Fatal("overview misrepresented caller capability or active counts")
	}
}

func TestVersionedQueriesRejectAmbiguityAndShowUnavailable(t *testing.T) {
	s, backend, token := queryFixture(t)
	for _, query := range []string{"rooms?limit=201", "rooms?limit=0", "rooms?offset=-1", "rooms?offset=100001", "rooms?limit=1&limit=2", "rooms?url=http://other/", "rooms?region=missing", "nodes?ready=yes", "overview?limit=1", "logs?query={anything}"} {
		if perform(s, apiRequest(token, query)).Code != 400 {
			t.Errorf("invalid query accepted: %s", query)
		}
	}
	snapshot := backend.value.(map[string]any)
	fleet := snapshot["fleet"].(map[string]any)
	fleet["ok"] = false
	fleet["error"] = "fleet_snapshot_stale"
	w := perform(s, apiRequest(token, "rooms"))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "fleet_snapshot_stale") {
		t.Fatal("unavailable fleet returned an empty success")
	}
	w = perform(s, apiRequest(token, "overview"))
	value := responseObject(t, w.Body.Bytes())
	f := value["data"].(map[string]any)["fleet"].(map[string]any)
	if value["partial"] != true || f["rooms_total"] != nil {
		t.Fatal("unknown counts looked like zero")
	}
}

func TestVersionedLogsAndBrowserSessionCompatibility(t *testing.T) {
	s, backend, token := queryFixture(t)
	before := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	w := perform(s, apiRequest(token, "logs?region=us-west&mode=history&pod=nag-example&before="+url.QueryEscape(before)+"&limit=10"))
	if w.Code != 200 || backend.logsQuery.Get("before") != before {
		t.Fatal("history log cursor not passed to bounded backend")
	}
	cookie, _ := authenticate(t, s)
	r := request("GET", apiV1Prefix+"overview", "")
	r.AddCookie(cookie)
	w = perform(s, r)
	value := responseObject(t, w.Body.Bytes())
	if w.Code != 200 || value["read_only"] != false || value["data"].(map[string]any)["fleet"].(map[string]any)["can_manage"] != true {
		t.Fatal("browser API session lost its separate capability")
	}
}
