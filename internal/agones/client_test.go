package agones

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xuhuanhello/nakama-agones/internal/provider"
)

// This fixture speaks the Kubernetes/Agones resource API (including conflict,
// resourceVersion, UID preconditions and status), not a provider-shaped mock.
type kubernetesFixture struct {
	mu                                                     sync.Mutex
	server                                                 *httptest.Server
	client                                                 *Client
	games                                                  map[string]gameServer
	secrets                                                map[string]secret
	token                                                  string
	gameCreates, secretCreates, gameDeletes, secretDeletes int
	postTimeout                                            bool
	replaceOnDelete                                        bool
	listHook                                               func(http.ResponseWriter, *http.Request) bool
}

func newFixture(t *testing.T) *kubernetesFixture {
	t.Helper()
	f := &kubernetesFixture{games: map[string]gameServer{}, secrets: map[string]secret{}, token: "fixture-service-account-token"}
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	dir := t.TempDir()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(dir, "ca"), ca, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte(f.token), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{APIURL: f.server.URL, TokenFile: filepath.Join(dir, "token"), CAFile: filepath.Join(dir, "ca"), Namespace: "agones-games", Pool: "test-pool", DeploymentID: "deployment-a", GameImage: "ghcr.io/example/game@sha256:" + strings.Repeat("a", 64), GamePort: 7770, PortName: "game", CPURequest: "500m", MemoryRequest: "256Mi", MemoryLimit: "1Gi"}
	var err error
	f.client, err = NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func output(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *kubernetesFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		output(w, 401, map[string]string{"message": "echo-private-fixture-token"})
		return
	}
	isGame := strings.HasPrefix(r.URL.Path, "/apis/agones.dev/v1/namespaces/agones-games/gameservers")
	isSecret := strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/agones-games/secrets")
	if !isGame && !isSecret {
		output(w, 404, map[string]string{})
		return
	}
	base := "/api/v1/namespaces/agones-games/secrets"
	if isGame {
		base = "/apis/agones.dev/v1/namespaces/agones-games/gameservers"
	}
	name := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, base), "/")
	switch r.Method {
	case http.MethodPost:
		if name != "" {
			output(w, 405, map[string]string{})
			return
		}
		if isSecret {
			var s secret
			if json.NewDecoder(r.Body).Decode(&s) != nil {
				output(w, 400, map[string]string{})
				return
			}
			if _, ok := f.secrets[s.Metadata.Name]; ok {
				output(w, 409, map[string]string{})
				return
			}
			f.secretCreates++
			s.Metadata.UID = fmt.Sprintf("secret-%d", f.secretCreates)
			s.Metadata.ResourceVersion = "1"
			f.secrets[s.Metadata.Name] = s
			output(w, 201, s)
		} else {
			var g gameServer
			if json.NewDecoder(r.Body).Decode(&g) != nil {
				output(w, 400, map[string]string{})
				return
			}
			if _, ok := f.games[g.Metadata.Name]; ok {
				output(w, 409, map[string]string{})
				return
			}
			f.gameCreates++
			g.Metadata.UID = fmt.Sprintf("game-%d", f.gameCreates)
			g.Metadata.ResourceVersion = "1"
			g.Status.State = "Creating"
			f.games[g.Metadata.Name] = g
			if f.postTimeout {
				f.postTimeout = false
				f.mu.Unlock()
				<-r.Context().Done()
				f.mu.Lock()
				return
			}
			output(w, 201, g)
		}
	case http.MethodGet:
		if name == "" && isGame {
			if f.listHook != nil && f.listHook(w, r) {
				return
			}
			items := []gameServer{}
			for _, g := range f.games {
				items = append(items, g)
			}
			output(w, 200, map[string]any{"metadata": map[string]string{}, "items": items})
		} else if isGame {
			if g, ok := f.games[name]; ok {
				output(w, 200, g)
			} else {
				output(w, 404, map[string]string{})
			}
		} else {
			if s, ok := f.secrets[name]; ok {
				output(w, 200, s)
			} else {
				output(w, 404, map[string]string{})
			}
		}
	case http.MethodPut:
		if !isSecret {
			output(w, 405, map[string]string{})
			return
		}
		var s secret
		if json.NewDecoder(r.Body).Decode(&s) != nil {
			output(w, 400, map[string]string{})
			return
		}
		old, ok := f.secrets[name]
		if !ok {
			output(w, 404, map[string]string{})
			return
		}
		if s.Metadata.UID != old.Metadata.UID || s.Metadata.ResourceVersion != old.Metadata.ResourceVersion {
			output(w, 409, map[string]string{})
			return
		}
		s.Metadata.ResourceVersion = "2"
		f.secrets[name] = s
		output(w, 200, s)
	case http.MethodDelete:
		var opts struct {
			Preconditions struct {
				UID string `json:"uid"`
			} `json:"preconditions"`
		}
		if json.NewDecoder(r.Body).Decode(&opts) != nil {
			output(w, 400, map[string]string{})
			return
		}
		if isGame {
			g, ok := f.games[name]
			if !ok {
				output(w, 404, map[string]string{})
				return
			}
			if f.replaceOnDelete {
				g.Metadata.UID = "replacement-game"
				f.games[name] = g
				f.replaceOnDelete = false
			}
			if opts.Preconditions.UID != g.Metadata.UID {
				output(w, 409, map[string]string{})
				return
			}
			delete(f.games, name)
			f.gameDeletes++
		} else {
			s, ok := f.secrets[name]
			if !ok {
				output(w, 404, map[string]string{})
				return
			}
			if opts.Preconditions.UID != s.Metadata.UID {
				output(w, 409, map[string]string{})
				return
			}
			delete(f.secrets, name)
			f.secretDeletes++
		}
		output(w, 200, map[string]string{"status": "Success"})
	default:
		output(w, 405, map[string]string{})
	}
}

