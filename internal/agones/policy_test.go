package agones

import (
	"context"
	"github.com/xuhuanhello/nakama-agones/internal/provider"
	"testing"
)

func TestPerWorkerCPUIsAppliedAndCannotChangeOnRetry(t *testing.T) {
	f := newFixture(t)
	req := requestFor(11)
	req.CPUResources = &provider.CPUResources{Request: "1500m", Limit: "1500m"}
	if _, err := f.client.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	game := f.games[req.Name]
	f.mu.Unlock()
	resources := game.Spec.Template.Spec.Containers[0].Resources
	if resources.Requests["cpu"] != "1500m" || resources.Limits["cpu"] != "1500m" || resources.Requests["memory"] != "256Mi" {
		t.Fatal("per-worker resource policy not reflected")
	}
	if _, err := f.client.Start(context.Background(), req); err != nil {
		t.Fatal("idempotent retry failed")
	}
	req.CPUResources = &provider.CPUResources{Request: "1000m", Limit: "1000m"}
	if _, err := f.client.Start(context.Background(), req); err == nil {
		t.Fatal("same worker silently adopted changed CPU template")
	}
	if f.gameCreates != 1 {
		t.Fatal("policy retry spawned duplicate game")
	}
}
