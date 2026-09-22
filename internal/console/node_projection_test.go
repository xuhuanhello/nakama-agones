package console

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

func TestNodeProjectionReportsAddressesAndReadyCondition(t *testing.T) {
	u, _ := testUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/nodes" {
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"game-1","labels":{"nakama-agones.io/role":"game","nakama-agones.io/game-node":"true"}},"status":{"capacity":{"cpu":"2","memory":"2Gi"},"allocatable":{"cpu":"1700m","memory":"1400Mi"},"addresses":[{"type":"InternalIP","address":"10.0.0.8"},{"type":"InternalIP","address":"2001:db8::8"},{"type":"InternalIP","address":"10.0.0.8"},{"type":"ExternalIP","address":"203.0.113.8"},{"type":"ExternalIP","address":"10.20.0.9"},{"type":"Hostname","address":"192.0.2.1"},{"type":"ExternalDNS","address":"example.test"},{"type":"InternalIP","address":"not-an-ip"}],"conditions":[{"type":"MemoryPressure","status":"False","lastTransitionTime":"2026-09-22T01:00:00Z"},{"type":"Ready","status":"Unknown","lastTransitionTime":"2026-09-22T02:00:00Z"}]}},{"metadata":{"name":"without-addresses","annotations":{"nakama-agones.io/operator-public-ip":"198.51.100.10"}},"status":{}},{"metadata":{"name":"invalid-note","annotations":{"nakama-agones.io/operator-public-ip":"https://secret.invalid"}},"status":{}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	r := regionSource{RegionConfig{Name: "us-west", Namespace: "agones-games", SystemNamespace: "agones-system"}, u}
	v := r.snapshot(context.Background()).(map[string]any)
	if v["ok"] != true {
		t.Fatal("node snapshot failed", v["error"])
	}
	nodes := v["nodes"].([]any)
	n := nodes[0].(map[string]any)
	if !reflect.DeepEqual(n["internal_ips"], []string{"10.0.0.8", "2001:db8::8"}) {
		t.Fatalf("internal addresses: %#v", n["internal_ips"])
	}
	// The declared address type wins. Do not silently reclassify a misconfigured
	// ExternalIP by subnet, infer it from the hostname, or resolve a DNS value.
	if !reflect.DeepEqual(n["external_ips"], []string{"10.20.0.9", "203.0.113.8"}) {
		t.Fatalf("external addresses: %#v", n["external_ips"])
	}
	if n["ready"] != false || n["ready_status"] != "Unknown" || n["ready_last_transition_at"] != "2026-09-22T02:00:00Z" {
		t.Fatalf("Ready condition lost: %#v", n)
	}
	if n["game_node"] != true || n["role"] != "game" || n["cpu_allocatable_millicores"] != 1700.0 {
		t.Fatal("existing capacity/role fields changed", n)
	}
	missing := nodes[1].(map[string]any)
	for _, key := range []string{"internal_ips", "external_ips"} {
		if !reflect.DeepEqual(missing[key], []string{}) {
			t.Fatalf("unreported %s must be an empty list, got %#v", key, missing[key])
		}
	}
	if missing["operator_public_ip"] != "198.51.100.10" {
		t.Fatal("validated operator note missing", missing)
	}
	if _, exists := nodes[2].(map[string]any)["operator_public_ip"]; exists {
		t.Fatal("invalid operator IP was projected")
	}
	if missing["ready_status"] != "Unknown" || missing["ready_last_transition_at"] != "" {
		t.Fatal("missing Ready condition must remain unknown", missing)
	}
}

func TestNodeReadinessPreservesFalseAndUnknown(t *testing.T) {
	for _, item := range []struct {
		name       string
		conditions []condition
		want       string
	}{
		{"ready", []condition{{Type: "Ready", Status: "True"}}, "True"},
		{"not-ready", []condition{{Type: "Ready", Status: "False"}}, "False"},
		{"unknown", []condition{{Type: "Ready", Status: "Unknown"}}, "Unknown"},
		{"missing", []condition{{Type: "MemoryPressure", Status: "False"}}, "Unknown"},
		{"invalid", []condition{{Type: "Ready", Status: "invalid"}}, "Unknown"},
	} {
		t.Run(item.name, func(t *testing.T) {
			got, _ := nodeReadiness(item.conditions)
			if got != item.want {
				t.Fatalf("got %s want %s", got, item.want)
			}
		})
	}
}
