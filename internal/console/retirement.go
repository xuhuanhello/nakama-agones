package console

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type retirementIdentity struct {
	Name            string   `json:"name"`
	UID             string   `json:"uid"`
	ResourceVersion string   `json:"resource_version"`
	InternalIPs     []string `json:"internal_ips"`
	ExternalIPs     []string `json:"external_ips"`
}
type retirementCheck struct {
	Enabled  bool               `json:"enabled"`
	Eligible bool               `json:"eligible"`
	Reason   string             `json:"reason"`
	Region   string             `json:"region"`
	Identity retirementIdentity `json:"identity"`
}
type retirementBackend interface {
	CheckNodeRetirement(context.Context, string, string, string) (retirementCheck, error)
}
type retirementSpec struct {
	Region              string   `json:"region"`
	NodeName            string   `json:"nodeName"`
	NodeUID             string   `json:"nodeUID"`
	InternalIPs         []string `json:"internalIPs"`
	ExternalIPs         []string `json:"externalIPs"`
	ConfirmedNodeName   string   `json:"confirmedNodeName"`
	ConfirmedIP         string   `json:"confirmedIP"`
	PermanentRetirement bool     `json:"permanentRetirement"`
}
type retirementBlocker struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Reason    string `json:"reason"`
}
type retirementResource struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   enrollmentMeta `json:"metadata"`
	Spec       retirementSpec `json:"spec"`
	Status     struct {
		Phase              string              `json:"phase"`
		ObservedGeneration int64               `json:"observedGeneration"`
		UpdatedAt          string              `json:"updatedAt"`
		Error              string              `json:"error"`
		Job                enrollmentJob       `json:"job"`
		Blockers           []retirementBlocker `json:"blockers"`
		AllowedSystemPods  []retirementBlocker `json:"allowedSystemPods"`
		BlockersTruncated  bool                `json:"blockersTruncated"`
	} `json:"status,omitempty"`
}