func requestFor(n int) provider.StartRequest {
	w := fmt.Sprintf("%032x", n)
	return provider.StartRequest{Name: gameName(w), Region: "test-region", CustomData: map[string]any{"worker_id": w, "fleet_owner": "deployment-a", "build_hash": "test-build"}, EnvironmentVariables: map[string]string{"AGONES_FLEET_WORKER_ID": w, "AGONES_FLEET_BUILD_HASH": "test-build", "AGONES_FLEET_BOOTSTRAP_TOKEN": "fixture-bootstrap-private", "AGONES_FLEET_ADMISSION_KEY": "fixture-admission-private", "GAME_RESULTS_TOKEN": "fixture-result-private"}}
}

func (f *kubernetesFixture) start(t *testing.T, n int) provider.Instance {
	t.Helper()
	i, err := f.client.Start(context.Background(), requestFor(n))
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func TestStartIdempotenceSecretsAndOwnedGarbageCollection(t *testing.T) {
	f := newFixture(t)
	i := f.start(t, 1)
	if i.TTL != 0 || i.Status != "launching" {
		t.Fatal("Agones startup is not ready and has no provider TTL")
	}
	again := f.start(t, 1)
	if i.InstanceID != again.InstanceID || f.gameCreates != 1 || f.secretCreates != 1 {
		t.Fatal("retry created duplicate resources")
	}
	f.mu.Lock()
	g := f.games[requestFor(1).Name]
	s := f.secrets[secretName(g.Metadata.Name)]
	f.mu.Unlock()
	encoded, _ := json.Marshal(g)
	for _, secret := range []string{"fixture-bootstrap-private", "fixture-admission-private", "fixture-result-private"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("GameServer exposed secret values")
		}
	}
	if len(s.Metadata.OwnerReferences) != 1 || s.Metadata.OwnerReferences[0].UID != g.Metadata.UID || !s.Immutable {
		t.Fatal("credentials are not owned and immutable")
	}
	for _, e := range g.Spec.Template.Spec.Containers[0].Env {
		if e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef.Name != s.Metadata.Name {
			t.Fatal("environment did not use secretKeyRef")
		}
	}
	if g.Spec.Ports[0].PortPolicy != "Dynamic" || g.Spec.Ports[0].ContainerPort != 7770 || g.Spec.Ports[0].Protocol != "UDP" {
		t.Fatal("incorrect Agones UDP contract")
	}
	if err := f.client.Stop(context.Background(), i.InstanceID); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Stop(context.Background(), i.InstanceID); err != nil {
		t.Fatal(err)
	}
	if f.gameDeletes != 1 || f.secretDeletes != 1 {
		t.Fatal("confirmed deletion did not clean owned credentials")
	}
}

