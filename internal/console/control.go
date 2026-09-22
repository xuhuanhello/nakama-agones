package console

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	controlHost        = "fleet-console-control"
	controlPrefix      = "/v1/"
	controlTimeout     = 4 * time.Second
	controlMaxResponse = 8 << 20
)

// ControlConfig belongs to the root broker, never to the unprivileged console.
// CredentialsFile refers to the existing root-only JSON containing admin_token.
type ControlConfig struct {
	SocketPath      string `json:"socket_path"`
	FleetURL        string `json:"fleet_url"`
	CredentialsFile string `json:"credentials_file"`
}

func LoadControlConfig(path string) (ControlConfig, error) {
	var cfg ControlConfig
	data, _, err := readPrivateFile(path, configSizeLimit, 0)
	if err != nil {
		return cfg, errors.New("invalid private control configuration")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cfg) != nil || decoder.Decode(new(any)) != io.EOF {
		return cfg, errors.New("invalid control configuration JSON")
	}
	return cfg, cfg.validate()
}

func (cfg ControlConfig) validate() error {
	if !validSocketPath(cfg.SocketPath) || !filepath.IsAbs(cfg.CredentialsFile) || filepath.Clean(cfg.CredentialsFile) != cfg.CredentialsFile {
		return errors.New("control paths must be absolute and canonical")
	}
	u, err := url.Parse(cfg.FleetURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("control upstream must be a loopback HTTP or HTTPS origin")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() || strings.HasSuffix(u.Host, ":") {
		return errors.New("control upstream requires a literal loopback address")
	}
	if u.Port() != "" {
		number, err := strconv.Atoi(u.Port())
		if err != nil || number < 1 || number > 65535 {
			return errors.New("invalid control upstream port")
		}
	}
	return nil
}

func validSocketPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && len(path) < 100 && !strings.ContainsAny(path, "\x00\r\n")
}

type ControlCapabilities struct {
	APIVersion string   `json:"api_version"`
	CanManage  bool     `json:"can_manage"`
	Actions    []string `json:"actions"`
}

type ControlServer struct {
	cfg        ControlConfig
	client     *http.Client
	credential func() (string, error)
	slots      chan struct{}
}

