package console

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const enrollmentNamespace = "fleet-enrollment-requests"
const enrollmentAPIVersion = "infrastructure.nakama-agones.io/v1alpha1"
const enrollmentAPIPath = "/apis/" + enrollmentAPIVersion + "/namespaces/" + enrollmentNamespace
const enrollmentSecretPath = "/api/v1/namespaces/" + enrollmentNamespace + "/secrets"
const enrollmentSubject = "system:serviceaccount:fleet-enrollment-system:fleet-enrollment-submitter"

var enrollmentLabel = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)

var enrollmentID = regexp.MustCompile(`^[a-f0-9]{32}$`)
var enrollmentUID = regexp.MustCompile(`^[a-zA-Z0-9-]{1,128}$`)
var enrollmentVersion = regexp.MustCompile(`^[0-9]{1,32}$`)
var enrollmentDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var enrollmentFingerprint = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)

// One explicit region/cluster binding. This token is separate from read-only observation.
type NodeOnboardingConfig struct {
	Region      string `json:"region"`
	ClusterID   string `json:"cluster_id"`
	APIURL      string `json:"api_url"`
	CAFile      string `json:"ca_file"`
	TokenFile   string `json:"token_file"`
	GamePortMin int    `json:"game_port_min"`
	GamePortMax int    `json:"game_port_max"`
}

func (c NodeOnboardingConfig) validate(source SourceConfig) error {
	u, e := url.Parse(c.APIURL)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" || u.Path != "" || strings.ContainsAny(u.Host, "\\% \t\r\n") || u.Host != strings.ToLower(u.Host) || strings.HasSuffix(u.Hostname(), ".") {
		return errors.New("node onboarding requires a canonical HTTPS Kubernetes API URL")
	}
	if u.Port() != "" {
		p, e := strconv.Atoi(u.Port())
		if e != nil || p < 1 || p > 65535 || strconv.Itoa(p) != u.Port() {
			return errors.New("invalid node onboarding API port")
		}
	}
	if !enrollmentLabel.MatchString(c.Region) || !enrollmentLabel.MatchString(c.ClusterID) || strings.Contains(c.Region, "..") || strings.Contains(c.ClusterID, "..") || !filepath.IsAbs(c.TokenFile) || !filepath.IsAbs(c.CAFile) || filepath.Clean(c.TokenFile) != c.TokenFile || filepath.Clean(c.CAFile) != c.CAFile || c.GamePortMin < 1024 || c.GamePortMax < c.GamePortMin || c.GamePortMax > 65535 {
		return errors.New("invalid node onboarding configuration")
	}
	match := false
	for _, r := range source.Regions {
		if r.Name == c.Region && strings.TrimRight(r.APIURL, "/") == c.APIURL && r.CAFile == c.CAFile {
			match = true
		}
		if sameCredentialFile(c.TokenFile, r.TokenFile) {
			return errors.New("node onboarding must not reuse observer credentials")
		}
	}
	if sameCredentialFile(c.TokenFile, source.FleetTokenFile) {
		return errors.New("node onboarding must not reuse Fleet credentials")
	}
	if !match {
		return errors.New("node onboarding must bind one configured region and its pinned cluster CA")
	}
	return nil
}
func sameCredentialFile(a, b string) bool {
	if b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	x, e1 := os.Stat(a)
	y, e2 := os.Stat(b)
	return e1 == nil && e2 == nil && os.SameFile(x, y)
}

type enrollmentClient struct {
	cfg  NodeOnboardingConfig
	http *http.Client
}