func TestAdoptionAcceptsCanonicalKubernetesQuantities(t *testing.T) {
	f := newFixture(t)
	f.client.cfg.CPURequest = "0.5"
	f.client.cfg.MemoryRequest = "1024Mi"
	f.start(t, 1)
	f.mu.Lock()
	g := f.games[requestFor(1).Name]
	g.Spec.Template.Spec.Containers[0].Resources.Requests["cpu"] = "500m"
	g.Spec.Template.Spec.Containers[0].Resources.Requests["memory"] = "1Gi"
	f.games[g.Metadata.Name] = g
	f.mu.Unlock()
	f.start(t, 1)
	if f.gameCreates != 1 {
		t.Fatal("canonical quantity normalization created another server")
	}
	if sameQuantities(map[string]string{"cpu": "500m"}, map[string]string{"cpu": "1"}) {
		t.Fatal("different resource constraints compared equal")
	}
}

func TestAgonesReadinessAndPublicPortMapping(t *testing.T) {
	f := newFixture(t)
	i := f.start(t, 1)
	for _, tc := range []struct{ state, want string }{{"Creating", "launching"}, {"Scheduled", "launching"}, {"Ready", "launching"}, {"Allocated", "running"}, {"Unhealthy", "unhealthy"}, {"Error", "unhealthy"}, {"Shutdown", "unhealthy"}} {
		t.Run(tc.state, func(t *testing.T) {
			f.mu.Lock()
			g := f.games[requestFor(1).Name]
			g.Status.State = tc.state
			g.Status.Address = "203.0.113.10"
			g.Status.Ports = []struct {
				Name string `json:"name"`
				Port int    `json:"port"`
			}{{"game", 31234}}
			f.games[g.Metadata.Name] = g
			f.mu.Unlock()
			got, err := f.client.Get(context.Background(), i.InstanceID)
			if err != nil || got.Status != tc.want {
				t.Fatalf("state mapping failed: %v", err)
			}
			if tc.want == "running" && (len(got.NetworkPorts) != 1 || got.NetworkPorts[0].Host != "203.0.113.10" || got.NetworkPorts[0].ExternalPort != 31234) {
				t.Fatal("not using Agones status endpoint")
			}
			if tc.want != "running" && len(got.NetworkPorts) != 0 {
				t.Fatal("unready server exposed an endpoint")
			}
		})
	}
}

func TestTimedOutCreateReconcilesSameIdentity(t *testing.T) {
	f := newFixture(t)
	f.postTimeout = true
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := f.client.Start(ctx, requestFor(1))
	if !provider.IsOutcomeUnknown(err) {
		t.Fatalf("create outcome must remain uncertain: %v", err)
	}
	i := f.start(t, 1)
	if i.InstanceID == "" || f.gameCreates != 1 || f.secretCreates != 1 {
		t.Fatal("recovery duplicated a timed-out creation")
	}
}

func TestCreateConflictDoesNotAdoptDifferentConfig(t *testing.T) {
	for _, part := range []string{"image", "secret-data", "owner", "secret-owner", "hash"} {
		t.Run(part, func(t *testing.T) {
			f := newFixture(t)
			f.start(t, 1)
			f.mu.Lock()
			g := f.games[requestFor(1).Name]
			s := f.secrets[secretName(g.Metadata.Name)]
			switch part {
			case "image":
				g.Spec.Template.Spec.Containers[0].Image = "other-image"
			case "secret-data":
				s.Data["GAME_RESULTS_TOKEN"] = []byte("other-token")
			case "owner":
				g.Metadata.Labels[deploymentLabel] = "another-controller"
			case "secret-owner":
				s.Metadata.OwnerReferences[0].UID = "other-game"
			case "hash":
				g.Metadata.Annotations[profileAnnotation] = "different-profile"
			}
			f.games[g.Metadata.Name] = g
			f.secrets[s.Metadata.Name] = s
			f.mu.Unlock()
			_, err := f.client.Start(context.Background(), requestFor(1))
			if !isStatus(err, 409) || f.gameDeletes != 0 || f.secretDeletes != 0 {
				t.Fatalf("foreign conflict was adopted or deleted: %v", err)
			}
		})
	}
}

