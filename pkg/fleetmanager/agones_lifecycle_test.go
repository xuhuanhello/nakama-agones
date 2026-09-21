package fleetmanager

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/xuhuanhello/nakama-agones/internal/provider"
	"github.com/xuhuanhello/nakama-agones/internal/state"
)

func TestAgonesAllocatedAndBusinessReadyAreBothRequired(t *testing.T) {
	f := setup(t)
	wid := f.readyWorker()
	f.update(func(s *state.State) { s.Workers[wid].NextCheckAt = f.clock.Load() })
	pid := f.snapshot().Workers[wid].ProviderID
	i := f.provider.instances[pid]
	i.Status = "launching"
	f.provider.instances[pid] = i
	f.tick()
	f.heartbeat(wid, nil, nil, state.Metrics{})
	if f.m.eligible(f.snapshot().Workers[wid], f.clock.Load()) {
		t.Fatal("business heartbeat overrode provider not-Allocated")
	}
	i.Status = "running"
	f.provider.instances[pid] = i
	f.update(func(s *state.State) { s.Workers[wid].NextCheckAt = f.clock.Load(); s.Workers[wid].Ready = false })
	f.tick()
	if f.m.eligible(f.snapshot().Workers[wid], f.clock.Load()) {
		t.Fatal("Allocated overrode business not-ready")
	}
	f.heartbeat(wid, nil, nil, state.Metrics{})
	if !f.m.eligible(f.snapshot().Workers[wid], f.clock.Load()) {
		t.Fatal("healthy allocated host with fresh business readiness is not eligible")
	}
	if f.snapshot().Workers[wid].ProviderExpiresAt != 0 {
		t.Fatal("absence of provider TTL became a finite expiry")
	}
}

func TestAgonesBootstrapFirstUseExpiresButSameBootCanRetry(t *testing.T) {
	f := setup(t)
	wid := f.readyWorker()
	f.clock.Add(f.m.cfg.LaunchTimeout + 1)
	status, _ := f.request("/agones/fleet/v1/agent/bootstrap", "", BootstrapRequest{WorkerID: wid, BootID: "boot-" + wid, BuildHash: f.m.cfg.BuildHash, BootstrapToken: encoded(derive(f.m.cfg.SigningKey, "bootstrap:"+wid))})
	if status != http.StatusOK {
		t.Fatal("same boot retry was rejected")
	}
	f.update(func(s *state.State) { s.Workers[wid].BootID = "" })
	status, _ = f.request("/agones/fleet/v1/agent/bootstrap", "", BootstrapRequest{WorkerID: wid, BootID: "new-late-process", BuildHash: f.m.cfg.BuildHash, BootstrapToken: encoded(derive(f.m.cfg.SigningKey, "bootstrap:"+wid))})
	if status != http.StatusForbidden {
		t.Fatal("expired unused bootstrap credential was accepted")
	}
}

func TestUnhealthyProviderCannotKeepAdmittingAndIsReaped(t *testing.T) {
	f := setup(t)
	wid := f.readyWorker()
	aid := f.assign(wid, "unhealthy")
	pid := f.snapshot().Workers[wid].ProviderID
	i := f.provider.instances[pid]
	i.Status = "unhealthy"
	f.provider.instances[pid] = i
	f.update(func(s *state.State) { s.Workers[wid].NextCheckAt = f.clock.Load() })
	f.tick()
	s := f.snapshot()
	if s.Workers[wid].State != "stopping" || s.Workers[wid].Ready || s.Allocations[aid].State != "failed" {
		t.Fatal("unhealthy provider did not fence rooms")
	}
	f.tick()
	if f.snapshot().Workers[wid].State != "stopped" || f.provider.stops != 1 {
		t.Fatal("unhealthy worker resource was not reaped")
	}
}

type lostCreateProvider struct {
	*fakeProvider
	first bool
}

func (p *lostCreateProvider) Start(ctx context.Context, req provider.StartRequest) (provider.Instance, error) {
	if !p.first {
		p.first = true
		return provider.Instance{}, &provider.APIError{OutcomeUnknown: true, Reason: "transport_error"}
	}
	return p.fakeProvider.Start(ctx, req)
}

