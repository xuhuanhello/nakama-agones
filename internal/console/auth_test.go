package console

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testPassword = "long console test password"

var testHashOnce sync.Once
var testHash string

func testConfig(t *testing.T) Config {
	t.Helper()
	var hashErr error
	testHashOnce.Do(func() { testHash, hashErr = HashPassword(testPassword) })
	if hashErr != nil || testHash == "" {
		t.Fatal("cannot prepare password hash")
	}
	return Config{Username: "operator", PasswordHash: testHash,
		PublicURL: "http://127.0.0.1:17365/fleet-admin/", Listen: "127.0.0.1:7365"}
}

func TestPasswordHashSaltAndVerification(t *testing.T) {
	one := testConfig(t).PasswordHash
	two, err := HashPassword(testPassword)
	if err != nil || one == two {
		t.Fatal("hashes must have independent salts")
	}
	if !strings.HasPrefix(one, "pbkdf2-sha256$600000$") || !VerifyPassword(testPassword, one) || !VerifyPassword(testPassword, two) {
		t.Fatal("password hash round trip failed")
	}
	if VerifyPassword("wrong console test password", one) || VerifyPassword(testPassword, strings.Replace(one, "$600000$", "$599999$", 1)) {
		t.Fatal("wrong password or weak iteration count accepted")
	}
	for _, encoded := range []string{"", "raw-password", strings.Replace(one, "$600000$", "$2000001$", 1), one + "=", one + "$extra", strings.Replace(one, "$600000$", "$0600000$", 1)} {
		if VerifyPassword(testPassword, encoded) {
			t.Fatalf("malformed hash accepted: %q", encoded)
		}
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short new password accepted")
	}
	if _, err := HashPassword(strings.Repeat("x", 1025)); err == nil {
		t.Fatal("oversized password accepted")
	}
}

func TestConfigRestrictsListenAndPublicURL(t *testing.T) {
	for _, public := range []string{"https://admin.example/fleet-admin/", "http://127.0.0.1:17365/fleet-admin/", "http://localhost:17365/fleet-admin/", "http://[::1]:17365/fleet-admin/"} {
		cfg := testConfig(t)
		cfg.PublicURL = public
		cfg.Listen = ""
		if err := cfg.validate(); err != nil || cfg.Listen != "127.0.0.1:7365" {
			t.Errorf("valid endpoint rejected: %s", public)
		}
	}
	for _, public := range []string{"http://example.com/fleet-admin/", "http://10.0.0.2:7365/fleet-admin/", "https://user:pass@admin.example/fleet-admin/", "https://admin.example/fleet-admin/?next=elsewhere", "https://admin.example/fleet-admin/#fragment", "https://admin.example/", "https://ADMIN.example/fleet-admin/", "https://admin.example:443/fleet-admin/", "http://127.0.0.1:80/fleet-admin/", "https://admin.example/fleet-%61dmin/"} {
		cfg := testConfig(t)
		cfg.PublicURL = public
		if err := cfg.validate(); err == nil {
			t.Errorf("unsafe or ambiguous endpoint accepted: %s", public)
		}
	}
	for _, listen := range []string{":7365", "0.0.0.0:7365", "[::]:7365", "10.0.0.1:7365", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1", "example.com:7365"} {
		cfg := testConfig(t)
		cfg.Listen = listen
		if err := cfg.validate(); err == nil {
			t.Errorf("non-loopback or invalid listener accepted: %s", listen)
		}
	}
}

func TestLoadConfigPrivateFileAndSafeErrors(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	data, _ := json.Marshal(testConfig(t))
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := LoadConfig(file); err != nil || cfg.Username != "operator" {
		t.Fatal("valid private config not loaded")
	}
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(file); err == nil {
		t.Fatal("world-readable authentication config accepted")
	}
	_ = os.Chmod(file, 0600)
	link := filepath.Join(dir, "symlink.json")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(link); err == nil {
		t.Fatal("symlink config accepted")
	}
	for _, value := range []string{`{"password_hash":"secret-value",`, string(data) + `{}`, `{"unexpected":"secret-value"}`, strings.Repeat("s", configSizeLimit+1)} {
		if err := os.WriteFile(file, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(file)
		if err == nil || strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), file) {
			t.Fatal("config failure must not reveal input or path")
		}
	}
}

func TestSessionExpiryReplayAndDuplicateCookie(t *testing.T) {
	s, _ := newTestServer(t)
	cookie, csrf := authenticate(t, s)
	if csrf == "" || len(cookie.Value) != 43 {
		t.Fatal("session lacks random credentials")
	}
	r := request(http.MethodGet, apiPrefix+"session", "")
	r.AddCookie(cookie)
	if _, ok := s.requestSession(r); !ok {
		t.Fatal("new session not found")
	}
	r.AddCookie(cookie)
	if _, ok := s.requestSession(r); ok {
		t.Fatal("duplicate session cookie accepted")
	}
	key := sha256.Sum256([]byte(cookie.Value))
	s.auth.mu.Lock()
	value := s.auth.sessions[key]
	value.ExpiresAt = time.Now().Add(-time.Second)
	s.auth.sessions[key] = value
	s.auth.mu.Unlock()
	r = request(http.MethodGet, apiPrefix+"session", "")
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("expired session was replayable")
	}
	s.auth.mu.Lock()
	_, retained := s.auth.sessions[key]
	s.auth.mu.Unlock()
	if retained {
		t.Fatal("expired session was not removed")
	}
}

func TestLoginLimitsIgnoreForwardedHeadersAndExpire(t *testing.T) {
	s, _ := newTestServer(t)
	r := request(http.MethodPost, apiPrefix+"login", "{}")
	for i := 0; i < loginPerPeer; i++ {
		if !s.allowLogin(r) {
			t.Fatal("login window filled too early")
		}
	}
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("X-Forwarded-For", "203.0.113.19")
	if s.allowLogin(r) {
		t.Fatal("source port or forwarded header bypassed login limit")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatal("rate limit response missing")
	}
	s.auth.mu.Lock()
	s.auth.peers["127.0.0.1"] = loginAttempts{Count: 5, Until: time.Now().Add(-time.Second)}
	s.auth.global = loginAttempts{Count: loginGlobal, Until: time.Now().Add(-time.Second)}
	s.auth.mu.Unlock()
	if !s.allowLogin(r) {
		t.Fatal("expired rate limit did not reset")
	}
	s.auth.mu.Lock()
	s.auth.global = loginAttempts{Count: loginGlobal, Until: time.Now().Add(time.Minute)}
	s.auth.mu.Unlock()
	r.RemoteAddr = "192.0.2.15:12345"
	if s.allowLogin(r) {
		t.Fatal("new peer bypassed global budget")
	}
}

func TestConcurrentSessionUseAndBoundedMemory(t *testing.T) {
	s, _ := newTestServer(t)
	var workers sync.WaitGroup
	for i := 0; i < maxSessions+10; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			w := httptest.NewRecorder()
			r := request(http.MethodPost, apiPrefix+"login", "{}")
			if _, err := s.createSession(w, r); err != nil {
				t.Error("session generation failed")
				return
			}
			for _, cookie := range w.Result().Cookies() {
				r.AddCookie(cookie)
			}
			_, _ = s.requestSession(r)
		}()
	}
	workers.Wait()
	s.auth.mu.Lock()
	count := len(s.auth.sessions)
	s.auth.mu.Unlock()
	if count != maxSessions {
		t.Fatalf("unexpected session count: %d", count)
	}
	s.Close()
	s.Close()
}
