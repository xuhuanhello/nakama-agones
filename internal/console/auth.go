package console

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	passwordIterations = 600_000
	passwordSaltSize   = 32
	passwordKeySize    = 32
	maxPasswordBytes   = 1024
	sessionLifetime    = 8 * time.Hour
	maxSessions        = 64
	loginWindow        = 5 * time.Minute
	loginPerPeer       = 5
	loginGlobal        = 20
	maxLoginPeers      = 4096
)

// HashPassword returns a versioned, salted PBKDF2-SHA256 hash. The plaintext is
// supplied on stdin by the CLI, never in its arguments or environment.
func HashPassword(password string) (string, error) {
	if len(password) < 12 || len(password) > maxPasswordBytes {
		return "", errors.New("password must contain 12 to 1024 bytes")
	}
	salt := make([]byte, passwordSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return "", errors.New("cannot generate password salt")
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, passwordKeySize)
	if err != nil {
		return "", errors.New("cannot hash password")
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", passwordIterations, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword deliberately accepts only bounded, strong hash parameters.
func VerifyPassword(password, encodedHash string) bool {
	iterations, salt, expected, ok := parsePasswordHash(encodedHash)
	if !ok || len(password) > maxPasswordBytes || len(password) < 12 {
		return false
	}
	actual, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(expected))
	return err == nil && subtle.ConstantTimeCompare(actual, expected) == 1
}

func parsePasswordHash(value string) (int, []byte, []byte, bool) {
	parts := strings.Split(value, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" || len(value) > 256 {
		return 0, nil, nil, false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < passwordIterations || iterations > 2_000_000 || strconv.Itoa(iterations) != parts[1] {
		return 0, nil, nil, false
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[2])
	if err != nil || len(salt) != passwordSaltSize {
		return 0, nil, nil, false
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[3])
	if err != nil || len(key) != passwordKeySize {
		return 0, nil, nil, false
	}
	return iterations, salt, key, true
}

type session struct {
	CSRF      string
	ExpiresAt time.Time
}

type loginAttempts struct {
	Count int
	Until time.Time
}

type authState struct {
	mu       sync.Mutex
	sessions map[[32]byte]session
	peers    map[string]loginAttempts
	global   loginAttempts
}

func (s *Server) sessionCookieName() string {
	if s.secureCookies {
		return "__Host-fleet-admin"
	}
	return "fleet-admin-session"
}

func (s *Server) cookie(value string, expires time.Time, maxAge int) *http.Cookie {
	path := "/fleet-admin/"
	if s.secureCookies {
		path = "/" // Required by the __Host- cookie prefix.
	}
	return &http.Cookie{Name: s.sessionCookieName(), Value: value, Path: path,
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteStrictMode,
		Expires: expires.UTC(), MaxAge: maxAge}
}

func (s *Server) requestSession(r *http.Request) (session, bool) {
	key, ok := s.requestSessionKey(r)
	if !ok {
		return session{}, false
	}
	s.auth.mu.Lock()
	defer s.auth.mu.Unlock()
	value, ok := s.auth.sessions[key]
	if !ok {
		return session{}, false
	}
	if !time.Now().Before(value.ExpiresAt) {
		delete(s.auth.sessions, key)
		return session{}, false
	}
	return value, true
}

func (s *Server) requestSessionKey(r *http.Request) ([32]byte, bool) {
	var token string
	count := 0
	for _, cookie := range r.Cookies() {
		if cookie.Name == s.sessionCookieName() {
			token = cookie.Value
			count++
		}
	}
	if count != 1 || len(token) != 43 {
		return [32]byte{}, false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(decoded) != 32 {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(token)), true
}

func randomToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errors.New("cannot generate session token")
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) (session, error) {
	token, err := randomToken()
	if err != nil {
		return session{}, err
	}
	csrf, err := randomToken()
	if err != nil {
		return session{}, err
	}
	now := time.Now()
	value := session{CSRF: csrf, ExpiresAt: now.Add(sessionLifetime)}
	s.auth.mu.Lock()
	s.pruneLocked(now)
	if old, ok := s.requestSessionKey(r); ok {
		delete(s.auth.sessions, old)
	}
	if len(s.auth.sessions) >= maxSessions {
		var oldestKey [32]byte
		var oldest time.Time
		for key, existing := range s.auth.sessions {
			if oldest.IsZero() || existing.ExpiresAt.Before(oldest) {
				oldestKey, oldest = key, existing.ExpiresAt
			}
		}
		delete(s.auth.sessions, oldestKey)
	}
	s.auth.sessions[sha256.Sum256([]byte(token))] = value
	s.auth.mu.Unlock()
	http.SetCookie(w, s.cookie(token, value.ExpiresAt, int(sessionLifetime/time.Second)))
	return value, nil
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	if key, ok := s.requestSessionKey(r); ok {
		s.auth.mu.Lock()
		delete(s.auth.sessions, key)
		s.auth.mu.Unlock()
	}
	http.SetCookie(w, s.cookie("", time.Unix(1, 0), -1))
}

func (s *Server) allowLogin(r *http.Request) bool {
	// Do not trust X-Forwarded-For from a direct connection. An SSH tunnel or
	// loopback proxy shares this per-peer budget; the global budget remains too.
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = "unknown"
	}
	now := time.Now()
	s.auth.mu.Lock()
	defer s.auth.mu.Unlock()
	s.pruneLocked(now)
	global := s.auth.global
	if !now.Before(global.Until) {
		global = loginAttempts{Until: now.Add(loginWindow)}
	}
	item, exists := s.auth.peers[peer]
	if !exists {
		if len(s.auth.peers) >= maxLoginPeers {
			return false
		}
		item = loginAttempts{Until: now.Add(loginWindow)}
	}
	if global.Count >= loginGlobal || item.Count >= loginPerPeer {
		return false
	}
	global.Count++
	item.Count++
	s.auth.global = global
	s.auth.peers[peer] = item
	return true
}

// auth.mu must be held. Session deadlines are absolute, not extended by reads.
func (s *Server) pruneLocked(now time.Time) {
	for key, value := range s.auth.sessions {
		if !now.Before(value.ExpiresAt) {
			delete(s.auth.sessions, key)
		}
	}
	for peer, item := range s.auth.peers {
		if !now.Before(item.Until) {
			delete(s.auth.peers, peer)
		}
	}
}
