package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xuhuanhello/nakama-agones/internal/state"
)

type operationBackend struct {
	fakeBackend
	mutations int
	actor     string
	policy    PolicyView
}

func (b *operationBackend) Policy(context.Context) (any, error) { return b.policy, nil }
func (b *operationBackend) UpdatePolicy(_ context.Context, p PolicyInput, actor string) (any, error) {
	if !p.valid() {
		return nil, sourceError{400, "policy_invalid"}
	}
	b.mutations++
	b.actor = actor
	return b.policy, nil
}
func (b *operationBackend) Alerts(context.Context) (any, error) {
	return map[string]any{"api_version": "v1", "can_configure": true, "revision": 1, "feishu_configured": true}, nil
}
func (b *operationBackend) UpdateAlerts(_ context.Context, p AlertUpdate, actor string) (any, error) {
	b.mutations++
	b.actor = actor
	return b.Alerts(context.Background())
}
func fixturePolicy() PolicyView {
	return PolicyView{APIVersion: "v1", Revision: 1, CanManage: true, ApplyMode: "future_instances", Desired: state.CapacityPolicy{Revision: 1, RoomsPerInstance: 2, CPURequestMillicores: 700}, EffectiveInstances: []EffectiveInstance{}, Audit: []state.PolicyAudit{}}
}

func TestPolicyAndAlertsRequireSessionOriginCSRFWrites(t *testing.T) {
	cfg := testConfig(t)
	token, hash, err := GenerateReadAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ReadAPITokenHash = hash
	b := &operationBackend{policy: fixturePolicy()}
	s, err := NewServer(cfg, b, testFiles())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cookie, csrf := authenticate(t, s)
	for _, route := range []string{"policy", "alerts"} {
		body := `{"expected_revision":1,"rooms_per_instance":4,"cpu_request_millicores":1500,"cpu_limit_millicores":1500}`
		if route == "alerts" {
			body = `{"expected_revision":1,"enabled":false,"wait_p95_ms":1000,"frame_p99_ms":100,"hold_seconds":60,"cooldown_seconds":600,"min_samples":5}`
		}
		if perform(s, request("POST", apiPrefix+route, body)).Code != 401 {
			t.Fatal("anonymous write")
		}
		r := request("POST", apiPrefix+route, body)
		r.AddCookie(cookie)
		if perform(s, r).Code != 403 {
			t.Fatal("missing CSRF accepted")
		}
		r = request("POST", apiPrefix+route, body)
		r.AddCookie(cookie)
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Origin", "http://evil.test")
		if perform(s, r).Code != 403 {
			t.Fatal("wrong origin accepted")
		}
		r = request("POST", apiPrefix+route, body)
		r.AddCookie(cookie)
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Authorization", "Bearer "+token)
		if perform(s, r).Code != 403 {
			t.Fatal("read token obtained write capability")
		}
		r = request("POST", apiPrefix+route, body)
		r.AddCookie(cookie)
		r.Header.Set("X-CSRF-Token", csrf)
		if w := perform(s, r); w.Code != 200 {
			t.Fatalf("authorized action failed %s: %d", route, w.Code)
		}
		r = request("GET", apiV1Prefix+route, "")
		r.Header.Set("Authorization", "Bearer "+token)
		w := perform(s, r)
		if w.Code != 200 {
			t.Fatal("read token failed read")
		}
		var v map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		if v["can_manage"] == true || v["can_configure"] == true {
			t.Fatal("read token exposed action capability")
		}
	}
	if b.mutations != 2 || b.actor != cfg.Username {
		t.Fatal("wrong actor or unintended mutation")
	}
	r := request("POST", apiPrefix+"policy", `{"expected_revision":1,"rooms_per_instance":4,"cpu_request_millicores":1500,"cpu_limit_millicores":1500,"actor":"root"}`)
	r.AddCookie(cookie)
	r.Header.Set("X-CSRF-Token", csrf)
	if perform(s, r).Code != 400 {
		t.Fatal("caller spoofed audit actor")
	}
}

func TestPolicyBrokerFixedRouteCASAndResponseProjection(t *testing.T) {
	response := fixturePolicy()
	calls := 0
	status := 200
	s, _ := controlFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/agones/fleet/v1/admin/policy" || r.Header.Get("Authorization") != "Bearer fixture-admin-secret" {
			t.Fatal("untrusted upstream route")
		}
		if r.Method == "POST" {
			var input policyCommand
			if json.NewDecoder(r.Body).Decode(&input) != nil || input.Actor != "operator" || input.CPURequestMillicores != 1500 {
				t.Fatal("wrong policy projection")
			}
		}
		w.WriteHeader(status)
		if status == 200 {
			raw, _ := json.Marshal(response)
			var out map[string]any
			_ = json.Unmarshal(raw, &out)
			out["private_credential"] = "must-not-leak"
			_ = json.NewEncoder(w).Encode(out)
		} else {
			io.WriteString(w, `{"secret":"must-not-leak"}`)
		}
	})
	for _, method := range []string{"GET", "POST"} {
		body := ""
		if method == "POST" {
			body = `{"expected_revision":1,"rooms_per_instance":4,"cpu_request_millicores":1500,"cpu_limit_millicores":1500,"actor":"operator"}`
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, controlRequest(method, "/v1/policy", body))
		if w.Code != 200 || strings.Contains(w.Body.String(), "must-not-leak") {
			t.Fatal("unsafe policy response")
		}
	}
	for _, body := range []string{`{"expected_revision":1,"rooms_per_instance":4,"cpu_request_millicores":700,"cpu_limit_millicores":1500,"actor":"operator"}`, `{"expected_revision":1,"rooms_per_instance":4,"cpu_request_millicores":1500,"cpu_limit_millicores":1500,"actor":"operator","path":"/secrets"}`} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, controlRequest("POST", "/v1/policy", body))
		if w.Code != 400 {
			t.Fatal("unbounded policy accepted")
		}
	}
	if calls != 2 {
		t.Fatal("invalid policy reached upstream")
	}
	status = 409
	w := httptest.NewRecorder()
	s.ServeHTTP(w, controlRequest("POST", "/v1/policy", `{"expected_revision":1,"rooms_per_instance":4,"cpu_request_millicores":1500,"cpu_limit_millicores":1500,"actor":"operator"}`))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "policy_conflict") || strings.Contains(w.Body.String(), "must-not-leak") {
		t.Fatal("unsafe CAS error")
	}
}

func TestCapacityDistinguishesAllocatableAndObservedNamespaces(t *testing.T) {
	policy := &state.CapacityPolicy{CPURequestMillicores: 1500}
	regions := []any{map[string]any{"name": "test", "ok": true, "nodes": []any{map[string]any{"name": "game-a", "game_node": true, "cpu_capacity_millicores": 2000, "cpu_allocatable_millicores": 1700}}, "pods": []any{map[string]any{"phase": "Pending", "scheduling_capacity_shortage": true}}}}
	v := capacityProjection(map[string]any{"capacity_policy": policy}, regions)
	if v["physical_shortage_confirmed"] != true || v["scope"] != "configured_namespaces" || v["availability"] != "scheduler_decides" {
		t.Fatal("scheduler evidence lost")
	}
	node := v["nodes"].([]any)[0].(map[string]any)
	if node["desired_game_cpu_millicores"] != float64(1500) || node["cpu_reserved_millicores"] != float64(300) {
		t.Fatal("CPU reservation projection wrong")
	}
}
