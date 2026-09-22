package console

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type enrollmentMeta struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
	Generation      int64  `json:"generation"`
	CreatedAt       string `json:"creationTimestamp"`
}
type enrollmentRef struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
}
type enrollmentSpec struct {
	Region                  string         `json:"region"`
	Host                    string         `json:"host"`
	Port                    int            `json:"port"`
	Action                  string         `json:"action"`
	HostFingerprint         string         `json:"hostFingerprint,omitempty"`
	CredentialSecretRef     *enrollmentRef `json:"credentialSecretRef,omitempty"`
	ApprovedPreflightDigest string         `json:"approvedPreflightDigest,omitempty"`
}
type enrollmentCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}
type enrollmentScan struct {
	ID           string `json:"scan_id"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	Region       string `json:"region"`
	ExpiresAt    int64  `json:"expires_at"`
	Fingerprints []struct {
		Algorithm   string `json:"algorithm"`
		Fingerprint string `json:"fingerprint"`
	} `json:"fingerprints"`
}
type enrollmentPreflight struct {
	ID                   string            `json:"preflight_id"`
	Host                 string            `json:"host"`
	Region               string            `json:"region"`
	NodeName             string            `json:"node_name"`
	ExpiresAt            int64             `json:"expires_at"`
	CanJoin              bool              `json:"can_join"`
	PrivateIP            string            `json:"private_ip"`
	ExternalIP           string            `json:"external_ip"`
	Interface            string            `json:"interface"`
	CPUCores             float64           `json:"cpu_cores"`
	MemoryMiB            float64           `json:"memory_mib"`
	DiskFreeGiB          float64           `json:"disk_free_gib"`
	ExistingInstallation bool              `json:"existing_installation"`
	Checks               []enrollmentCheck `json:"checks"`
	Plan                 []string          `json:"plan"`
}
type enrollmentJob struct {
	ID                string               `json:"id"`
	Host              string               `json:"host,omitempty"`
	Region            string               `json:"region"`
	NodeName          string               `json:"node_name,omitempty"`
	NodeUID           string               `json:"node_uid,omitempty"`
	State             string               `json:"state"`
	Stage             string               `json:"stage"`
	Error             string               `json:"error,omitempty"`
	CreatedAt         int64                `json:"created_at"`
	UpdatedAt         int64                `json:"updated_at"`
	Checks            []enrollmentCheck    `json:"checks,omitempty"`
	Phase             string               `json:"phase"`
	Pending           bool                 `json:"pending,omitempty"`
	Scan              *enrollmentScan      `json:"scan,omitempty"`
	Preflight         *enrollmentPreflight `json:"preflight,omitempty"`
	Blockers          []retirementBlocker  `json:"blockers,omitempty"`
	AllowedSystemPods []retirementBlocker  `json:"allowed_system_pods,omitempty"`
	BlockersTruncated bool                 `json:"blockers_truncated,omitempty"`
}
type enrollmentResource struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   enrollmentMeta `json:"metadata"`
	Spec       enrollmentSpec `json:"spec"`
	Status     struct {
		Phase              string               `json:"phase"`
		ObservedGeneration int64                `json:"observedGeneration"`
		UpdatedAt          string               `json:"updatedAt"`
		Error              string               `json:"error"`
		PreflightDigest    string               `json:"preflightDigest"`
		Scan               *enrollmentScan      `json:"scan"`
		Preflight          *enrollmentPreflight `json:"preflight"`
		Job                enrollmentJob        `json:"job"`
	} `json:"status,omitempty"`
}

func enrollmentPath(id string) string { return enrollmentAPIPath + "/nodeenrollments/enroll-" + id }
func newEnrollmentID() (string, error) {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", sourceError{503, "node_onboarding_unavailable"}
	}
	return hex.EncodeToString(b[:]), nil
}
func validEnrollmentHost(host string) bool {
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.Is4() || host != ip.String() || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	// Public IPv4 VPS targets only. The controller additionally checks its protected hosts.
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4"} {
		if netip.MustParsePrefix(cidr).Contains(ip) {
			return false
		}
	}
	return true
}
func (c *enrollmentClient) get(ctx context.Context, id string) (*enrollmentResource, error) {
	var r enrollmentResource
	if e := c.request(ctx, http.MethodGet, enrollmentPath(id), nil, &r); e != nil {
		return nil, e
	}
	if e := c.verify(r, id); e != nil {
		return nil, e
	}
	return &r, nil
}
func (c *enrollmentClient) verify(r enrollmentResource, id string) error {
	if r.APIVersion != enrollmentAPIVersion || r.Kind != "NodeEnrollment" || r.Metadata.Name != "enroll-"+id || r.Metadata.Namespace != enrollmentNamespace || !enrollmentUID.MatchString(r.Metadata.UID) || !enrollmentVersion.MatchString(r.Metadata.ResourceVersion) || r.Metadata.Generation < 1 || r.Spec.Region != c.cfg.Region || !validEnrollmentHost(r.Spec.Host) || r.Spec.Port < 1 || r.Spec.Port > 65535 {
		return sourceError{502, "node_onboarding_invalid_response"}
	}
	switch r.Spec.Action {
	case "Scan", "Preflight", "Join":
	default:
		return sourceError{502, "node_onboarding_invalid_response"}
	}
	return nil
}
func enrollmentPhase(r *enrollmentResource) string {
	if r.Status.ObservedGeneration != r.Metadata.Generation {
		switch r.Spec.Action {
		case "Scan":
			return "Scanning"
		case "Preflight":
			return "Preflighting"
		case "Join":
			return "Installing"
		}
	}
	switch r.Status.Phase {
	case "Scanning", "AwaitingPreflight", "Preflighting", "AwaitingApproval", "Installing", "Verifying", "Ready", "Failed", "NeedsReview", "Expired":
		return r.Status.Phase
	default:
		return "NeedsReview"
	}
}
func smallSafe(s string) string {
	s = safeText(s)
	if len(s) > 512 {
		return s[:512]
	}
	return s
}
func safeChecks(input []enrollmentCheck) []enrollmentCheck {
	if len(input) > 32 {
		input = input[:32]
	}
	out := make([]enrollmentCheck, len(input))
	for i, v := range input {
		out[i] = enrollmentCheck{smallSafe(v.Name), v.OK, smallSafe(v.Detail)}
	}
	return out
}
func safeScan(r *enrollmentResource, id string) *enrollmentScan {
	input := r.Status.Scan
	if input == nil || input.ID != id || input.Host != r.Spec.Host || input.Region != r.Spec.Region || input.Port != r.Spec.Port || input.ExpiresAt <= 0 {
		return nil
	}
	out := *input
	out.Fingerprints = nil
	for _, f := range input.Fingerprints {
		if f.Algorithm == "ssh-ed25519" && enrollmentFingerprint.MatchString(f.Fingerprint) {
			out.Fingerprints = append(out.Fingerprints, f)
			if len(out.Fingerprints) == 4 {
				break
			}
		}
	}
	if len(out.Fingerprints) == 0 {
		return nil
	}
	return &out
}
func safePreflight(r *enrollmentResource, id string) *enrollmentPreflight {
	p := r.Status.Preflight
	if p == nil || p.ID != id || p.Host != r.Spec.Host || p.Region != r.Spec.Region || p.ExpiresAt <= 0 {
		return nil
	}
	v := *p
	v.NodeName = smallSafe(v.NodeName)
	v.Interface = smallSafe(v.Interface)
	v.PrivateIP = smallSafe(v.PrivateIP)
	v.ExternalIP = smallSafe(v.ExternalIP)
	v.Checks = safeChecks(v.Checks)
	v.Plan = []string{}
	for _, item := range p.Plan {
		v.Plan = append(v.Plan, smallSafe(item))
		if len(v.Plan) == 16 {
			break
		}
	}
	return &v
}
func projectedEnrollment(r *enrollmentResource, id string) enrollmentJob {
	phase := enrollmentPhase(r)
	state := "queued"
	switch phase {
	case "Installing":
		state = "installing"
	case "Verifying":
		state = "verifying"
	case "Ready":
		state = "ready"
	case "Failed":
		state = "failed"
	case "NeedsReview":
		state = "needs_review"
	case "Expired":
		state = "expired"
	}
	created, _ := time.Parse(time.RFC3339, r.Metadata.CreatedAt)
	updated, _ := time.Parse(time.RFC3339, r.Status.UpdatedAt)
	job := enrollmentJob{ID: id, Host: r.Spec.Host, Region: r.Spec.Region, State: state, Phase: phase, CreatedAt: created.Unix(), UpdatedAt: updated.Unix(), Stage: phase}
	if created.IsZero() {
		job.CreatedAt = 0
	}
	if updated.IsZero() {
		job.UpdatedAt = job.CreatedAt
	}
	if safeErrorCode(r.Status.Error) {
		job.Error = r.Status.Error
	}
	if safeErrorCode(r.Status.Job.Stage) {
		job.Stage = r.Status.Job.Stage
	}
	job.NodeName = smallSafe(r.Status.Job.NodeName)
	job.Checks = safeChecks(r.Status.Job.Checks)
	if r.Status.ObservedGeneration == r.Metadata.Generation {
		job.Scan = safeScan(r, id)
		job.Preflight = safePreflight(r, id)
		if len(job.Checks) == 0 && job.Preflight != nil {
			job.Checks = job.Preflight.Checks
		}
	}
	return job
}
func (c *enrollmentClient) wait(ctx context.Context, r *enrollmentResource, id, phase string) *enrollmentResource {
	poll, stop := context.WithTimeout(ctx, 4*time.Second)
	defer stop()
	for enrollmentPhase(r) != phase && enrollmentPhase(r) != "Failed" && enrollmentPhase(r) != "NeedsReview" && enrollmentPhase(r) != "Expired" {
		select {
		case <-poll.Done():
			return r
		case <-time.After(200 * time.Millisecond):
		}
		next, e := c.get(poll, id)
		if e != nil {
			return r
		}
		r = next
	}
	return r
}

// Writes remain GUI-only. Machine read tokens can inspect only these safe projections.
func (s *Server) authorizeNodeAPI(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == http.MethodGet && method == http.MethodGet {
		if r.ContentLength != 0 || hasCredentialQuery(r.URL.Query()) {
			writeError(w, 400, "invalid_request")
			return false
		}
		_, ok := s.authenticateReadAPI(w, r)
		return ok
	}
	if len(r.Header.Values("Authorization")) != 0 {
		writeError(w, 403, "api_token_scope")
		return false
	}
	if r.Method != method {
		methodNotAllowed(w, method)
		return false
	}
	current, ok := s.requestSession(r)
	if !ok {
		writeError(w, 401, "unauthenticated")
		return false
	}
	if method == http.MethodPost {
		if !s.validOrigin(r) {
			writeError(w, 403, "invalid_origin")
			return false
		}
		if !validCSRF(r, current.CSRF) {
			writeError(w, 403, "invalid_csrf")
			return false
		}
	}
	return true
}
func jobQuery(raw string) (string, bool) {
	q, e := url.ParseQuery(raw)
	return q.Get("id"), e == nil && len(q) == 1 && len(q["id"]) == 1 && enrollmentID.MatchString(q.Get("id"))
}
func (s *Server) nodeOnboardingAPI(w http.ResponseWriter, r *http.Request) {
	route := strings.TrimPrefix(r.URL.Path, apiPrefix+"node-onboarding")
	method := http.MethodGet
	switch route {
	case "", "/jobs":
	case "/scan", "/preflight", "/join":
		method = http.MethodPost
	default:
		writeError(w, 404, "not_found")
		return
	}
	if !s.authorizeNodeAPI(w, r, method) {
		return
	}
	var id string
	if route == "/jobs" {
		var ok bool
		id, ok = jobQuery(r.URL.RawQuery)
		if !ok {
			writeError(w, 400, "invalid_job_id")
			return
		}
	} else if r.URL.RawQuery != "" {
		writeError(w, 400, "invalid_query")
		return
	}
	if s.enrollment == nil {
		if route == "" {
			writeJSON(w, 200, map[string]any{"enabled": false, "regions": []any{}, "jobs": []any{}})
		} else {
			writeError(w, 503, "node_onboarding_not_configured")
		}
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 18*time.Second)
	defer cancel()
	c := s.enrollment
	switch route {
	case "":
		var list struct {
			Items    []enrollmentResource `json:"items"`
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
		}
		if e := c.request(ctx, http.MethodGet, enrollmentAPIPath+"/nodeenrollments?limit=100", nil, &list); e != nil {
			writeBackend(w, nil, e)
			return
		}
		jobs := []enrollmentJob{}
		for _, item := range list.Items {
			id := strings.TrimPrefix(item.Metadata.Name, "enroll-")
			if enrollmentID.MatchString(id) && c.verify(item, id) == nil {
				jobs = append(jobs, projectedEnrollment(&item, id))
			}
		}
		sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt > jobs[j].CreatedAt })
		writeJSON(w, 200, map[string]any{"enabled": true, "username_mode": "root_only", "transport": "kubernetes_api", "regions": []any{map[string]any{"name": c.cfg.Region, "cluster_id": c.cfg.ClusterID, "server": c.cfg.APIURL, "game_port_min": c.cfg.GamePortMin, "game_port_max": c.cfg.GamePortMax}}, "jobs": jobs, "jobs_truncated": list.Metadata.Continue != ""})
	case "/jobs":
		item, e := c.get(ctx, id)
		if e != nil {
			writeBackend(w, nil, e)
			return
		}
		writeJSON(w, 200, projectedEnrollment(item, id))
	case "/scan":
		var input struct {
			Region string `json:"region"`
			Host   string `json:"host"`
			Port   int    `json:"port"`
		}
		if !readJSON(w, r, &input) {
			return
		}
		if input.Port == 0 {
			input.Port = 22
		}
		if input.Region != c.cfg.Region {
			writeError(w, 400, "unknown_region")
			return
		}
		if !validEnrollmentHost(input.Host) || input.Port < 1 || input.Port > 65535 {
			writeError(w, 400, "invalid_request")
			return
		}
		id, e := newEnrollmentID()
		if e != nil {
			writeBackend(w, nil, e)
			return
		}
		request := map[string]any{"apiVersion": enrollmentAPIVersion, "kind": "NodeEnrollment", "metadata": map[string]string{"name": "enroll-" + id, "namespace": enrollmentNamespace}, "spec": enrollmentSpec{Region: input.Region, Host: input.Host, Port: input.Port, Action: "Scan"}}
		var item enrollmentResource
		if e = c.request(ctx, http.MethodPost, enrollmentAPIPath+"/nodeenrollments", request, &item); e != nil {
			writeBackend(w, nil, e)
			return
		}
		if e = c.verify(item, id); e != nil {
			writeBackend(w, nil, e)
			return
		}
		item = *c.wait(ctx, &item, id, "AwaitingPreflight")
		writeEnrollmentAction(w, &item, id, "scan")
	case "/preflight":
		s.submitPreflight(ctx, w, r)
	case "/join":
		s.submitJoin(ctx, w, r)
	}
}
func writeEnrollmentAction(w http.ResponseWriter, item *enrollmentResource, id, action string) {
	view := projectedEnrollment(item, id)
	// A completed, failed preflight is still useful diagnostic data. It never permits Join.
	if action == "preflight" && view.Phase == "Failed" && view.Preflight != nil && !view.Preflight.CanJoin {
		writeJSON(w, 200, view.Preflight)
		return
	}
	if view.State == "failed" || view.State == "needs_review" || view.State == "expired" {
		code := view.Error
		if code == "" {
			code = "node_onboarding_failed"
		}
		writeError(w, 409, code)
		return
	}
	if action == "scan" && view.Phase == "AwaitingPreflight" && view.Scan != nil {
		writeJSON(w, 200, view.Scan)
		return
	}
	if action == "preflight" && view.Phase == "AwaitingApproval" && view.Preflight != nil {
		writeJSON(w, 200, view.Preflight)
		return
	}
	view.Pending = true
	writeJSON(w, 202, view)
}
func casEnrollment(item *enrollmentResource, updates map[string]any) []map[string]any {
	out := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": item.Metadata.UID}, {"op": "test", "path": "/metadata/resourceVersion", "value": item.Metadata.ResourceVersion}, {"op": "test", "path": "/spec/action", "value": item.Spec.Action}}
	keys := make([]string, 0, len(updates))
	for k := range updates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, map[string]any{"op": "add", "path": "/spec/" + k, "value": updates[k]})
	}
	return out
}
func (s *Server) submitPreflight(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID          string `json:"scan_id"`
		Username    string `json:"username"`
		Password    string `json:"password"`
		Fingerprint string `json:"fingerprint"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !enrollmentID.MatchString(input.ID) || input.Username != "root" || !enrollmentFingerprint.MatchString(input.Fingerprint) || !utf8.ValidString(input.Password) || len(input.Password) < 1 || len(input.Password) > 1024 || strings.ContainsAny(input.Password, "\r\n\x00") {
		writeError(w, 400, "invalid_request")
		return
	}
	c := s.enrollment
	item, e := c.get(ctx, input.ID)
	if e != nil {
		writeBackend(w, nil, e)
		return
	}
	scan := safeScan(item, input.ID)
	if item.Spec.Action != "Scan" || enrollmentPhase(item) != "AwaitingPreflight" || scan == nil || scan.ExpiresAt <= time.Now().Unix() {
		writeError(w, 409, "scan_expired")
		return
	}
	matched := false
	for _, f := range scan.Fingerprints {
		matched = matched || f.Fingerprint == input.Fingerprint
	}
	if !matched {
		writeError(w, 409, "host_fingerprint_mismatch")
		return
	}
	name := "ssh-" + input.ID
	secret := map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "immutable": true, "metadata": map[string]any{"name": name, "namespace": enrollmentNamespace, "labels": map[string]string{"nakama-agones.io/enrollment-id": input.ID}, "ownerReferences": []any{map[string]any{"apiVersion": enrollmentAPIVersion, "kind": "NodeEnrollment", "name": item.Metadata.Name, "uid": item.Metadata.UID, "controller": false, "blockOwnerDeletion": false}}}, "data": map[string]string{"password": base64.StdEncoding.EncodeToString([]byte(input.Password))}}
	input.Password = ""
	var created struct {
		Metadata enrollmentMeta `json:"metadata"`
	}
	if e = c.request(ctx, http.MethodPost, enrollmentSecretPath, secret, &created); e != nil {
		writeBackend(w, nil, e)
		return
	}
	secret = nil
	if created.Metadata.Name != name || created.Metadata.Namespace != enrollmentNamespace || !enrollmentUID.MatchString(created.Metadata.UID) {
		writeError(w, 502, "node_onboarding_invalid_response")
		return
	}
	patch := casEnrollment(item, map[string]any{"action": "Preflight", "hostFingerprint": input.Fingerprint, "credentialSecretRef": enrollmentRef{Name: name, UID: created.Metadata.UID}})
	var updated enrollmentResource
	if e = c.request(ctx, http.MethodPatch, enrollmentPath(input.ID), patch, &updated); e != nil {
		// On an ambiguous PATCH failure, do not delete credentials a controller may already be using.
		// The controller's 600-second TTL also cleans orphaned creates without any Secret read here.
		if sourceCode(e) == "onboarding_state_changed" {
			cleanup, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			_ = c.request(cleanup, http.MethodDelete, enrollmentSecretPath+"/"+name, map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]string{"uid": created.Metadata.UID}}, nil)
		}
		writeBackend(w, nil, e)
		return
	}
	if e = c.verify(updated, input.ID); e != nil {
		writeBackend(w, nil, e)
		return
	}
	updated = *c.wait(ctx, &updated, input.ID, "AwaitingApproval")
	writeEnrollmentAction(w, &updated, input.ID, "preflight")
}
func (s *Server) submitJoin(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID string `json:"preflight_id"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !enrollmentID.MatchString(input.ID) {
		writeError(w, 400, "invalid_request")
		return
	}
	c := s.enrollment
	item, e := c.get(ctx, input.ID)
	if e != nil {
		writeBackend(w, nil, e)
		return
	}
	if item.Spec.Action == "Join" {
		writeJSON(w, 200, projectedEnrollment(item, input.ID))
		return
	}
	preflight := safePreflight(item, input.ID)
	if item.Spec.Action != "Preflight" || enrollmentPhase(item) != "AwaitingApproval" || preflight == nil || !preflight.CanJoin || preflight.ExpiresAt <= time.Now().Unix() || !enrollmentDigest.MatchString(item.Status.PreflightDigest) {
		writeError(w, 409, "preflight_expired")
		return
	}
	var updated enrollmentResource
	e = c.request(ctx, http.MethodPatch, enrollmentPath(input.ID), casEnrollment(item, map[string]any{"action": "Join", "approvedPreflightDigest": item.Status.PreflightDigest}), &updated)
	if e != nil {
		writeBackend(w, nil, e)
		return
	}
	if e = c.verify(updated, input.ID); e != nil {
		writeBackend(w, nil, e)
		return
	}
	writeJSON(w, 200, projectedEnrollment(&updated, input.ID))
}

// Keep API payloads typed. Unreviewed spec/status keys, SSH host key text and Secret metadata never reach the browser.