func TestDeleteUIDPreconditionProtectsReusedName(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprint(race), func(t *testing.T) {
			f := newFixture(t)
			i := f.start(t, 1)
			f.mu.Lock()
			if race {
				f.replaceOnDelete = true
			} else {
				g := f.games[requestFor(1).Name]
				g.Metadata.UID = "replacement-game"
				f.games[g.Metadata.Name] = g
			}
			f.mu.Unlock()
			err := f.client.Stop(context.Background(), i.InstanceID)
			if !isStatus(err, 409) || f.gameDeletes != 0 || f.secretDeletes != 0 {
				t.Fatal("stale UID deleted replacement resources")
			}
		})
	}
}

func TestForeignResourceIsNeitherListedNorDeleted(t *testing.T) {
	f := newFixture(t)
	i := f.start(t, 1)
	f.mu.Lock()
	g := f.games[requestFor(1).Name]
	g.Metadata.Labels[poolLabel] = "foreign-pool"
	f.games[g.Metadata.Name] = g
	f.mu.Unlock()
	all, err := f.client.List(context.Background())
	if err != nil || len(all) != 0 {
		t.Fatal("list did not filter foreign resource")
	}
	if err = f.client.Stop(context.Background(), i.InstanceID); !isStatus(err, 409) || f.gameDeletes != 0 {
		t.Fatal("foreign resource deletion was allowed")
	}
}

func TestListPaginationSelectorsAndPartialFailure(t *testing.T) {
	for _, scenario := range []string{"success", "second-page-error", "cursor-loop"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			f.start(t, 1)
			f.start(t, 2)
			f.listHook = func(w http.ResponseWriter, r *http.Request) bool {
				selector := r.URL.Query().Get("labelSelector")
				if !strings.Contains(selector, poolLabel+"=test-pool") || !strings.Contains(selector, deploymentLabel+"=") || r.URL.Query().Get("limit") != "100" {
					t.Error("list scope missing")
				}
				if r.URL.Query().Get("continue") == "" {
					output(w, 200, map[string]any{"metadata": map[string]string{"continue": "next/page+token"}, "items": []gameServer{f.games[requestFor(1).Name]}})
				} else if scenario == "second-page-error" {
					output(w, 503, map[string]string{"message": "secret-must-not-escape"})
				} else {
					if r.URL.Query().Get("continue") != "next/page+token" {
						t.Error("continue token corrupted")
					}
					cursor := ""
					if scenario == "cursor-loop" {
						cursor = "next/page+token"
					}
					output(w, 200, map[string]any{"metadata": map[string]string{"continue": cursor}, "items": []gameServer{f.games[requestFor(1).Name], f.games[requestFor(2).Name]}})
				}
				return true
			}
			got, err := f.client.List(context.Background())
			if scenario == "success" {
				if err != nil || len(got) != 2 {
					t.Fatalf("pagination failed: %v", err)
				}
			} else if err == nil || got != nil {
				t.Fatal("incomplete list returned as a valid snapshot")
			}
		})
	}
}

func TestRetryRepairsInterruptedSecretOwnerAttachment(t *testing.T) {
	f := newFixture(t)
	f.start(t, 1)
	f.mu.Lock()
	s := f.secrets[secretName(requestFor(1).Name)]
	s.Metadata.OwnerReferences = nil
	f.secrets[s.Metadata.Name] = s
	f.mu.Unlock()
	got, err := f.client.Start(context.Background(), requestFor(1))
	if err != nil || got.InstanceID == "" {
		t.Fatalf("reconciliation failed: %v", err)
	}
	f.mu.Lock()
	s = f.secrets[s.Metadata.Name]
	f.mu.Unlock()
	if len(s.Metadata.OwnerReferences) != 1 {
		t.Fatal("orphan credentials were not repaired")
	}
}

