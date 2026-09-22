package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type retirementFixtureBackend struct {
	fakeBackend
	check  retirementCheck
	checks int
}

func (b *retirementFixtureBackend) CheckNodeRetirement(context.Context, string, string, string) (retirementCheck, error) {
	b.checks++
	return b.check, nil
}
func retirementPost(s *Server, cookie *http.Cookie, csrf, body string) *httptest.ResponseRecorder {
	r := request("POST", apiPrefix+"node-retirement", body)
	r.AddCookie(cookie)
	r.Header.Set("X-CSRF-Token", csrf)
	return perform(s, r)
}
func validRetirementInput() map[string]any {
	return map[string]any{"region": "test", "node_name": "game-1", "node_uid": "node-fixture-uid", "resource_version": "22", "confirmed_node_name": "game-1", "confirmed_ip": "8.8.8.8", "permanent_retirement": true}
}
func validRetirementCheck() retirementCheck {
	return retirementCheck{Enabled: true, Eligible: true, Region: "test", Identity: retirementIdentity{Name: "game-1", UID: "node-fixture-uid", ResourceVersion: "22", InternalIPs: []string{"10.4.0.5"}, ExternalIPs: []string{"8.8.8.8"}}}
}
func TestRetirementTypedConfirmationAndFreshIdentity(t *testing.T) {
	for _, mode := range []string{"valid", "no permanent confirmation", "name mismatch", "wrong IP", "UID changed", "revision changed", "node became ready", "no csrf", "read token cannot write"} {
		t.Run(mode, func(t *testing.T) {
			f := newKubeEnrollmentFixture(t)
			s, _ := newTestServer(t)
			s.enrollment = f.client
			b := &retirementFixtureBackend{check: validRetirementCheck()}
			s.backend = b
			cookie, csrf := authenticate(t, s)
			input := validRetirementInput()
			switch mode {
			case "no permanent confirmation":
				input["permanent_retirement"] = false
			case "name mismatch":
				input["confirmed_node_name"] = "other"
			case "wrong IP":
				input["confirmed_ip"] = "8.8.4.4"
			case "UID changed":
				b.check.Identity.UID = "replacement-node-uid"
			case "revision changed":
				b.check.Identity.ResourceVersion = "23"
			case "node became ready":
				b.check.Eligible = false
				b.check.Reason = "node_still_ready"
			case "no csrf":
				csrf = ""
			}
			raw, _ := json.Marshal(input)
			var w *httptest.ResponseRecorder
			if mode == "read token cannot write" {
				token, hash, _ := GenerateReadAPIToken()
				s.cfg.ReadAPITokenHash = hash
				r := request("POST", apiPrefix+"node-retirement", string(raw))
				r.AddCookie(cookie)
				r.Header.Set("X-CSRF-Token", csrf)
				r.Header.Set("Authorization", "Bearer "+token)
				w = perform(s, r)
			} else {
				w = retirementPost(s, cookie, csrf, string(raw))
			}
			if mode == "valid" {
				if w.Code != 202 || f.retirementCreates != 1 || b.checks != 1 {
					t.Fatalf("valid request did not freshly check and submit: %d", w.Code)
				}
				var job enrollmentJob
				json.Unmarshal(w.Body.Bytes(), &job)
				obj := f.retirements[job.ID]
				if job.State != "queued" || obj.Spec.NodeUID != "node-fixture-uid" || obj.Spec.InternalIPs[0] != "10.4.0.5" || obj.Spec.ConfirmedIP != "8.8.8.8" {
					t.Fatal("precise identity lost")
				}
			} else if w.Code < 400 || f.retirementCreates != 0 {
				t.Fatalf("unconfirmed/stale request submitted: %d", w.Code)
			}
		})
	}
}
func TestNodeJobsReadTokenScopeAndSafeRetirementStatus(t *testing.T) {
	f := newKubeEnrollmentFixture(t)
	s, _ := newTestServer(t)
	s.enrollment = f.client
	b := &retirementFixtureBackend{check: validRetirementCheck()}
	s.backend = b
	cookie, csrf := authenticate(t, s)
	raw, _ := json.Marshal(validRetirementInput())
	w := retirementPost(s, cookie, csrf, string(raw))
	var created enrollmentJob
	json.Unmarshal(w.Body.Bytes(), &created)
	value := f.retirements[created.ID]
	value.Status.ObservedGeneration = value.Metadata.Generation
	value.Status.Phase = "Blocked"
	value.Status.Error = "node_has_remaining_workloads"
	value.Status.Blockers = []retirementBlocker{{"Pod", "agones-games", "game-pod", "remaining_workload"}, {"Secret", "default", "password=DO_NOT_DISCLOSE", "password=DO_NOT_DISCLOSE"}}
	value.Status.AllowedSystemPods = []retirementBlocker{{"Pod", "kube-system", "system-agent", "system_daemonset_no_force_cleanup"}}
	token, hash, _ := GenerateReadAPIToken()
	s.cfg.ReadAPITokenHash = hash
	for _, path := range []string{"node-onboarding", "node-retirement/jobs", "node-retirement/jobs?id=" + created.ID} {
		r := request("GET", apiPrefix+path, "")
		r.Header.Set("Authorization", "Bearer "+token)
		w := perform(s, r)
		if w.Code != 200 || strings.Contains(w.Body.String(), "DO_NOT_DISCLOSE") || strings.Contains(w.Body.String(), enrollmentSubject) {
			t.Fatalf("read query denied or unprojected: %d", w.Code)
		}
		if strings.Contains(path, "?id=") {
			var got enrollmentJob
			json.Unmarshal(w.Body.Bytes(), &got)
			if got.State != "blocked" || len(got.Blockers) != 1 || len(got.AllowedSystemPods) != 1 {
				t.Fatal("safe diagnostics missing")
			}
		}
	}
	r := request("GET", apiPrefix+"node-retirement/jobs?id="+created.ID, "")
	r.AddCookie(cookie)
	r.Header.Set("Authorization", "Bearer invalid")
	if perform(s, r).Code != 401 {
		t.Fatal("invalid token fell back to session")
	}
	before := f.retirementCreates
	for _, path := range []string{"node-onboarding/scan", "node-onboarding/preflight", "node-onboarding/join", "node-retirement"} {
		r := request("POST", apiPrefix+path, "{}")
		r.AddCookie(cookie)
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Authorization", "Bearer "+token)
		if perform(s, r).Code != 403 {
			t.Fatal("machine token granted mutation")
		}
	}
	if f.retirementCreates != before {
		t.Fatal("machine request created task")
	}
	value.Status.Phase = "password=DO_NOT_DISCLOSE"
	projection, _ := json.Marshal(projectedRetirement(value, created.ID))
	if strings.Contains(string(projection), "DO_NOT_DISCLOSE") {
		t.Fatal("unrecognized phase leaked")
	}
}
func TestNodeRetirementFreshReadGuards(t *testing.T) {
	for _, mode := range []string{"eligible", "ready", "unknown readiness", "control", "not owned", "missing role", "wrong cluster", "missing identity", "no IP", "deleting", "game pod", "game server", "incomplete pods", "incomplete games", "active worker", "active allocation", "unlocated worker", "stale fleet", "invalid fleet", "no fleet snapshot", "UI cache ignored"} {
		t.Run(mode, func(t *testing.T) {
			metadata := map[string]any{"name": "game-1", "uid": "node-fixture-uid", "resourceVersion": "22", "labels": map[string]string{"nakama-agones.io/game-node": "true", "nakama-agones.io/role": "game", "nakama-agones.io/cluster": "test-cluster"}}
			labels := metadata["labels"].(map[string]string)
			state := map[string]any{"conditions": []any{map[string]string{"type": "Ready", "status": "Unknown"}}, "addresses": []any{map[string]string{"type": "InternalIP", "address": "10.4.0.5"}, map[string]string{"type": "ExternalIP", "address": "8.8.8.8"}}}
			pods := map[string]any{"items": []any{}}
			games := map[string]any{"items": []any{}}
			fleet := map[string]any{"ok": true, "workers": []any{}, "rooms": []any{}}
			switch mode {
			case "ready", "UI cache ignored":
				state["conditions"] = []any{map[string]string{"type": "Ready", "status": "True"}}
			case "unknown readiness":
				state["conditions"] = []any{}
			case "control":
				labels["node-role.kubernetes.io/control-plane"] = ""
			case "not owned":
				delete(labels, "nakama-agones.io/game-node")
			case "missing role":
				delete(labels, "nakama-agones.io/role")
			case "wrong cluster":
				labels["nakama-agones.io/cluster"] = "other"
			case "missing identity":
				delete(metadata, "uid")
			case "no IP":
				state["addresses"] = []any{}
			case "deleting":
				metadata["deletionTimestamp"] = time.Now().UTC().Format(time.RFC3339)
			case "game pod":
				pods["items"] = []any{map[string]any{"spec": map[string]string{"nodeName": "game-1"}}}
			case "game server":
				games["items"] = []any{map[string]any{"status": map[string]string{"nodeName": "game-1"}}}
			case "incomplete pods":
				pods["metadata"] = map[string]string{"continue": "next"}
			case "incomplete games":
				games["metadata"] = map[string]string{"continue": "next"}
			case "active worker":
				fleet["workers"] = []any{map[string]string{"id": "worker-one", "region": "test", "host": "8.8.8.8", "state": "draining"}}
			case "active allocation":
				fleet["workers"] = []any{map[string]string{"id": "worker-one", "region": "test", "host": "8.8.8.8", "state": "lost"}}
				fleet["rooms"] = []any{map[string]string{"worker_id": "worker-one", "state": "active"}}
			case "unlocated worker":
				fleet["workers"] = []any{map[string]string{"id": "worker-one", "region": "test", "state": "starting"}}
			case "invalid fleet":
				delete(fleet, "rooms")
			}
			calls := 0
			up, _ := testUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" {
					t.Error("observer attempted mutation")
				}
				switch r.URL.Path {
				case "/api/v1/nodes/game-1":
					json.NewEncoder(w).Encode(map[string]any{"metadata": metadata, "status": state})
				case "/api/v1/namespaces/agones-games/pods":
					if r.URL.Query().Get("fieldSelector") != "spec.nodeName=game-1" {
						t.Error("pod query not scoped to node")
					}
					json.NewEncoder(w).Encode(pods)
				case "/apis/agones.dev/v1/namespaces/agones-games/gameservers":
					json.NewEncoder(w).Encode(games)
				default:
					t.Error("unexpected read path")
					w.WriteHeader(404)
				}
			})
			raw, _ := json.Marshal(fleet)
			path := filepath.Join(t.TempDir(), "fleet.json")
			if err := os.WriteFile(path, raw, 0640); err != nil {
				t.Fatal(err)
			}
			if mode == "stale fleet" {
				old := time.Now().Add(-21 * time.Second)
				os.Chtimes(path, old, old)
			}
			if mode == "no fleet snapshot" {
				path = ""
			}
			d := &DataSource{cfg: SourceConfig{FleetSnapshotFile: path}, regions: []regionSource{{cfg: RegionConfig{Name: "test", Namespace: "agones-games"}, up: up}}}
			if mode == "UI cache ignored" {
				d.cachedAt = time.Now()
				d.cached = map[string]any{"regions": []any{map[string]any{"name": "test", "nodes": []any{map[string]any{"name": "game-1", "ready": false}}}}}
			}
			check, err := d.CheckNodeRetirement(context.Background(), "test", "game-1", "test-cluster")
			if mode == "eligible" {
				if err != nil || !check.Eligible || calls != 3 || len(check.Identity.InternalIPs) != 1 || check.Identity.ResourceVersion != "22" {
					t.Fatalf("eligible read failed: %#v %v", check, err)
				}
			} else if err == nil && check.Eligible {
				t.Fatal("unsafe retirement deemed eligible: " + mode)
			}
			if mode == "stale fleet" && sourceCode(err) != "fleet_snapshot_stale" {
				t.Fatal("stale snapshot did not block")
			}
		})
	}
}