func newEnrollmentClient(cfg NodeOnboardingConfig) (*enrollmentClient, error) {
	info, e := os.Lstat(cfg.CAFile)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > 1<<20 {
		return nil, errors.New("node onboarding CA is unavailable or writable by others")
	}
	raw, e := os.ReadFile(cfg.CAFile)
	pool := x509.NewCertPool()
	if e != nil || !pool.AppendCertsFromPEM(raw) {
		return nil, errors.New("invalid node onboarding CA")
	}
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, DisableCompression: true,
		DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 4 * time.Second, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second}
	return &enrollmentClient{cfg: cfg, http: &http.Client{Transport: transport, Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func readEnrollmentToken(path string) ([]byte, error) {
	raw, _, err := readPrivateFile(path, 32768, os.Geteuid())
	token := bytes.TrimSpace(raw)
	if err != nil || len(token) == 0 || bytes.ContainsAny(token, " \t\r\n\x00") || !validEnrollmentToken(token, time.Now()) {
		for i := range raw {
			raw[i] = 0
		}
		return nil, sourceError{503, "node_onboarding_credential_unavailable"}
	}
	return token, nil
}

// This only prevents accidental use of an observer/admin or long-lived credential.
// The pinned Kubernetes API remains responsible for signature and audience validation.
func validEnrollmentToken(token []byte, now time.Time) bool {
	parts := bytes.Split(token, []byte("."))
	if len(parts) != 3 || len(parts[0]) == 0 || len(parts[2]) == 0 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(string(parts[1]))
	if err != nil || len(raw) > 16384 {
		return false
	}
	var claims struct {
		Subject   string `json:"sub"`
		Issued    int64  `json:"iat"`
		Expires   int64  `json:"exp"`
		NotBefore int64  `json:"nbf"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return false
	}
	return claims.Subject == enrollmentSubject && claims.Issued > 0 && claims.Expires > claims.Issued && claims.Expires-claims.Issued <= 7200 && claims.Issued <= now.Unix()+60 && claims.NotBefore <= now.Unix()+60 && claims.Expires >= now.Unix()+30
}

// Only these exact resources and verbs can carry the submission credential.
func permittedEnrollmentRequest(method, path string) bool {
	if method == http.MethodPost && (path == enrollmentAPIPath+"/nodeenrollments" || path == enrollmentAPIPath+"/noderetirements" || path == enrollmentSecretPath) {
		return true
	}
	if method == http.MethodGet && (path == enrollmentAPIPath+"/nodeenrollments?limit=100" || path == enrollmentAPIPath+"/noderetirements?limit=100") {
		return true
	}
	for _, kind := range []struct{ plural, prefix string }{{"nodeenrollments", "enroll-"}, {"noderetirements", "retire-"}} {
		prefix := enrollmentAPIPath + "/" + kind.plural + "/" + kind.prefix
		if strings.HasPrefix(path, prefix) && enrollmentID.MatchString(strings.TrimPrefix(path, prefix)) {
			return method == http.MethodGet || (method == http.MethodPatch && kind.plural == "nodeenrollments")
		}
	}
	return method == http.MethodDelete && strings.HasPrefix(path, enrollmentSecretPath+"/ssh-") && enrollmentID.MatchString(strings.TrimPrefix(path, enrollmentSecretPath+"/ssh-"))
}
func (c *enrollmentClient) request(ctx context.Context, method, path string, value, out any) error {
	if !permittedEnrollmentRequest(method, path) {
		return sourceError{400, "invalid_enrollment_operation"}
	}
	token, e := readEnrollmentToken(c.cfg.TokenFile)
	if e != nil {
		return e
	}
	defer func() {
		for i := range token {
			token[i] = 0
		}
	}()
	var raw []byte
	if value != nil {
		raw, e = json.Marshal(value)
		if e != nil {
			return sourceError{400, "invalid_request"}
		}
		defer func() {
			for i := range raw {
				raw[i] = 0
			}
		}()
	}
	req, e := http.NewRequestWithContext(ctx, method, c.cfg.APIURL+path, bytes.NewReader(raw))
	if e != nil {
		return sourceError{503, "node_onboarding_unavailable"}
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	req.Header.Set("Accept", "application/json")
	if value != nil {
		req.Header.Set("Content-Type", "application/json")
		if method == http.MethodPatch {
			req.Header.Set("Content-Type", "application/json-patch+json")
		}
	}
	response, e := c.http.Do(req)
	if e != nil {
		return sourceError{503, "node_onboarding_unavailable"}
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case 404:
		return sourceError{404, "job_not_found"}
	case 409:
		return sourceError{409, "onboarding_state_changed"}
	case 401, 403:
		return sourceError{503, "node_onboarding_access_denied"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return sourceError{502, "node_onboarding_request_failed"}
	}
	data, e := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if e != nil || len(data) > 1<<20 {
		return sourceError{502, "node_onboarding_invalid_response"}
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		return sourceError{502, "node_onboarding_invalid_response"}
	}
	return nil
}