func TestTokenRotationAndSafeFailureDiagnostics(t *testing.T) {
	f := newFixture(t)
	i := f.start(t, 1)
	f.mu.Lock()
	f.token = "rotated-private-fixture-token"
	f.mu.Unlock()
	if err := os.WriteFile(f.client.cfg.TokenFile, []byte("rotated-private-fixture-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Get(context.Background(), i.InstanceID); err != nil {
		t.Fatal("projected token rotation was not read")
	}
	if err := os.WriteFile(f.client.cfg.TokenFile, []byte("wrong-private-fixture-token"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := f.client.Get(context.Background(), i.InstanceID)
	if !isStatus(err, 401) || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "https://") {
		t.Fatalf("unsafe diagnostics: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = f.client.Get(ctx, i.InstanceID)
	if !errors.Is(err, context.Canceled) || provider.IsRetryable(err) {
		t.Fatal("caller cancellation must not be a retryable transport failure")
	}
}

func TestTLSVerificationAndRedirectRefusal(t *testing.T) {
	f := newFixture(t)
	privateKey, keyErr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, certErr := x509.CreateCertificate(rand.Reader, certificate, certificate, &privateKey.PublicKey, privateKey)
	if certErr != nil {
		t.Fatal(certErr)
	}
	cfg := f.client.cfg
	cfg.CAFile = filepath.Join(t.TempDir(), "wrong-ca")
	if err := os.WriteFile(cfg.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	untrusted, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = untrusted.List(context.Background()); err == nil {
		t.Fatal("untrusted TLS certificate was accepted")
	}
	redirected := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected++; output(w, 200, map[string]any{}) }))
	defer target.Close()
	f.listHook = func(w http.ResponseWriter, r *http.Request) bool {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		return true
	}
	_, err = f.client.List(context.Background())
	if !isStatus(err, 307) || redirected != 0 {
		t.Fatal("redirect leaked bearer credentials")
	}
}

func TestConfigRejectsInsecureOrAmbiguousProductionSettings(t *testing.T) {
	f := newFixture(t)
	for _, change := range []func(*Config){
		func(c *Config) { c.APIURL = "http://127.0.0.1:8001" },
		func(c *Config) { c.APIURL = "https://token@example.invalid" },
		func(c *Config) { c.GameImage = "game:latest" },
		func(c *Config) { c.Namespace = "../foreign" },
		func(c *Config) { c.Pool = "pool,other" },
		func(c *Config) { c.CAFile = "" },
		func(c *Config) { c.TokenFile = "" },
		func(c *Config) { c.GamePort = 70000 },
		func(c *Config) { c.NodeSelector = map[string]string{"kubernetes.io/os": "windows"} },
	} {
		cfg := f.client.cfg
		change(&cfg)
		if cfg.Validate() == nil {
			t.Fatal("unsafe configuration accepted")
		}
	}
	cfg := f.client.cfg
	cfg.Local = true
	cfg.APIURL = "http://127.0.0.1:8001"
	cfg.CAFile = ""
	cfg.GameImage = "fixture:local"
	if cfg.Validate() != nil {
		t.Fatal("explicit local loopback fixture rejected")
	}
	cfg.APIURL = "http://public.example.invalid"
	if cfg.Validate() == nil {
		t.Fatal("local mode allowed public cleartext credential transport")
	}
}

func TestMalformedAndRawErrorResponsesRemainSafe(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{{200, `{"metadata":{}}`}, {200, `{"items": "private-secret"}`}, {403, `{"message":"token=private-secret"}`}} {
		t.Run(fmt.Sprint(tc.status)+tc.body, func(t *testing.T) {
			f := newFixture(t)
			f.listHook = func(w http.ResponseWriter, r *http.Request) bool {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
				return true
			}
			result, err := f.client.List(context.Background())
			if err == nil || result != nil || strings.Contains(err.Error(), "private-secret") {
				t.Fatal("malformed API response was accepted or leaked")
			}
		})
	}
}