func NewControlServer(cfg ControlConfig) (*ControlServer, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	transport := &http.Transport{Proxy: nil, DisableCompression: true, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, MaxIdleConns: 4, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second}
	s := &ControlServer{cfg: cfg, slots: make(chan struct{}, 2), client: &http.Client{
		Transport: transport, Timeout: controlTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	s.credential = func() (string, error) { return readControlCredential(cfg.CredentialsFile, 0) }
	return s, nil
}

func readControlCredential(path string, owner int) (string, error) {
	raw, _, err := readPrivateFile(path, 1<<20, owner)
	if err != nil {
		return "", sourceError{502, "control_credential_unavailable"}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return "", sourceError{502, "control_credential_unavailable"}
	}
	seen := map[string]bool{}
	var token string
	for decoder.More() {
		name, err := decoder.Token()
		key, ok := name.(string)
		if err != nil || !ok || seen[key] {
			return "", sourceError{502, "control_credential_unavailable"}
		}
		seen[key] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return "", sourceError{502, "control_credential_unavailable"}
		}
		if key == "admin_token" && json.Unmarshal(value, &token) != nil {
			return "", sourceError{502, "control_credential_unavailable"}
		}
	}
	last, err := decoder.Token()
	if err != nil || last != json.Delim('}') || decoder.Decode(new(any)) != io.EOF || len(token) < 1 || len(token) > 32768 {
		return "", sourceError{502, "control_credential_unavailable"}
	}
	for _, c := range []byte(token) {
		if c <= 32 || c >= 127 {
			return "", sourceError{502, "control_credential_unavailable"}
		}
	}
	return token, nil
}

// ServeHTTP is mounted only on the protected Unix listener. Caller-controlled
// paths, URLs, methods and headers are never forwarded upstream.
func (s *ControlServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	if r.Host != controlHost || r.URL.RawQuery != "" || r.URL.RawPath != "" || r.URL.EscapedPath() != r.URL.Path {
		writeError(w, 404, "not_found")
		return
	}
	want := http.MethodPost
	switch r.URL.Path {
	case controlPrefix + "policy":
		if r.Method == http.MethodGet {
			want = http.MethodGet
		}
	case controlPrefix + "capabilities":
		want = http.MethodGet
	case controlPrefix + "drain", controlPrefix + "retry-creation":
	default:
		writeError(w, 404, "not_found")
		return
	}
	if r.Method != want {
		methodNotAllowed(w, want)
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		writeError(w, 429, "control_busy")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), controlTimeout)
	defer cancel()
	if r.URL.Path == controlPrefix+"policy" {
		s.policy(w, r.WithContext(ctx))
		return
	}
	if want == http.MethodGet {
		if r.ContentLength != 0 {
			writeError(w, 400, "invalid_request")
			return
		}
		data, err := s.upstream(ctx, http.MethodGet, "/agones/fleet/v1/admin/status", nil, controlMaxResponse)
		if err != nil {
			writeBackend(w, nil, err)
			return
		}
		var status struct {
			Revision    *uint64         `json:"revision"`
			Workers     json.RawMessage `json:"workers"`
			Allocations json.RawMessage `json:"allocations"`
		}
		if json.Unmarshal(data, &status) != nil || status.Revision == nil || len(status.Workers) == 0 || len(status.Allocations) == 0 {
			writeError(w, 502, "control_invalid_response")
			return
		}
		writeJSON(w, 200, ControlCapabilities{APIVersion: "v1", CanManage: true, Actions: []string{"drain", "retry-creation", "policy"}})
		return
	}
	var payload []byte
	if r.URL.Path == controlPrefix+"drain" {
		var input struct {
			WorkerID string `json:"worker_id"`
		}
		if !readJSON(w, r, &input) {
			return
		}
		if !workerName.MatchString(input.WorkerID) {
			writeError(w, 400, "invalid_worker_id")
			return
		}
		payload, _ = json.Marshal(input)
	} else {
		var input struct{}
		if !readJSON(w, r, &input) {
			return
		}
		payload = []byte("{}")
	}
	// These two lifecycle operations complement the typed policy route above.
	upstreamPath := "/agones/fleet/v1/admin/retry-creation"
	if r.URL.Path == controlPrefix+"drain" {
		upstreamPath = "/agones/fleet/v1/admin/drain"
	}
	data, err := s.upstream(ctx, http.MethodPost, upstreamPath, payload, 64<<10)
	if err != nil {
		writeBackend(w, nil, err)
		return
	}
	var result struct {
		Accepted bool `json:"accepted"`
	}
	if json.Unmarshal(data, &result) != nil || !result.Accepted {
		writeError(w, 502, "control_invalid_response")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (s *ControlServer) upstream(ctx context.Context, method, path string, body []byte, limit int64) ([]byte, error) {
	token, err := s.credential()
	if err != nil {
		return nil, sourceError{502, "control_credential_unavailable"}
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.cfg.FleetURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, sourceError{502, "control_unavailable"}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := s.client.Do(request)
	if err != nil {
		return nil, sourceError{502, "control_unavailable"}
	}
	defer response.Body.Close()
	if response.StatusCode == 401 || response.StatusCode == 403 {
		return nil, sourceError{502, "control_access_denied"}
	}
	if method == http.MethodPost && strings.HasSuffix(path, "/drain") {
		if response.StatusCode == 404 {
			return nil, sourceError{404, "worker_not_found"}
		}
		if response.StatusCode == 409 {
			return nil, sourceError{409, "worker_not_drainable"}
		}
	}
	if strings.HasSuffix(path, "/policy") {
		switch response.StatusCode {
		case 400:
			return nil, sourceError{400, "policy_invalid"}
		case 409:
			return nil, sourceError{409, "policy_conflict"}
		case 404:
			return nil, sourceError{503, "policy_unavailable"}
		}
	}
	expected := http.StatusOK
	if method == http.MethodPost && !strings.HasSuffix(path, "/policy") {
		expected = http.StatusAccepted
	}
	if response.StatusCode != expected {
		return nil, sourceError{502, "control_unavailable"}
	}
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return nil, sourceError{502, "control_invalid_response"}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, sourceError{502, "control_invalid_response"}
	}
	return data, nil
}

type controlClient struct {
	path   string
	owner  int
	client *http.Client
}

func newControlClient(path string) (*controlClient, error) {
	if !validSocketPath(path) {
		return nil, errors.New("invalid control socket path")
	}
	c := &controlClient{path: path, owner: 0}
	transport := &http.Transport{Proxy: nil, MaxIdleConns: 2, MaxIdleConnsPerHost: 2, IdleConnTimeout: 15 * time.Second,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if err := verifyControlSocket(c.path, c.owner); err != nil {
				return nil, err
			}
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", c.path)
		}}
	c.client = &http.Client{Transport: transport, Timeout: controlTimeout + time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return c, nil
}

func (c *controlClient) request(ctx context.Context, method, path string, value any) ([]byte, error) {
	var body []byte
	if value != nil {
		body, _ = json.Marshal(value)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+controlHost+controlPrefix+path, bytes.NewReader(body))
	if err != nil {
		return nil, sourceError{502, "control_unavailable"}
	}
	if value != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.client.Do(req)
	if err != nil {
		return nil, sourceError{502, "control_unavailable"}
	}
	defer res.Body.Close()
	limit := int64(16 << 10)
	if path == "policy" {
		limit = 1 << 20
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, sourceError{502, "control_invalid_response"}
	}
	if (method == http.MethodGet && res.StatusCode == 200) || (method == http.MethodPost && (res.StatusCode == 202 || path == "policy" && res.StatusCode == 200)) {
		return raw, nil
	}
	var failure struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &failure) == nil {
		for _, allowed := range []string{"control_unavailable", "control_access_denied", "control_credential_unavailable", "control_invalid_response", "control_busy", "worker_not_found", "worker_not_drainable", "policy_invalid", "policy_conflict", "policy_unavailable"} {
			if failure.Error == allowed && res.StatusCode >= 400 && res.StatusCode <= 599 {
				return nil, sourceError{res.StatusCode, allowed}
			}
		}
	}
	return nil, sourceError{502, "control_invalid_response"}
}

func (c *controlClient) capabilities(ctx context.Context) error {
	raw, err := c.request(ctx, http.MethodGet, "capabilities", nil)
	if err != nil {
		return err
	}
	var caps ControlCapabilities
	if json.Unmarshal(raw, &caps) != nil || caps.APIVersion != "v1" || !caps.CanManage || (len(caps.Actions) != 2 && len(caps.Actions) != 3) || caps.Actions[0] != "drain" || caps.Actions[1] != "retry-creation" || len(caps.Actions) == 3 && caps.Actions[2] != "policy" {
		return sourceError{502, "control_invalid_response"}
	}
	return nil
}

func (c *controlClient) action(ctx context.Context, action string, value any) error {
	if action != "drain" && action != "retry-creation" {
		return sourceError{400, "invalid_action"}
	}
	raw, err := c.request(ctx, http.MethodPost, action, value)
	if err != nil {
		return err
	}
	var result struct {
		Accepted bool `json:"accepted"`
	}
	if json.Unmarshal(raw, &result) != nil || !result.Accepted {
		return sourceError{502, "control_invalid_response"}
	}
	return nil
}

// Keep the OS-level socket implementation separate from the HTTP allowlist.
func ListenControlSocket(cfg ControlConfig) (*net.UnixListener, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("control broker must run as root")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return listenControlSocket(cfg.SocketPath, 0)
}
