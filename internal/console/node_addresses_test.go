package console

import (
	"strings"
	"testing"
)

func TestVersionedNodeAddressesMatchBrowserProjection(t *testing.T) {
	s, b, token := queryFixture(t)
	snapshot := b.value.(map[string]any)
	region := snapshot["regions"].([]any)[0].(map[string]any)
	node := region["nodes"].([]any)[0].(map[string]any)
	node["internal_ips"] = []string{"10.4.0.5"}
	node["external_ips"] = []string{"8.8.8.8"}
	node["operator_public_ip"] = "8.8.4.4"
	node["ready_status"] = "Unknown"
	node["ready_last_transition_at"] = "2026-09-22T00:00:00Z"
	node["unreviewed_private_field"] = "DO_NOT_DISCLOSE"
	w := perform(s, apiRequest(token, "nodes?name=game-1"))
	if w.Code != 200 || strings.Contains(w.Body.String(), "DO_NOT_DISCLOSE") {
		t.Fatal("unsafe node response")
	}
	value := responseObject(t, w.Body.Bytes())
	row := value["data"].([]any)[0].(map[string]any)
	for _, key := range []string{"internal_ips", "external_ips", "operator_public_ip", "ready_status", "ready_last_transition_at"} {
		if _, ok := row[key]; !ok {
			t.Fatal("machine API omitted browser field: " + key)
		}
	}
}
