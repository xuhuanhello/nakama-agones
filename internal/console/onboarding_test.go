package console

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNodeOnboardingRequiresSessionOriginAndCSRF(t *testing.T) {
	s, _ := newTestServer(t)
	if got := perform(s, request("GET", apiPrefix+"node-onboarding", "")).Code; got != 401 {
		t.Fatalf("unauthenticated %d", got)
	}
	cookie, csrf := authenticate(t, s)
	cases := []struct {
		name, method, route, origin, token, auth string
		want                                     int
	}{
		{"disabled", "GET", "", "", "", "", 200},
		{"missing csrf", "POST", "/scan", "http://127.0.0.1:17365", "", "", 403},
		{"wrong origin", "POST", "/scan", "https://untrusted.invalid", csrf, "", 403},
		{"api read token", "POST", "/scan", "http://127.0.0.1:17365", csrf, "Bearer read-only", 403},
		{"disabled join", "POST", "/join", "http://127.0.0.1:17365", csrf, "", 503},
		{"no arbitrary command", "POST", "/exec", "http://127.0.0.1:17365", csrf, "", 404},
		{"no arbitrary job query", "GET", "/jobs?id=a&command=whoami", "", "", "", 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := ""
			if c.method == "POST" {
				body = "{}"
			}
			r := request(c.method, apiPrefix+"node-onboarding"+c.route, body)
			r.AddCookie(cookie)
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if c.token != "" {
				r.Header.Set("X-CSRF-Token", c.token)
			}
			if c.auth != "" {
				r.Header.Set("Authorization", c.auth)
			}
			w := perform(s, r)
			if w.Code != c.want {
				t.Fatalf("got %d want %d", w.Code, c.want)
			}
		})
	}
}

type kubeEnrollmentFixture struct {
	mu                                                         sync.Mutex
	client                                                     *enrollmentClient
	records                                                    map[string]*enrollmentResource
	calls                                                      []string
	token                                                      string
	secretCalls, deleteCalls, patchCalls                       int
	secretConflict, patchConflict, patchAmbiguous, pendingScan bool
	secretUID                                                  string
	server                                                     *httptest.Server
	responseMode                                               string
	retirements                                                map[string]*retirementResource
	retirementCreates                                          int
}

