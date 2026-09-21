package console

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

type fakeBackend struct {
	Snapshots int
	Drains    []string
	Retries   int
	Query     url.Values
	Err       error
	Deadline  time.Time
}

func (f *fakeBackend) Snapshot(ctx context.Context) (any, error) {
	f.Snapshots++
	f.Deadline, _ = ctx.Deadline()
	return map[string]int{"workers": 2}, f.Err
}

func (f *fakeBackend) Logs(ctx context.Context, query url.Values) (any, error) {
	f.Query = query
	f.Deadline, _ = ctx.Deadline()
	return map[string]any{"lines": []string{"safe line"}}, f.Err
}

func (f *fakeBackend) Drain(_ context.Context, workerID string) error {
	f.Drains = append(f.Drains, workerID)
	return f.Err
}

func (f *fakeBackend) RetryCreation(context.Context) error {
	f.Retries++
	return f.Err
}

func testFiles() fs.FS {
	return fstest.MapFS{"index.html": {Data: []byte(`<!doctype html><script src="app.js"></script>`)}, "app.js": {Data: []byte(`"use strict";`)}, "hidden/.env": {Data: []byte("not-public")}}
}

func newTestServer(t *testing.T) (*Server, *fakeBackend) {
	t.Helper()
	backend := &fakeBackend{}
	s, err := NewServer(testConfig(t), backend, testFiles())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, backend
}