func TestUnknownCreateBeforeCommitRetriesTheSameWorker(t *testing.T) {
	f := setup(t)
	p := &lostCreateProvider{fakeProvider: f.provider}
	f.m.provider = p
	f.allocate("retry")
	f.tick()
	s := f.snapshot()
	if len(s.Workers) != 1 {
		t.Fatal("missing uncertain operation")
	}
	var wid string
	for id, w := range s.Workers {
		wid = id
		if w.State != "unknown" {
			t.Fatal("uncertain create not retained")
		}
	}
	f.clock.Add(2)
	f.tick()
	s = f.snapshot()
	if len(s.Workers) != 1 || s.Workers[wid].ProviderID == "" || s.Workers[wid].State != "launching" || f.provider.starts != 1 {
		t.Fatal("retry failed to reuse deterministic identity")
	}
}

func TestProviderConfigurationChangesFenceAnExistingPool(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) {
			c.Kubernetes.GameImage = "other@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		},
		func(c *Config) { c.Kubernetes.Namespace = "other-namespace" },
		func(c *Config) { c.Kubernetes.Pool = "other-pool" },
		func(c *Config) { c.Kubernetes.NodeSelector = map[string]string{"nodepool": "other"} },
		func(c *Config) { c.Kubernetes.GamePort = 7771 },
	} {
		f := setup(t)
		cfg := f.m.Config()
		mutate(&cfg)
		if _, err := New(cfg, f.store, f.provider); err == nil {
			t.Fatal("immutable pool configuration drift was accepted")
		}
	}
}

func TestLegacyRoutesAreNotRegistered(t *testing.T) {
	f := setup(t)
	status, _ := f.request("/fleet/v1/agent/bootstrap", "", map[string]string{})
	if status != http.StatusNotFound {
		t.Fatal("Agones accidentally retained the old PlayFlow HTTP route")
	}
}

func TestDrainStopsNewAdmissionButPreservesValidReconnect(t *testing.T) {
	f := setup(t)
	wid := f.readyWorker()
	aid := f.assign(wid, "drain-resume")
	a := f.snapshot().Allocations[aid]
	f.heartbeat(wid, []RoomReport{{RoomID: a.RoomID, State: "playing", UserIDs: a.UserIDs}}, nil, state.Metrics{})
	if err := f.m.Drain(context.Background(), wid); err != nil {
		t.Fatal(err)
	}
	f.sequences[wid]++
	status, _ := f.request("/agones/fleet/v1/agent/heartbeat", encoded(derive(f.m.cfg.SigningKey, "agent:"+wid+":boot-"+wid)), HeartbeatRequest{WorkerID: wid, BootID: "boot-" + wid, Sequence: f.sequences[wid], Ready: false, Rooms: []RoomReport{{RoomID: a.RoomID, State: "playing", UserIDs: []string{"drain-resume-b"}}}})
	if status != http.StatusOK {
		t.Fatal("draining heartbeat failed")
	}
	worker := f.snapshot().Workers[wid]
	if worker.Ready || !worker.ProviderReady || !worker.Draining || f.m.eligible(worker, f.clock.Load()) {
		t.Fatal("drained worker still accepts new rooms")
	}
	assignment, err := f.m.Assignment(context.Background(), "drain-resume-a", aid, true)
	if err != nil || assignment.AdmissionToken == "" {
		t.Fatalf("valid reconnect lost while draining: %v", err)
	}
	if _, err = f.m.Assignment(context.Background(), "drain-resume-a", aid, false); !errors.Is(err, ErrBusy) {
		t.Fatal("fresh non-resume admission bypassed ready=false")
	}
	f.update(func(s *state.State) { s.Allocations[aid].Sessions[0].EverConnected = false })
	if _, err = f.m.Assignment(context.Background(), "drain-resume-a", aid, true); !errors.Is(err, ErrBusy) {
		t.Fatal("resume exception created a never-entered seat")
	}
	f.update(func(s *state.State) {
		s.Allocations[aid].Sessions[0].EverConnected = true
		s.Workers[wid].NextCheckAt = f.clock.Load()
	})
	pid := worker.ProviderID
	i := f.provider.instances[pid]
	i.Status = "launching"
	f.provider.instances[pid] = i
	f.tick()
	if _, err = f.m.Assignment(context.Background(), "drain-resume-a", aid, true); !errors.Is(err, ErrBusy) {
		t.Fatal("drain resume ignored loss of Agones Allocated state")
	}
	if f.snapshot().Workers[wid].ProviderReady {
		t.Fatal("draining worker retained stale provider readiness")
	}
}
