package console

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Backend exposes only the reviewed console operations, never a proxy target.
type Backend interface {
	Snapshot(context.Context) (any, error)
	Logs(context.Context, url.Values) (any, error)
	Drain(context.Context, string) error
	RetryCreation(context.Context) error
}

const (
	adminPrefix    = "/fleet-admin/"
	apiPrefix      = adminPrefix + "api/"
	requestLimit   = 8 << 10
	queryLimit     = 2 << 10
	backendTimeout = 10 * time.Second
)

type Server struct {
	cfg           Config
	backend       Backend
	files         fs.FS
	origin        string
	host          string
	secureCookies bool
	auth          authState
	loginSlots    chan struct{}
	stop          chan struct{}
	done          chan struct{}
	closeOnce     sync.Once
}

// NewServer mounts the entire fixed /fleet-admin/ surface. Close releases its
// expiration goroutine; the caller owns the listener and HTTP server deadlines.
func NewServer(cfg Config, backend Backend, files fs.FS) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if backend == nil || files == nil {
		return nil, errors.New("console backend and assets are required")
	}
	info, err := fs.Stat(files, "index.html")
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("console index asset is missing")
	}
	u, _ := publicEndpoint(cfg.PublicURL)
	s := &Server{cfg: cfg, backend: backend, files: files,
		origin: u.Scheme + "://" + u.Host, host: u.Host, secureCookies: u.Scheme == "https",
		auth:       authState{sessions: make(map[[32]byte]session), peers: make(map[string]loginAttempts)},
		loginSlots: make(chan struct{}, 2), stop: make(chan struct{}), done: make(chan struct{})}
	go s.expireSessions()
	return s, nil
}

func (s *Server) Close() {
	s.closeOnce.Do(func() { close(s.stop) })
	<-s.done
}

func (s *Server) expireSessions() {
	defer close(s.done)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-ticker.C:
			s.auth.mu.Lock()
			s.pruneLocked(now)
			s.auth.mu.Unlock()
		}
	}
}

func securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	if r.Host != s.host {
		writeError(w, http.StatusMisdirectedRequest, "invalid_host")
		return
	}
	if len(r.URL.RawQuery) > queryLimit {
		writeError(w, http.StatusRequestURITooLong, "query_too_large")
		return
	}
	// No ServeMux cleaning redirects or encoded aliases for authenticated routes.
	if r.URL.RawPath != "" || r.URL.EscapedPath() != r.URL.Path || strings.Contains(r.URL.Path, "//") || strings.Contains(r.URL.Path, "\\") {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if r.URL.Path == "/fleet-admin" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w, "GET, HEAD")
			return
		}
		http.Redirect(w, r, adminPrefix, http.StatusPermanentRedirect)
		return
	}
	if strings.HasPrefix(r.URL.Path, apiPrefix+"v1/") {
		s.versionedAPI(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, apiPrefix) {
		s.api(w, r)
		return
	}
	s.static(w, r)
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	if len(r.Header.Values("Authorization")) != 0 {
		writeError(w, http.StatusForbidden, "api_token_scope")
		return
	}
	if hasCredentialQuery(r.URL.Query()) {
		writeError(w, 400, "credential_query_forbidden")
		return
	}
	var wantMethod string
	switch r.URL.Path {
	case apiPrefix + "session", apiPrefix + "snapshot", apiPrefix + "logs":
		wantMethod = http.MethodGet
	case apiPrefix + "login", apiPrefix + "logout", apiPrefix + "drain", apiPrefix + "retry-creation":
		wantMethod = http.MethodPost
	default:
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if r.Method != wantMethod {
		methodNotAllowed(w, wantMethod)
		return
	}
	if wantMethod == http.MethodPost && !s.validOrigin(r) {
		writeError(w, http.StatusForbidden, "invalid_origin")
		return
	}
	if r.URL.Path == apiPrefix+"login" {
		s.login(w, r)
		return
	}
	current, ok := s.requestSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	if wantMethod == http.MethodPost && !validCSRF(r, current.CSRF) {
		writeError(w, http.StatusForbidden, "invalid_csrf")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
	defer cancel()
	switch r.URL.Path {
	case apiPrefix + "session":
		s.writeSession(w, current)
	case apiPrefix + "logout":
		var input struct{}
		if !readJSON(w, r, &input) {
			return
		}
		s.deleteSession(w, r)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case apiPrefix + "snapshot":
		value, err := s.backend.Snapshot(ctx)
		writeBackend(w, value, err)
	case apiPrefix + "logs":
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(query) > 12 {
			writeError(w, http.StatusBadRequest, "invalid_query")
			return
		}
		for key, values := range query {
			if len(key) > 64 || len(values) != 1 || len(values[0]) > 1024 {
				writeError(w, http.StatusBadRequest, "invalid_query")
				return
			}
		}
		value, err := s.backend.Logs(ctx, query)
		writeBackend(w, value, err)
	case apiPrefix + "drain":
		var input struct {
			WorkerID string `json:"worker_id"`
		}
		if !readJSON(w, r, &input) {
			return
		}
		if !validWorkerID(input.WorkerID) {
			writeError(w, http.StatusBadRequest, "invalid_worker_id")
			return
		}
		err := s.backend.Drain(ctx, input.WorkerID)
		writeBackend(w, map[string]bool{"ok": true}, err)
	case apiPrefix + "retry-creation":
		var input struct{}
		if !readJSON(w, r, &input) {
			return
		}
		err := s.backend.RetryCreation(ctx)
		writeBackend(w, map[string]bool{"ok": true}, err)
	}
}

func (s *Server) validOrigin(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	return len(origins) == 1 && origins[0] == s.origin && r.Header.Get("Sec-Fetch-Site") != "cross-site"
}

func validCSRF(r *http.Request, expected string) bool {
	values := r.Header.Values("X-CSRF-Token")
	return len(values) == 1 && len(values[0]) == len(expected) && subtle.ConstantTimeCompare([]byte(values[0]), []byte(expected)) == 1
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.allowLogin(r) {
		w.Header().Set("Retry-After", "300")
		writeError(w, http.StatusTooManyRequests, "login_rate_limited")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if len(input.Username) > 64 || len(input.Password) > maxPasswordBytes {
		writeError(w, http.StatusBadRequest, "invalid_credentials_format")
		return
	}
	select {
	case s.loginSlots <- struct{}{}:
		defer func() { <-s.loginSlots }()
	default:
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "login_busy")
		return
	}
	// Always perform password verification for a well-formed candidate, including
	// an unknown username, to avoid a separate username-existence signal.
	passwordOK := VerifyPassword(input.Password, s.cfg.PasswordHash)
	usernameOK := subtle.ConstantTimeCompare([]byte(input.Username), []byte(s.cfg.Username)) == 1
	if !passwordOK || !usernameOK {
		writeError(w, http.StatusUnauthorized, "invalid_credentials")
		return
	}
	current, err := s.createSession(w, r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_unavailable")
		return
	}
	s.writeSession(w, current)
}

func (s *Server) writeSession(w http.ResponseWriter, value session) {
	writeJSON(w, http.StatusOK, map[string]any{"username": s.cfg.Username, "csrf_token": value.CSRF, "expires_at": value.ExpiresAt.UTC().Format(time.RFC3339)})
}

func readJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "json_required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, requestLimit)
	defer r.Body.Close()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_json")
		}
		return false
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' || !utf8.Valid(data) {
		writeError(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	return true
}

func validWorkerID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range []byte(value) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func writeBackend(w http.ResponseWriter, value any, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, value)
		return
	}
	var safe interface {
		HTTPStatus() int
		Error() string
	}
	if errors.As(err, &safe) && safe.HTTPStatus() >= 400 && safe.HTTPStatus() <= 599 && safeErrorCode(safe.Error()) {
		writeError(w, safe.HTTPStatus(), safe.Error())
		return
	}
	writeError(w, http.StatusBadGateway, "upstream_unavailable")
}

func safeErrorCode(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, c := range []byte(value) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		status, data = http.StatusInternalServerError, []byte(`{"error":"internal_error"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, adminPrefix) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET, HEAD")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, adminPrefix)
	if name == "" {
		name = "index.html"
	}
	if !fs.ValidPath(name) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	for _, segment := range strings.Split(name, "/") {
		if strings.HasPrefix(segment, ".") {
			writeError(w, http.StatusNotFound, "not_found")
			return
		}
	}
	info, err := fs.Stat(s.files, name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	data, err := fs.ReadFile(s.files, name)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	contentType := mime.TypeByExtension(path.Ext(name))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}