func request(method, target, body string) *http.Request {
	r := httptest.NewRequest(method, "http://127.0.0.1:17365"+target, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	if method == http.MethodPost {
		r.Header.Set("Origin", "http://127.0.0.1:17365")
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func authenticate(t *testing.T, s *Server) (*http.Cookie, string) {
	t.Helper()
	w := httptest.NewRecorder()
	current, err := s.createSession(w, request(http.MethodPost, apiPrefix+"login", "{}"))
	if err != nil {
		t.Fatal(err)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing session cookie")
	}
	return cookies[0], current.CSRF
}

func perform(s *Server, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestAPIRequiresAuthenticationAndKnownRoute(t *testing.T) {
	s, backend := newTestServer(t)
	for _, item := range []struct{ method, route string }{{"GET", "session"}, {"GET", "snapshot"}, {"GET", "logs"}, {"POST", "drain"}, {"POST", "retry-creation"}, {"POST", "logout"}} {
		w := perform(s, request(item.method, apiPrefix+item.route, "{}"))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s unauthenticated: %d", item.route, w.Code)
		}
	}
	if backend.Snapshots != 0 || len(backend.Drains) != 0 || backend.Retries != 0 {
		t.Fatal("unauthenticated call reached backend")
	}
	for _, route := range []string{"proxy", "admin/status", "session/", "login/anything"} {
		if got := perform(s, request("GET", apiPrefix+route, "")).Code; got != 404 {
			t.Errorf("unexpected exposed route %s: %d", route, got)
		}
	}
	w := perform(s, request("DELETE", apiPrefix+"session", ""))
	if w.Code != 405 || w.Header().Get("Allow") != "GET" {
		t.Fatal("unsupported method was not rejected")
	}
}

func TestLoginCreatesCookieAndRotatesExistingSession(t *testing.T) {
	s, _ := newTestServer(t)
	oldCookie, _ := authenticate(t, s)
	data, _ := json.Marshal(map[string]string{"username": "operator", "password": testPassword})
	r := request("POST", apiPrefix+"login", string(data))
	r.AddCookie(oldCookie)
	w := perform(s, r)
	if w.Code != 200 {
		t.Fatalf("login failed: %d", w.Code)
	}
	var result struct {
		Username string `json:"username"`
		CSRF     string `json:"csrf_token"`
		Expires  string `json:"expires_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Username != "operator" || len(result.CSRF) != 43 {
		t.Fatal("invalid session response")
	}
	expires, err := time.Parse(time.RFC3339, result.Expires)
	if err != nil || time.Until(expires) < sessionLifetime-time.Minute || time.Until(expires) > sessionLifetime {
		t.Fatal("unexpected session lifetime")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing login cookie")
	}
	cookie := cookies[0]
	if cookie.Value == oldCookie.Value || !cookie.HttpOnly || cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != adminPrefix || cookie.Domain != "" {
		t.Fatal("unsafe loopback cookie or missing session rotation")
	}
	r = request("GET", apiPrefix+"session", "")
	r.AddCookie(oldCookie)
	if perform(s, r).Code != 401 {
		t.Fatal("old session survived re-login")
	}
	r = request("GET", apiPrefix+"session", "")
	r.AddCookie(cookie)
	if perform(s, r).Code != 200 {
		t.Fatal("new session not usable")
	}
	for _, input := range []map[string]string{{"username": "other", "password": testPassword}, {"username": "operator", "password": "incorrect password"}} {
		data, _ := json.Marshal(input)
		failed := perform(s, request("POST", apiPrefix+"login", string(data)))
		if failed.Code != 401 || failed.Body.String() != "{\"error\":\"invalid_credentials\"}\n" || len(failed.Result().Cookies()) != 0 {
			t.Fatal("failed login disclosed credential information")
		}
	}
}

func TestHTTPSCookiePrefixRequiresSecureAndRootPath(t *testing.T) {
	cfg := testConfig(t)
	cfg.PublicURL = "https://admin.example/fleet-admin/"
	s, err := NewServer(cfg, &fakeBackend{}, testFiles())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cookie, _ := authenticate(t, s)
	if cookie.Name != "__Host-fleet-admin" || !cookie.Secure || !cookie.HttpOnly || cookie.Path != "/" || cookie.Domain != "" || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("invalid secure cookie")
	}
}

func TestOriginAndCSRFCannotBeBypassed(t *testing.T) {
	s, backend := newTestServer(t)
	cookie, csrf := authenticate(t, s)
	for _, origin := range []string{"", "null", "https://evil.example", "http://localhost:17365", "http://127.0.0.1:17365/", "http://127.0.0.1:17365, https://evil.example"} {
		for _, route := range []string{"login", "drain"} {
			r := request("POST", apiPrefix+route, `{"worker_id":"worker-1"}`)
			r.Header.Set("Origin", origin)
			r.Header.Set("X-CSRF-Token", csrf)
			r.AddCookie(cookie)
			if got := perform(s, r).Code; got != 403 {
				t.Errorf("origin %q accepted for %s: %d", origin, route, got)
			}
		}
	}
	for _, route := range []string{"logout", "drain", "retry-creation"} {
		for _, token := range []string{"", "bad-token"} {
			r := request("POST", apiPrefix+route, "{}")
			r.AddCookie(cookie)
			r.Header.Set("X-CSRF-Token", token)
			if perform(s, r).Code != 403 {
				t.Fatalf("mutation %s accepted missing/wrong CSRF", route)
			}
		}
	}
	for _, header := range []string{"Origin", "X-CSRF-Token"} {
		r := request("POST", apiPrefix+"drain", `{"worker_id":"worker-1"}`)
		r.AddCookie(cookie)
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Add(header, r.Header.Get(header))
		if perform(s, r).Code != 403 {
			t.Fatalf("duplicate %s header accepted", header)
		}
	}
	r := request("POST", apiPrefix+"drain", `{"worker_id":"worker-1"}`)
	r.AddCookie(cookie)
	r.Header.Set("X-CSRF-Token", csrf)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if perform(s, r).Code != 403 || len(backend.Drains) != 0 {
		t.Fatal("cross-site request reached backend")
	}
}

func TestAuthenticatedOperationsAndLogout(t *testing.T) {
	s, backend := newTestServer(t)
	cookie, csrf := authenticate(t, s)
	for _, item := range []struct{ method, route, body string }{
		{"GET", "snapshot", ""}, {"GET", "logs?region=us&worker_id=worker-1", ""},
		{"POST", "drain", `{"worker_id":"worker-1"}`}, {"POST", "retry-creation", "{}"},
	} {
		r := request(item.method, apiPrefix+item.route, item.body)
		r.AddCookie(cookie)
		r.Header.Set("X-CSRF-Token", csrf)
		if w := perform(s, r); w.Code != 200 {
			t.Fatalf("operation %s failed: %d", item.route, w.Code)
		}
	}
	if backend.Snapshots != 1 || len(backend.Drains) != 1 || backend.Drains[0] != "worker-1" || backend.Retries != 1 || backend.Query.Get("region") != "us" {
		t.Fatal("backend operation mismatch")
	}
	if backend.Deadline.IsZero() || time.Until(backend.Deadline) > backendTimeout {
		t.Fatal("backend request was not deadline-bound")
	}
	r := request("POST", apiPrefix+"logout", "{}")
	r.AddCookie(cookie)
	r.Header.Set("X-CSRF-Token", csrf)
	w := perform(s, r)
	if w.Code != 200 || len(w.Result().Cookies()) != 1 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("logout did not clear browser cookie")
	}
	r = request("GET", apiPrefix+"snapshot", "")
	r.AddCookie(cookie)
	if perform(s, r).Code != 401 {
		t.Fatal("logged-out cookie remained valid")
	}
}

func TestJSONValidationAndRequestLimit(t *testing.T) {
	for _, item := range []struct {
		body   string
		status int
	}{
		{"", 400}, {"null", 400}, {"[]", 400}, {"{} {}", 400}, {`{"extra":true}`, 400}, {"{broken", 400},
		{strings.Repeat(" ", requestLimit+1), 413}, {`{"worker_id":"../../admin"}`, 400},
	} {
		t.Run(item.body[:min(len(item.body), 30)], func(t *testing.T) {
			s, backend := newTestServer(t)
			cookie, csrf := authenticate(t, s)
			r := request("POST", apiPrefix+"drain", item.body)
			r.AddCookie(cookie)
			r.Header.Set("X-CSRF-Token", csrf)
			if w := perform(s, r); w.Code != item.status || len(backend.Drains) != 0 {
				t.Fatalf("invalid body accepted: status %d", w.Code)
			}
		})
	}
	s, _ := newTestServer(t)
	r := request("POST", apiPrefix+"login", `username=operator&password=secret`)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if perform(s, r).Code != 415 {
		t.Fatal("form login accepted")
	}
}

func TestPathHostAndQueryBoundaries(t *testing.T) {
	s, backend := newTestServer(t)
	cookie, _ := authenticate(t, s)
	for _, route := range []string{"/fleet-admin/api/%73napshot", "/fleet-admin/api//snapshot", "/fleet-admin/../api/snapshot", "/fleet-admin/%2e%2e/config.json", "/fleet-admin/hidden/.env", "/fleet-admin/hidden/", "/fleet-admin/api/../../index.html"} {
		r := request("GET", route, "")
		r.AddCookie(cookie)
		if perform(s, r).Code != 404 {
			t.Errorf("path alias or hidden file served: %s", route)
		}
	}
	r := request("GET", apiPrefix+"snapshot", "")
	r.Host = "evil.example"
	r.AddCookie(cookie)
	if perform(s, r).Code != 421 || backend.Snapshots != 0 {
		t.Fatal("unexpected Host reached backend")
	}
	for _, query := range []string{"region=us&region=eu", "broken=%zz", "a;b=x"} {
		r := request("GET", apiPrefix+"logs?"+query, "")
		r.AddCookie(cookie)
		if perform(s, r).Code != 400 || backend.Query != nil {
			t.Fatal("ambiguous logs query reached backend")
		}
	}
	r = request("GET", apiPrefix+"logs?query="+strings.Repeat("a", queryLimit), "")
	r.AddCookie(cookie)
	if perform(s, r).Code != 414 {
		t.Fatal("oversized query accepted")
	}
}

type safeBackendError struct {
	code   string
	status int
}

func (e safeBackendError) Error() string   { return e.code }
func (e safeBackendError) HTTPStatus() int { return e.status }

func TestBackendErrorsNeverExposeUpstreamDetails(t *testing.T) {
	s, backend := newTestServer(t)
	cookie, _ := authenticate(t, s)
	for _, item := range []struct {
		err    error
		status int
		code   string
	}{
		{errors.New("url?token=private-secret"), 502, "upstream_unavailable"},
		{safeBackendError{"invalid_log_query", 400}, 400, "invalid_log_query"},
		{safeBackendError{"unsafe error token=private-secret", 500}, 502, "upstream_unavailable"},
		{safeBackendError{"wrong_status", 200}, 502, "upstream_unavailable"},
	} {
		backend.Err = item.err
		r := request("GET", apiPrefix+"snapshot", "")
		r.AddCookie(cookie)
		w := perform(s, r)
		if w.Code != item.status || !strings.Contains(w.Body.String(), item.code) || strings.Contains(w.Body.String(), "private-secret") {
			t.Fatal("unsafe backend error response")
		}
	}
}

func TestStaticSecurityHeadersAndNoDirectoryListing(t *testing.T) {
	s, _ := newTestServer(t)
	for _, route := range []string{adminPrefix, adminPrefix + "app.js", apiPrefix + "session", adminPrefix + "missing"} {
		w := perform(s, request("GET", route, ""))
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || strings.Contains(w.Header().Get("Content-Security-Policy"), "unsafe-inline") || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("missing security headers or unexpected CORS/inline relaxation")
		}
	}
	if w := perform(s, request("HEAD", adminPrefix, "")); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatal("static HEAD returned a body")
	}
	if w := perform(s, request("GET", "/fleet-admin", "")); w.Code != 308 || w.Header().Get("Location") != adminPrefix {
		t.Fatal("noncanonical static root must redirect locally")
	}
}

func TestReadonlySourceRejectsAuthenticatedMutationThroughHTTP(t *testing.T) {
	// Even a future configuration regression setting the management flag cannot
	// turn a snapshot-backed console into a write-capable backend.
	backend := &DataSource{cfg: SourceConfig{FleetSnapshotFile: "/not-used-by-mutations/state.json", AllowManagement: true}}
	s, err := NewServer(testConfig(t), backend, testFiles())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cookie, csrf := authenticate(t, s)
	for _, item := range []struct{ route, body string }{
		{"drain", `{"worker_id":"0123456789abcdef0123456789abcdef"}`},
		{"retry-creation", "{}"},
	} {
		r := request(http.MethodPost, apiPrefix+item.route, item.body)
		r.AddCookie(cookie)
		r.Header.Set("X-CSRF-Token", csrf)
		w := perform(s, r)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "management_disabled") {
			t.Fatalf("read-only source accepted authenticated %s: %d", item.route, w.Code)
		}
	}
}