func newKubeEnrollmentFixture(t *testing.T) *kubeEnrollmentFixture {
	t.Helper()
	f := &kubeEnrollmentFixture{records: map[string]*enrollmentResource{}, token: fixtureEnrollmentToken(enrollmentSubject, time.Now().Unix(), time.Now().Unix()+3600, "one"), retirements: map[string]*retirementResource{}, secretUID: "secret-fixture-uid"}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.serve(t, w, r) }))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	f.server = server
	t.Cleanup(server.Close)
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.crt")
	token := filepath.Join(dir, "token")
	if e := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(token, []byte(f.token), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := newEnrollmentClient(NodeOnboardingConfig{Region: "test", ClusterID: "test-cluster", APIURL: server.URL, CAFile: ca, TokenFile: token, GamePortMin: 20000, GamePortMax: 20999})
	if e != nil {
		t.Fatal(e)
	}
	f.client = c
	t.Cleanup(c.http.CloseIdleConnections)
	return f
}
func (f *kubeEnrollmentFixture) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		t.Error("unexpected submission credential")
		w.WriteHeader(401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch f.responseMode {
	case "redirect":
		w.Header().Set("Location", "https://untrusted.invalid/token")
		w.WriteHeader(307)
		return
	case "oversized":
		io.Copy(w, bytes.NewReader(bytes.Repeat([]byte("x"), (1<<20)+1)))
		return
	case "errorbody":
		w.WriteHeader(422)
		io.WriteString(w, "password=DO_NOT_DISCLOSE")
		return
	}
	if r.Method == "GET" && r.URL.RequestURI() == enrollmentAPIPath+"/noderetirements?limit=100" {
		items := []*retirementResource{}
		for _, item := range f.retirements {
			items = append(items, item)
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
		return
	}
	if r.Method == "POST" && r.URL.Path == enrollmentAPIPath+"/noderetirements" {
		f.retirementCreates++
		var item retirementResource
		if err := json.NewDecoder(r.Body).Decode(&item); err != nil {
			t.Error(err)
		}
		id := strings.TrimPrefix(item.Metadata.Name, "retire-")
		item.Metadata.UID = "retirement-fixture-uid"
		item.Metadata.ResourceVersion = "1"
		item.Metadata.Generation = 1
		item.Metadata.CreatedAt = time.Now().UTC().Format(time.RFC3339)
		f.retirements[id] = &item
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(item)
		return
	}
	if r.Method == "GET" && strings.HasPrefix(r.URL.Path, enrollmentAPIPath+"/noderetirements/retire-") {
		id := strings.TrimPrefix(r.URL.Path, enrollmentAPIPath+"/noderetirements/retire-")
		if f.retirements[id] == nil {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(f.retirements[id])
		return
	}
	if r.Method == "GET" && r.URL.RequestURI() == enrollmentAPIPath+"/nodeenrollments?limit=100" {
		items := []*enrollmentResource{}
		for _, item := range f.records {
			items = append(items, item)
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
		return
	}
	if r.Method == "POST" && r.URL.Path == enrollmentAPIPath+"/nodeenrollments" {
		var item enrollmentResource
		json.NewDecoder(r.Body).Decode(&item)
		id := strings.TrimPrefix(item.Metadata.Name, "enroll-")
		item.Metadata.UID = "enroll-fixture-uid"
		item.Metadata.ResourceVersion = "1"
		item.Metadata.Generation = 1
		item.Metadata.CreatedAt = time.Now().UTC().Format(time.RFC3339)
		f.complete(&item, id)
		if f.pendingScan {
			item.Status.Phase = "Scanning"
		}
		f.records[id] = &item
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(item)
		return
	}
	if r.Method == "POST" && r.URL.Path == enrollmentSecretPath {
		f.secretCalls++
		if f.secretConflict {
			w.WriteHeader(409)
			return
		}
		var secret map[string]any
		json.NewDecoder(r.Body).Decode(&secret)
		meta := secret["metadata"].(map[string]any)
		id := strings.TrimPrefix(meta["name"].(string), "ssh-")
		if secret["immutable"] != true || secret["type"] != "Opaque" || meta["namespace"] != enrollmentNamespace {
			t.Error("secret restrictions lost")
		}
		refs := meta["ownerReferences"].([]any)
		owner := refs[0].(map[string]any)
		if len(refs) != 1 || owner["uid"] != "enroll-fixture-uid" || owner["name"] != "enroll-"+id || owner["blockOwnerDeletion"] != false {
			t.Error("secret owner identity lost")
		}
		labels := meta["labels"].(map[string]any)
		if labels["nakama-agones.io/enrollment-id"] != id || len(secret["data"].(map[string]any)) != 1 {
			t.Error("unreviewed secret data")
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]string{"name": "ssh-" + id, "namespace": enrollmentNamespace, "uid": f.secretUID}, "data": map[string]string{"password": "DO_NOT_DISCLOSE"}})
		return
	}
	if r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, enrollmentSecretPath+"/ssh-") {
		f.deleteCalls++
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["preconditions"].(map[string]any)["uid"] != f.secretUID {
			t.Error("cleanup lacks returned UID precondition")
		}
		io.WriteString(w, `{}`)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, enrollmentAPIPath+"/nodeenrollments/enroll-")
	item := f.records[id]
	if item == nil {
		t.Error("unreviewed Kubernetes path: " + r.Method + " " + r.URL.Path)
		w.WriteHeader(404)
		return
	}
	if r.Method == "PATCH" {
		f.patchCalls++
		if f.patchConflict {
			w.WriteHeader(409)
			return
		}
		if f.patchAmbiguous {
			w.WriteHeader(500)
			return
		}
		if r.Header.Get("Content-Type") != "application/json-patch+json" {
			t.Error("missing CAS JSON patch media type")
		}
		var patch []map[string]any
		json.NewDecoder(r.Body).Decode(&patch)
		if len(patch) < 4 || patch[0]["path"] != "/metadata/uid" || patch[0]["op"] != "test" || patch[1]["path"] != "/metadata/resourceVersion" || patch[2]["path"] != "/spec/action" {
			t.Error("missing CAS tests")
		}
		for _, part := range patch[3:] {
			switch part["path"] {
			case "/spec/action":
				item.Spec.Action = part["value"].(string)
			case "/spec/hostFingerprint":
				item.Spec.HostFingerprint = part["value"].(string)
			case "/spec/credentialSecretRef":
				raw, _ := json.Marshal(part["value"])
				json.Unmarshal(raw, &item.Spec.CredentialSecretRef)
			case "/spec/approvedPreflightDigest":
				item.Spec.ApprovedPreflightDigest = part["value"].(string)
			default:
				t.Error("unreviewed mutable CR field")
			}
		}
		item.Metadata.Generation++
		item.Metadata.ResourceVersion = "2"
		f.complete(item, id)
	} else if r.Method != "GET" {
		t.Error("unexpected method")
	}
	json.NewEncoder(w).Encode(item)
}
func (f *kubeEnrollmentFixture) complete(item *enrollmentResource, id string) {
	item.Status.ObservedGeneration = item.Metadata.Generation
	item.Status.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if item.Spec.Action == "Scan" {
		item.Status.Phase = "AwaitingPreflight"
		item.Status.Scan = &enrollmentScan{ID: id, Host: item.Spec.Host, Port: item.Spec.Port, Region: item.Spec.Region, ExpiresAt: time.Now().Add(time.Minute).Unix()}
		json.Unmarshal([]byte(`[{"algorithm":"ssh-ed25519","fingerprint":"SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}]`), &item.Status.Scan.Fingerprints)
	}
	if item.Spec.Action == "Preflight" {
		item.Status.Phase = "AwaitingApproval"
		item.Status.PreflightDigest = strings.Repeat("a", 64)
		item.Status.Preflight = &enrollmentPreflight{ID: id, Host: item.Spec.Host, Region: item.Spec.Region, ExpiresAt: time.Now().Add(time.Minute).Unix(), CanJoin: true, Plan: []string{"Install pinned worker"}}
	}
	if item.Spec.Action == "Join" {
		item.Status.Phase = "Installing"
	}
}
func enrollmentPost(t *testing.T, s *Server, cookie *http.Cookie, csrf, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := request("POST", apiPrefix+"node-onboarding"+path, body)
	r.AddCookie(cookie)
	r.Header.Set("X-CSRF-Token", csrf)
	return perform(s, r)
}
func beginFixtureScan(t *testing.T, s *Server, cookie *http.Cookie, csrf string) string {
	t.Helper()
	w := enrollmentPost(t, s, cookie, csrf, "/scan", `{"region":"test","host":"8.8.8.8","port":22}`)
	if w.Code != 200 {
		t.Fatalf("scan status %d", w.Code)
	}
	var scan enrollmentScan
	json.Unmarshal(w.Body.Bytes(), &scan)
	if !enrollmentID.MatchString(scan.ID) {
		t.Fatal("invalid generated scan id")
	}
	return scan.ID
}
func preflightFixture(t *testing.T, s *Server, cookie *http.Cookie, csrf, id string) *httptest.ResponseRecorder {
	return enrollmentPost(t, s, cookie, csrf, "/preflight", `{"scan_id":"`+id+`","username":"root","password":"fixture-password-DO_NOT_DISCLOSE","fingerprint":"SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`)
}

