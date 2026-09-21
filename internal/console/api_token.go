package console

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// Read API tokens contain 256 random bits. SHA256 is appropriate for these
// machine-generated secrets; administrator passwords still use salted PBKDF2.
func GenerateReadAPIToken() (token, hash string, err error) {
	var value [32]byte
	if _, err = rand.Read(value[:]); err != nil {
		return "", "", errors.New("cannot generate read API token")
	}
	token = "fcro_" + base64.RawURLEncoding.EncodeToString(value[:])
	digest := sha256.Sum256([]byte(token))
	return token, "sha256:" + hex.EncodeToString(digest[:]), nil
}

func validReadAPITokenHash(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == 32
}

func verifyReadAPIToken(token, hash string) bool {
	if !validReadAPITokenHash(hash) || len(token) != 48 || !strings.HasPrefix(token, "fcro_") {
		return false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(token, "fcro_"))
	if err != nil || len(raw) != 32 {
		return false
	}
	expected, _ := hex.DecodeString(strings.TrimPrefix(hash, "sha256:"))
	actual := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(actual[:], expected) == 1
}

func hasCredentialQuery(q url.Values) bool {
	for key := range q {
		switch strings.ToLower(key) {
		case "token", "api_token", "access_token", "read_api_token", "api_key", "authorization":
			return true
		}
	}
	return false
}

// authenticateReadAPI does not create a browser session or grant write access.
// An explicitly supplied invalid Bearer token never falls back to a cookie.
func (s *Server) authenticateReadAPI(w http.ResponseWriter, r *http.Request) (string, bool) {
	if values := r.Header.Values("Authorization"); len(values) > 0 {
		if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") || !verifyReadAPIToken(strings.TrimPrefix(values[0], "Bearer "), s.cfg.ReadAPITokenHash) {
			writeError(w, 401, "invalid_api_token")
			return "", false
		}
		return "api_token", true
	}
	if _, ok := s.requestSession(r); !ok {
		writeError(w, 401, "unauthenticated")
		return "", false
	}
	return "session", true
}