func retirementPath(id string) string { return enrollmentAPIPath + "/noderetirements/retire-" + id }
func (c *enrollmentClient) verifyRetirement(v retirementResource, id string) error {
	if v.APIVersion != enrollmentAPIVersion || v.Kind != "NodeRetirement" || v.Metadata.Name != "retire-"+id || v.Metadata.Namespace != enrollmentNamespace || !enrollmentUID.MatchString(v.Metadata.UID) || !enrollmentVersion.MatchString(v.Metadata.ResourceVersion) || v.Metadata.Generation < 1 || v.Spec.Region != c.cfg.Region || !enrollmentLabel.MatchString(v.Spec.NodeName) || strings.Contains(v.Spec.NodeName, "..") || !enrollmentUID.MatchString(v.Spec.NodeUID) || !v.Spec.PermanentRetirement || v.Spec.ConfirmedNodeName != v.Spec.NodeName || !validRetirementIPs(v.Spec.InternalIPs, v.Spec.ExternalIPs, v.Spec.ConfirmedIP) {
		return sourceError{502, "node_retirement_invalid_response"}
	}
	return nil
}
func validRetirementIPs(internal, external []string, confirmed string) bool {
	found := false
	for _, values := range [][]string{internal, external} {
		if len(values) > 8 {
			return false
		}
		seen := map[string]bool{}
		for _, value := range values {
			ip := net.ParseIP(value)
			if ip == nil || ip.String() != value || seen[value] {
				return false
			}
			seen[value] = true
			found = found || value == confirmed
		}
	}
	return found
}
func safeRetirementBlockers(input []retirementBlocker) []retirementBlocker {
	out := []retirementBlocker{}
	for _, v := range input {
		if (v.Kind != "Pod" && v.Kind != "GameServer") || !dnsName.MatchString(v.Namespace) || !dnsName.MatchString(v.Name) || strings.Contains(v.Namespace, "..") || strings.Contains(v.Name, "..") || !safeErrorCode(v.Reason) {
			continue
		}
		out = append(out, v)
		if len(out) == 200 {
			break
		}
	}
	return out
}
func projectedRetirement(v *retirementResource, id string) enrollmentJob {
	phase := v.Status.Phase
	if v.Status.ObservedGeneration != v.Metadata.Generation {
		phase = "Pending"
	}
	state := "queued"
	switch phase {
	case "Pending":
	case "Checking":
		state = "checking"
	case "Deleted":
		state = "deleted"
	case "Blocked":
		state = "blocked"
	case "NeedsReview":
		state = "needs_review"
	default:
		phase, state = "NeedsReview", "needs_review"
	}
	created, _ := time.Parse(time.RFC3339, v.Metadata.CreatedAt)
	updated, _ := time.Parse(time.RFC3339, v.Status.UpdatedAt)
	job := enrollmentJob{ID: id, Region: v.Spec.Region, NodeName: v.Spec.NodeName, NodeUID: v.Spec.NodeUID, State: state, Phase: phase, Stage: phase, CreatedAt: created.Unix(), UpdatedAt: updated.Unix()}
	if created.IsZero() {
		job.CreatedAt = 0
	}
	if updated.IsZero() {
		job.UpdatedAt = job.CreatedAt
	}
	if safeErrorCode(v.Status.Error) {
		job.Error = v.Status.Error
	}
	if safeErrorCode(v.Status.Job.Stage) {
		job.Stage = v.Status.Job.Stage
	}
	if v.Status.ObservedGeneration == v.Metadata.Generation {
		job.Blockers = safeRetirementBlockers(v.Status.Blockers)
		job.AllowedSystemPods = safeRetirementBlockers(v.Status.AllowedSystemPods)
		job.BlockersTruncated = v.Status.BlockersTruncated || len(v.Status.Blockers) > 200
	}
	return job
}
func (s *Server) nodeRetirementAPI(w http.ResponseWriter, r *http.Request) {
	route := strings.TrimPrefix(r.URL.Path, apiPrefix+"node-retirement")
	method := http.MethodGet
	if route != "" && route != "/jobs" {
		writeError(w, 404, "not_found")
		return
	}
	if route == "" && r.Method == http.MethodPost {
		method = http.MethodPost
	}
	if !s.authorizeNodeAPI(w, r, method) {
		return
	}
	if s.enrollment == nil {
		if method == http.MethodGet && route == "" {
			writeJSON(w, 200, map[string]any{"enabled": false, "eligible": false, "reason": "node_onboarding_not_configured"})
		} else {
			writeError(w, 503, "node_onboarding_not_configured")
		}
		return
	}
	c := s.enrollment
	ctx, cancel := context.WithTimeout(r.Context(), 18*time.Second)
	defer cancel()
	if route == "/jobs" {
		if r.URL.RawQuery == "" {
			var list struct {
				Items    []retirementResource `json:"items"`
				Metadata struct {
					Continue string `json:"continue"`
				} `json:"metadata"`
			}
			if e := c.request(ctx, http.MethodGet, enrollmentAPIPath+"/noderetirements?limit=100", nil, &list); e != nil {
				writeBackend(w, nil, e)
				return
			}
			jobs := []enrollmentJob{}
			for _, value := range list.Items {
				id := strings.TrimPrefix(value.Metadata.Name, "retire-")
				if enrollmentID.MatchString(id) && c.verifyRetirement(value, id) == nil {
					jobs = append(jobs, projectedRetirement(&value, id))
				}
			}
			sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt > jobs[j].CreatedAt })
			writeJSON(w, 200, map[string]any{"jobs": jobs, "jobs_truncated": list.Metadata.Continue != ""})
			return
		}
		id, ok := jobQuery(r.URL.RawQuery)
		if !ok {
			writeError(w, 400, "invalid_job_id")
			return
		}
		var value retirementResource
		if e := c.request(ctx, http.MethodGet, retirementPath(id), nil, &value); e != nil {
			writeBackend(w, nil, e)
			return
		}
		if e := c.verifyRetirement(value, id); e != nil {
			writeBackend(w, nil, e)
			return
		}
		writeJSON(w, 200, projectedRetirement(&value, id))
		return
	}
	backend, ok := s.backend.(retirementBackend)
	if !ok {
		writeError(w, 503, "node_retirement_unavailable")
		return
	}
	if method == http.MethodGet {
		q, e := url.ParseQuery(r.URL.RawQuery)
		if e != nil || len(q) != 2 || len(q["region"]) != 1 || len(q["node"]) != 1 || q.Get("region") != c.cfg.Region || !enrollmentLabel.MatchString(q.Get("node")) || strings.Contains(q.Get("node"), "..") {
			writeError(w, 400, "invalid_query")
			return
		}
		value, e := backend.CheckNodeRetirement(ctx, c.cfg.Region, q.Get("node"), c.cfg.ClusterID)
		writeBackend(w, value, e)
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, 400, "invalid_query")
		return
	}
	var input struct {
		Region              string `json:"region"`
		NodeName            string `json:"node_name"`
		NodeUID             string `json:"node_uid"`
		ResourceVersion     string `json:"resource_version"`
		ConfirmedNodeName   string `json:"confirmed_node_name"`
		ConfirmedIP         string `json:"confirmed_ip"`
		PermanentRetirement bool   `json:"permanent_retirement"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if input.Region != c.cfg.Region || !enrollmentLabel.MatchString(input.NodeName) || strings.Contains(input.NodeName, "..") || !enrollmentUID.MatchString(input.NodeUID) || !enrollmentVersion.MatchString(input.ResourceVersion) || input.ConfirmedNodeName != input.NodeName || net.ParseIP(input.ConfirmedIP) == nil || !input.PermanentRetirement {
		writeError(w, 400, "invalid_retirement_confirmation")
		return
	}
	check, e := backend.CheckNodeRetirement(ctx, input.Region, input.NodeName, c.cfg.ClusterID)
	if e != nil {
		writeBackend(w, nil, e)
		return
	}
	if !check.Eligible {
		writeError(w, 409, check.Reason)
		return
	}
	identity := check.Identity
	confirmedIP := false
	for _, ip := range append(append([]string{}, identity.InternalIPs...), identity.ExternalIPs...) {
		confirmedIP = confirmedIP || ip == input.ConfirmedIP
	}
	if identity.Name != input.NodeName || identity.UID != input.NodeUID || identity.ResourceVersion != input.ResourceVersion || !confirmedIP {
		writeError(w, 409, "node_identity_changed")
		return
	}
	id, e := newEnrollmentID()
	if e != nil {
		writeBackend(w, nil, e)
		return
	}
	spec := retirementSpec{Region: input.Region, NodeName: identity.Name, NodeUID: identity.UID, InternalIPs: identity.InternalIPs, ExternalIPs: identity.ExternalIPs, ConfirmedNodeName: input.ConfirmedNodeName, ConfirmedIP: input.ConfirmedIP, PermanentRetirement: true}
	request := map[string]any{"apiVersion": enrollmentAPIVersion, "kind": "NodeRetirement", "metadata": map[string]string{"name": "retire-" + id, "namespace": enrollmentNamespace}, "spec": spec}
	var value retirementResource
	if e = c.request(ctx, http.MethodPost, enrollmentAPIPath+"/noderetirements", request, &value); e != nil {
		writeBackend(w, nil, e)
		return
	}
	if e = c.verifyRetirement(value, id); e != nil {
		writeBackend(w, nil, e)
		return
	}
	writeJSON(w, 202, projectedRetirement(&value, id))
}

// Fresh reads bypass Snapshot's UI cache and use only the observer's read-only credential.
// The request service account never acquires permission to read/write Nodes or Pods.
func (d *DataSource) CheckNodeRetirement(ctx context.Context, region, name, cluster string) (retirementCheck, error) {
	result := retirementCheck{Enabled: true, Region: region, Identity: retirementIdentity{InternalIPs: []string{}, ExternalIPs: []string{}}}
	reject := func(code string) (retirementCheck, error) { result.Reason = code; return result, nil }
	var source *regionSource
	for i := range d.regions {
		if d.regions[i].cfg.Name == region {
			source = &d.regions[i]
			break
		}
	}
	if source == nil || !enrollmentLabel.MatchString(name) || strings.Contains(name, "..") {
		return result, sourceError{400, "invalid_query"}
	}
	var node struct {
		Metadata struct {
			Name              string            `json:"name"`
			UID               string            `json:"uid"`
			ResourceVersion   string            `json:"resourceVersion"`
			Labels            map[string]string `json:"labels"`
			DeletionTimestamp string            `json:"deletionTimestamp"`
		} `json:"metadata"`
		Status struct {
			Conditions []condition `json:"conditions"`
			Addresses  []struct {
				Type    string `json:"type"`
				Address string `json:"address"`
			} `json:"addresses"`
		} `json:"status"`
	}
	if e := source.up.get(ctx, "/api/v1/nodes/"+name, &node); e != nil {
		return result, e
	}
	if node.Metadata.Name != name || !enrollmentUID.MatchString(node.Metadata.UID) || !enrollmentVersion.MatchString(node.Metadata.ResourceVersion) || node.Metadata.DeletionTimestamp != "" {
		return reject("node_identity_unavailable")
	}
	result.Identity.Name = name
	result.Identity.UID = node.Metadata.UID
	result.Identity.ResourceVersion = node.Metadata.ResourceVersion
	ips, internal, external := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, a := range node.Status.Addresses {
		p := net.ParseIP(a.Address)
		if p == nil || p.String() != a.Address {
			continue
		}
		if a.Type == "InternalIP" && !internal[a.Address] {
			result.Identity.InternalIPs = append(result.Identity.InternalIPs, a.Address)
			internal[a.Address] = true
			ips[a.Address] = true
		}
		if a.Type == "ExternalIP" && !external[a.Address] {
			result.Identity.ExternalIPs = append(result.Identity.ExternalIPs, a.Address)
			external[a.Address] = true
			ips[a.Address] = true
		}
	}
	sort.Strings(result.Identity.InternalIPs)
	sort.Strings(result.Identity.ExternalIPs)
	if len(ips) == 0 || len(internal) > 8 || len(external) > 8 {
		return reject("node_identity_unavailable")
	}
	labels := node.Metadata.Labels
	if _, yes := labels["node-role.kubernetes.io/control-plane"]; yes {
		return reject("control_node_protected")
	}
	if _, yes := labels["node-role.kubernetes.io/master"]; yes {
		return reject("control_node_protected")
	}
	if labels["nakama-agones.io/game-node"] != "true" || labels["nakama-agones.io/role"] != "game" || labels["nakama-agones.io/cluster"] != cluster {
		return reject("node_not_owned_worker")
	}
	ready, readyCount := "", 0
	for _, c := range node.Status.Conditions {
		if c.Type == "Ready" {
			ready = c.Status
			readyCount++
		}
	}
	if ready == "True" {
		return reject("node_still_ready")
	}
	if readyCount != 1 || (ready != "False" && ready != "Unknown") {
		return reject("node_readiness_unknown")
	}
	var pods struct {
		Metadata struct {
			Continue string `json:"continue"`
		} `json:"metadata"`
		Items []podObject `json:"items"`
	}
	if e := source.up.get(ctx, itemsPath(source.cfg.Namespace, "pods")+"?fieldSelector="+url.QueryEscape("spec.nodeName="+name)+"&limit=1000", &pods); e != nil {
		return result, e
	}
	if pods.Metadata.Continue != "" {
		return reject("node_inventory_incomplete")
	}
	for _, p := range pods.Items {
		if p.Spec.NodeName == name {
			return reject("node_has_pods")
		}
	}
	var servers struct {
		Metadata struct {
			Continue string `json:"continue"`
		} `json:"metadata"`
		Items []struct {
			Status struct {
				NodeName string `json:"nodeName"`
			} `json:"status"`
		} `json:"items"`
	}
	if e := source.up.get(ctx, "/apis/agones.dev/v1/namespaces/"+source.cfg.Namespace+"/gameservers?limit=1000", &servers); e != nil {
		return result, e
	}
	if servers.Metadata.Continue != "" {
		return reject("node_inventory_incomplete")
	}
	for _, gs := range servers.Items {
		if gs.Status.NodeName == name {
			return reject("node_has_gameservers")
		}
	}
	if d.cfg.FleetSnapshotFile == "" {
		return reject("fleet_snapshot_required")
	}
	fleet, e := readFleetSnapshot(d.cfg.FleetSnapshotFile)
	if e != nil {
		return result, e
	}
	workers, ok := fleet["workers"].([]any)
	if !ok {
		return reject("fleet_snapshot_invalid")
	}
	rooms, ok := fleet["rooms"].([]any)
	if !ok {
		return reject("fleet_snapshot_invalid")
	}
	attached := map[string]bool{}
	for _, raw := range workers {
		worker, ok := raw.(map[string]any)
		if !ok {
			return reject("fleet_snapshot_invalid")
		}
		if textField(worker, "region") != region {
			continue
		}
		state := textField(worker, "state")
		terminal := state == "stopped" || state == "lost" || state == "failed"
		host := textField(worker, "host")
		if ips[host] || textField(worker, "node") == name {
			attached[textField(worker, "id")] = true
			if !terminal {
				return reject("node_has_processing_worker")
			}
		} else if !terminal && net.ParseIP(host) == nil {
			return reject("worker_location_unavailable")
		}
	}
	for _, raw := range rooms {
		room, ok := raw.(map[string]any)
		if !ok {
			return reject("fleet_snapshot_invalid")
		}
		if !attached[textField(room, "worker_id")] {
			continue
		}
		state := textField(room, "state")
		if state != "completed" && state != "cancelled" && state != "failed" && state != "expired" {
			return reject("node_has_active_allocations")
		}
	}
	result.Eligible = true
	result.Reason = "eligible_permanent_retirement_requires_confirmation"
	return result, nil
}
