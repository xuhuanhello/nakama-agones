package fleetmanager

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/xuhuanhello/nakama-agones/internal/state"
)

func TestPolicyCASPinsLiveAndFutureWorkersAcrossRestart(t *testing.T) {
	f := setup(t)
	oldID := f.readyWorker()
	old := f.snapshot().Workers[oldID]
	oldReq := f.m.startRequest(old)
	in := PolicyUpdate{ExpectedRevision: 1, RoomsPerInstance: 8, CPURequestMillicores: 1500, CPULimitMillicores: 1500, Actor: "admin"}
	var success atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := f.m.UpdatePolicy(context.Background(), in)
			if err == nil {
				success.Add(1)
			} else if !errors.Is(err, ErrPolicyConflict) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if success.Load() != 1 {
		t.Fatal("policy lost CAS isolation")
	}
	after := f.snapshot()
	if after.Workers[oldID].MaxRooms != old.MaxRooms || after.Workers[oldID].Draining {
		t.Fatal("policy mutated or drained live worker")
	}
	if len(after.PolicyAudit) != 1 {
		t.Fatal("policy audit duplicated")
	}
	created, err := f.m.Create(context.Background(), 16, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := f.snapshot().Workers[created["worker_id"]]
	if w.MaxRooms != 8 || w.CPURequest != "1500m" || w.CPULimit != "1500m" || w.PolicyRevision != 2 {
		t.Fatal("new worker did not pin desired template")
	}
	in.ExpectedRevision = 2
	in.CPURequestMillicores = 1000
	in.CPULimitMillicores = 1000
	in.RoomsPerInstance = 4
	if f.m.UpdatePolicy(context.Background(), in) != nil {
		t.Fatal("second policy failed")
	}
	retry := f.m.startRequest(w)
	if retry.CPUResources.Request != "1500m" || retry.EnvironmentVariables["AGONES_FLEET_MAX_ROOMS"] != "8" {
		t.Fatal("retry silently changed an already requested process")
	}
	again, err := New(f.m.cfg, f.store, f.provider)
	if err != nil {
		t.Fatal(err)
	}
	restored := f.snapshot()
	if restored.CapacityPolicy.Revision != 3 || again.startRequest(restored.Workers[oldID]).CPUResources.Request != oldReq.CPUResources.Request {
		t.Fatal("restart erased policy or changed legacy request identity")
	}
	view, err := again.Policy(context.Background())
	if err != nil || view["requires_replacement_count"].(int) != 2 {
		t.Fatal("desired/effective difference missing")
	}
}
func TestPolicySafetyAndAuthentication(t *testing.T) {
	f := setup(t)
	base := PolicyUpdate{ExpectedRevision: 1, RoomsPerInstance: 4, CPURequestMillicores: 1500, CPULimitMillicores: 1500, Actor: "admin"}
	status, _ := f.request("/agones/fleet/v1/admin/policy", "", base)
	if status != 403 {
		t.Fatal("unauthorized policy write")
	}
	for _, alter := range []func(*PolicyUpdate){func(p *PolicyUpdate) { p.CPURequestMillicores = 700 }, func(p *PolicyUpdate) { p.CPULimitMillicores = 0 }, func(p *PolicyUpdate) { p.RoomsPerInstance = 513 }, func(p *PolicyUpdate) { p.Actor = "admin\nforged" }} {
		p := base
		alter(&p)
		if !errors.Is(f.m.UpdatePolicy(context.Background(), p), ErrPolicyInvalid) {
			t.Fatal("unsafe policy accepted")
		}
	}
	status, _ = f.request("/agones/fleet/v1/admin/policy", f.m.cfg.AdminToken, base)
	if status != 200 {
		t.Fatalf("authorized update status=%d", status)
	}
	status, _ = f.request("/agones/fleet/v1/admin/policy", f.m.cfg.AdminToken, base)
	if status != 409 {
		t.Fatal("stale expected revision accepted")
	}
}
func TestTimingWindowsCannotClaimMissingAsZeroOrInvalidQuantiles(t *testing.T) {
	f := func(n float64) *float64 { return &n }
	valid := state.LatencyWindow{WindowSeconds: 60, Count: 5, P50: f(30), P95: f(1000), P99: f(1200), Max: f(1400), LastSampleAgeSeconds: f(2)}
	m := state.Metrics{ClientPresentationToReadyMS: &valid}
	if !validMetrics(m) {
		t.Fatal("valid timing rejected")
	}
	for _, alter := range []func(*state.LatencyWindow){func(w *state.LatencyWindow) { w.Count = 0 }, func(w *state.LatencyWindow) { w.P95 = nil }, func(w *state.LatencyWindow) { w.P99 = f(1) }, func(w *state.LatencyWindow) { w.Max = f(math.Inf(1)) }, func(w *state.LatencyWindow) { w.LastSampleAgeSeconds = f(-1) }, func(w *state.LatencyWindow) { w.LastSampleAgeSeconds = f(61) }, func(w *state.LatencyWindow) { w.WindowSeconds = 0 }} {
		copy := valid
		alter(&copy)
		m.ClientPresentationToReadyMS = &copy
		if validMetrics(m) {
			t.Fatal("invalid timing accepted")
		}
	}
	m.ClientPresentationToReadyMS = &state.LatencyWindow{WindowSeconds: 60}
	if !validMetrics(m) {
		t.Fatal("explicit no-sample window rejected")
	}
	workers := 9
	m.SimulationWorkers = &workers
	if validMetrics(m) {
		t.Fatal("invalid worker count accepted")
	}
}

func TestLegacyPolicyMigrationPreservesImmutableProfileAndExactCPU(t *testing.T) {
	cfg := testConfig()
	cfg.Kubernetes.CPURequest = "0.5"
	cfg.Kubernetes.CPULimit = "1.5"
	store := state.NewMemory()
	provider := &fakeProvider{}
	manager, err := New(cfg, store, provider)
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Create(context.Background(), 4, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := store.View(context.Background())
	profile := before.Profile
	id := result["worker_id"]
	if err := store.Update(context.Background(), func(s *state.State) error {
		s.CapacityPolicy = nil
		s.PolicyAudit = nil
		w := s.Workers[id]
		w.PolicyRevision = 0
		w.CPURequest = ""
		w.CPULimit = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	manager, err = New(cfg, store, provider)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := store.View(context.Background())
	if after.Profile != profile || after.CapacityPolicy.Revision != 1 || after.CapacityPolicy.CPURequestMillicores != 500 || after.CapacityPolicy.CPULimitMillicores != 1500 {
		t.Fatal("legacy migration altered profile or desired values")
	}
	request := manager.startRequest(after.Workers[id])
	if request.CPUResources.Request != "0.5" || request.CPUResources.Limit != "1.5" {
		t.Fatal("migration changed exact provider request fingerprint")
	}
}