func TestNodeOnboardingKubernetesLifecycleAndSafeProjection(t *testing.T) {
	f := newKubeEnrollmentFixture(t)
	s, _ := newTestServer(t)
	s.enrollment = f.client
	cookie, csrf := authenticate(t, s)
	id := beginFixtureScan(t, s, cookie, csrf)
	w := preflightFixture(t, s, cookie, csrf, id)
	if w.Code != 200 {
		t.Fatalf("preflight %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "DO_NOT_DISCLOSE") || strings.Contains(w.Body.String(), "secret-fixture") {
		t.Fatal("credential in browser response")
	}
	w = enrollmentPost(t, s, cookie, csrf, "/join", `{"preflight_id":"`+id+`"}`)
	if w.Code != 200 {
		t.Fatalf("join %d", w.Code)
	}
	patches := f.patchCalls
	w = enrollmentPost(t, s, cookie, csrf, "/join", `{"preflight_id":"`+id+`"}`)
	if w.Code != 200 || f.patchCalls != patches {
		t.Fatal("join retry was not idempotent")
	}
	if f.secretCalls != 1 || f.deleteCalls != 0 {
		t.Fatal("unexpected secret lifecycle")
	}
	for _, call := range f.calls {
		if strings.Contains(call, "GET "+enrollmentSecretPath) || strings.Contains(call, "pods") || strings.Contains(call, "nodes") || strings.Contains(call, "exec") {
			t.Fatal("submission token escaped resource allowlist")
		}
	}
}
func TestPreflightStateFailuresNeverAdoptUnknownSecret(t *testing.T) {
	for _, mode := range []string{"expired", "mismatched fingerprint", "secret conflict", "CAS conflict", "ambiguous patch"} {
		t.Run(mode, func(t *testing.T) {
			f := newKubeEnrollmentFixture(t)
			s, _ := newTestServer(t)
			s.enrollment = f.client
			cookie, csrf := authenticate(t, s)
			id := beginFixtureScan(t, s, cookie, csrf)
			switch mode {
			case "expired":
				f.records[id].Status.Scan.ExpiresAt = time.Now().Unix() - 1
			case "mismatched fingerprint":
				f.records[id].Status.Scan.Fingerprints[0].Fingerprint = "SHA256:" + strings.Repeat("B", 43)
			case "secret conflict":
				f.secretConflict = true
			case "CAS conflict":
				f.patchConflict = true
			case "ambiguous patch":
				f.patchAmbiguous = true
			}
			w := preflightFixture(t, s, cookie, csrf, id)
			if w.Code < 400 {
				t.Fatal("invalid preflight accepted")
			}
			if mode == "CAS conflict" {
				if f.deleteCalls != 1 {
					t.Fatal("unaccepted secret not cleaned with UID")
				}
			} else if f.deleteCalls != 0 {
				t.Fatal("ambiguous/unknown secret was deleted")
			}
			if (mode == "expired" || mode == "mismatched fingerprint") && f.secretCalls != 0 {
				t.Fatal("credentials submitted before authorization")
			}
		})
	}
}
func TestEnrollmentPrivateTokenRotationAndTLSBoundaries(t *testing.T) {
	f := newKubeEnrollmentFixture(t)
	ctx := context.Background()
	path := enrollmentAPIPath + "/nodeenrollments?limit=100"
	if e := f.client.request(ctx, "GET", path, nil, &map[string]any{}); e != nil {
		t.Fatal(e)
	}
	replacement := f.client.cfg.TokenFile + ".next"
	rotated := fixtureEnrollmentToken(enrollmentSubject, time.Now().Unix(), time.Now().Unix()+3600, "two")
	os.WriteFile(replacement, []byte(rotated), 0600)
	os.Rename(replacement, f.client.cfg.TokenFile)
	f.token = rotated
	if e := f.client.request(ctx, "GET", path, nil, &map[string]any{}); e != nil {
		t.Fatal("rotated credential was not read")
	}
	os.Chmod(f.client.cfg.TokenFile, 0644)
	if _, e := readEnrollmentToken(f.client.cfg.TokenFile); e == nil {
		t.Fatal("public credential permissions accepted")
	}
	os.Chmod(f.client.cfg.TokenFile, 0600)
	link := f.client.cfg.TokenFile + ".link"
	os.Symlink(f.client.cfg.TokenFile, link)
	if _, e := readEnrollmentToken(link); e == nil {
		t.Fatal("symlink credential accepted")
	}
	wrong := newKubeEnrollmentFixture(t)
	wrong.client.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = x509.NewCertPool()
	if e := wrong.client.request(ctx, "GET", path, nil, &map[string]any{}); e == nil {
		t.Fatal("unpinned API certificate accepted")
	}
	before := len(f.calls)
	for _, p := range []string{enrollmentSecretPath, "/api/v1/nodes", enrollmentPath(strings.Repeat("a", 32)) + "/status", enrollmentAPIPath + "/nodeenrollments?limit=100&watch=true"} {
		if e := f.client.request(ctx, "GET", p, nil, nil); e == nil {
			t.Fatal("unreviewed API route accepted")
		}
	}
	if len(f.calls) != before {
		t.Fatal("invalid routes reached network")
	}
}
func TestEnrollmentRedirectAndBodyLimitDoNotExposeAPIResponses(t *testing.T) {
	for _, mode := range []string{"redirect", "oversized", "errorbody"} {
		t.Run(mode, func(t *testing.T) {
			f := newKubeEnrollmentFixture(t)
			f.responseMode = mode
			e := f.client.request(context.Background(), "GET", enrollmentAPIPath+"/nodeenrollments?limit=100", nil, &map[string]any{})
			if e == nil || strings.Contains(e.Error(), "DO_NOT_DISCLOSE") {
				t.Fatal("unsafe Kubernetes response")
			}
		})
	}
}
func TestEnrollmentConfigBindsClusterAndSeparateCredential(t *testing.T) {
	f := newKubeEnrollmentFixture(t)
	cfg := f.client.cfg
	source := SourceConfig{Regions: []RegionConfig{{Name: "test", APIURL: cfg.APIURL, CAFile: cfg.CAFile, TokenFile: filepath.Join(t.TempDir(), "observer-token")}}}
	if e := cfg.validate(source); e != nil {
		t.Fatal(e)
	}
	for _, url := range []string{"http://127.0.0.1:6443", cfg.APIURL + "/arbitrary", cfg.APIURL + "?token=x", cfg.APIURL + "#fragment", "https://user:password@cluster.invalid"} {
		bad := cfg
		bad.APIURL = url
		if bad.validate(source) == nil {
			t.Fatal("unsafe API URL accepted")
		}
	}
	same := cfg
	same.TokenFile = source.Regions[0].TokenFile
	if same.validate(source) == nil {
		t.Fatal("observer token reused")
	}
	mismatch := cfg
	mismatch.Region = "other"
	if mismatch.validate(source) == nil {
		t.Fatal("cross-region cluster binding accepted")
	}
}

func fixtureEnrollmentToken(subject string, issued, expires int64, signature string) string {
	raw, _ := json.Marshal(map[string]any{"sub": subject, "iat": issued, "exp": expires})
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`)) + "." + base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString([]byte(signature))
}
func TestEnrollmentTokenIdentityAndExpiryGuard(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name, subject   string
		issued, expires int64
		want            bool
	}{
		{"hour", enrollmentSubject, now.Unix(), now.Unix() + 3600, true},
		{"observer", "system:serviceaccount:agones-control:fleet-console", now.Unix(), now.Unix() + 3600, false},
		{"expiring", enrollmentSubject, now.Unix() - 3600, now.Unix() + 29, false},
		{"expired", enrollmentSubject, now.Unix() - 3600, now.Unix() - 1, false},
		{"long lived", enrollmentSubject, now.Unix(), now.Unix() + 7201, false},
		{"future", enrollmentSubject, now.Unix() + 61, now.Unix() + 3600, false},
		{"no issued", enrollmentSubject, 0, now.Unix() + 3600, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := fixtureEnrollmentToken(tc.subject, tc.issued, tc.expires, "synthetic-unsigned")
			if validEnrollmentToken([]byte(token), now) != tc.want {
				t.Fatal("unexpected identity/expiry result")
			}
		})
	}
	if validEnrollmentToken([]byte("plain-fixture-key"), now) {
		t.Fatal("non-TokenRequest credential accepted")
	}
}
func TestEnrollmentPendingAndGenerationNeverInventCompletion(t *testing.T) {
	f := newKubeEnrollmentFixture(t)
	f.pendingScan = true
	s, _ := newTestServer(t)
	s.enrollment = f.client
	cookie, csrf := authenticate(t, s)
	w := enrollmentPost(t, s, cookie, csrf, "/scan", `{"region":"test","host":"8.8.8.8","port":22}`)
	var job enrollmentJob
	json.Unmarshal(w.Body.Bytes(), &job)
	if w.Code != 202 || !job.Pending || job.Phase != "Scanning" || !enrollmentID.MatchString(job.ID) {
		t.Fatal("scan pending lost")
	}
	record := f.records[job.ID]
	record.Metadata.Generation++
	record.Spec.Action = "Preflight"
	stale := projectedEnrollment(record, job.ID)
	if stale.Phase != "Preflighting" || stale.Scan != nil || stale.Preflight != nil {
		t.Fatal("stale status was presented as current completion")
	}
	record.Status.ObservedGeneration = record.Metadata.Generation
	record.Status.Phase = "password=DO_NOT_DISCLOSE"
	raw, _ := json.Marshal(projectedEnrollment(record, job.ID))
	if strings.Contains(string(raw), "DO_NOT_DISCLOSE") {
		t.Fatal("unreviewed status exposed")
	}
}
func TestEnrollmentPublicVPSHostGuard(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "10.0.0.1", "172.31.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "100.127.255.254", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "240.1.1.1", "::1", "::ffff:8.8.8.8", "worker.example.com", "8.8.8.8/32", "8.8.8.8;id"} {
		if validEnrollmentHost(host) {
			t.Fatalf("invalid VPS target accepted: %s", host)
		}
	}
	if !validEnrollmentHost("8.8.8.8") {
		t.Fatal("public IPv4 rejected")
	}
}

func TestFailedPreflightPreservesChecksWithoutJoinAuthorization(t *testing.T) {
	f := newKubeEnrollmentFixture(t)
	s, _ := newTestServer(t)
	s.enrollment = f.client
	cookie, csrf := authenticate(t, s)
	id := beginFixtureScan(t, s, cookie, csrf)
	if preflightFixture(t, s, cookie, csrf, id).Code != 200 {
		t.Fatal("preflight setup failed")
	}
	item := f.records[id]
	item.Status.Phase = "Failed"
	item.Status.Preflight.CanJoin = false
	item.Status.Preflight.Checks = []enrollmentCheck{{Name: "resources", OK: false, Detail: "Insufficient memory"}}
	w := httptest.NewRecorder()
	writeEnrollmentAction(w, item, id, "preflight")
	var result enrollmentPreflight
	json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 200 || result.CanJoin || len(result.Checks) != 1 {
		t.Fatal("failed checks were hidden or authorized")
	}
	if len(projectedEnrollment(item, id).Checks) != 1 {
		t.Fatal("async diagnostic checks missing")
	}
	w = enrollmentPost(t, s, cookie, csrf, "/join", `{"preflight_id":"`+id+`"}`)
	if w.Code != 409 {
		t.Fatal("failed preflight allowed Join")
	}
}
