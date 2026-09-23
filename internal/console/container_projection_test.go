package console

import (
	"encoding/json"
	"testing"
)

func TestContainerProjectionNamesNotOrderAndMissingUsage(t *testing.T) {
	var p podObject
	if err := json.Unmarshal([]byte(`{"spec":{"containers":[{"name":"game","resources":{"requests":{"cpu":"1500m","memory":"512Mi"},"limits":{"cpu":"1500m","memory":"1Gi"}}},{"name":"sidecar"}],"initContainers":[{"name":"setup"}]}}`), &p); err != nil {
		t.Fatal(err)
	}
	var m resourceMetric
	if err := json.Unmarshal([]byte(`{"timestamp":"2026-09-24T00:00:00Z","window":"30s","containers":[{"name":"sidecar","usage":{"cpu":"1000000n","memory":"10Mi"}},{"name":"game","usage":{"cpu":"250m","memory":"100Mi"}}]}`), &m); err != nil {
		t.Fatal(err)
	}
	rows := projectContainers(p, m)
	game, side, init := rows[0].(map[string]any), rows[1].(map[string]any), rows[2].(map[string]any)
	if game["cpu_millicores"] != float64(250) || side["cpu_millicores"] != float64(1) {
		t.Fatalf("wrong name join: %#v", rows)
	}
	if game["memory_bytes"] != int64(100*1024*1024) || game["metrics_window"] != "30s" {
		t.Fatal(game)
	}
	if game["limits"].(map[string]any)["cpu_millicores"] != float64(1500) {
		t.Fatal(game)
	}
	if init["kind"] != "init" {
		t.Fatal(init)
	}
	if _, ok := init["memory_bytes"]; ok {
		t.Fatal("missing init metrics presented as zero")
	}
	empty := projectContainers(p, resourceMetric{})[0].(map[string]any)
	if _, ok := empty["cpu_millicores"]; ok {
		t.Fatal("missing metrics presented as zero")
	}
	if empty["requests"].(map[string]any)["memory_bytes"] != int64(512*1024*1024) {
		t.Fatal("spec requests lost on metrics outage")
	}
}
