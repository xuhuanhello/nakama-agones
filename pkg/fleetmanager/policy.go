package fleetmanager

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/xuhuanhello/nakama-agones/internal/state"
)

var ErrPolicyConflict = errors.New("policy_conflict")
var ErrPolicyInvalid = errors.New("policy_invalid")

type PolicyUpdate struct {
	ExpectedRevision     int64  `json:"expected_revision"`
	RoomsPerInstance     int    `json:"rooms_per_instance"`
	CPURequestMillicores int    `json:"cpu_request_millicores"`
	CPULimitMillicores   int    `json:"cpu_limit_millicores"`
	Actor                string `json:"actor"`
}

func (p PolicyUpdate) Validate() error {
	if p.ExpectedRevision < 1 || p.RoomsPerInstance < 1 || p.RoomsPerInstance > 512 || p.CPURequestMillicores < 100 || p.CPURequestMillicores > 64000 || p.CPURequestMillicores != p.CPULimitMillicores || len(p.Actor) < 1 || len(p.Actor) > 64 {
		return ErrPolicyInvalid
	}
	for _, c := range p.Actor {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._@-", c)) {
			return ErrPolicyInvalid
		}
	}
	return nil
}
func cpuMilli(raw string) int {
	if raw == "" {
		return 0
	}
	multiplier := 1000.0
	if strings.HasSuffix(raw, "m") {
		raw = strings.TrimSuffix(raw, "m")
		multiplier = 1
	}
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n*multiplier > 1e9 {
		return 0
	}
	return int(math.Ceil(n * multiplier))
}
func (m *Manager) policy(s *state.State) state.CapacityPolicy {
	if s.CapacityPolicy != nil {
		return *s.CapacityPolicy
	}
	return state.CapacityPolicy{Revision: 1, RoomsPerInstance: m.cfg.MaxRooms, CPURequestMillicores: cpuMilli(m.cfg.Kubernetes.CPURequest), CPULimitMillicores: cpuMilli(m.cfg.Kubernetes.CPULimit), UpdatedBy: "deployment"}
}
func (m *Manager) Policy(ctx context.Context) (map[string]any, error) {
	s, err := m.store.View(ctx)
	if err != nil {
		return nil, err
	}
	p := m.policy(s)
	instances := []map[string]any{}
	replacement := 0
	for _, w := range s.Workers {
		if !state.LiveWorker(w.State) {
			continue
		}
		diff := w.MaxRooms != p.RoomsPerInstance || cpuMilli(w.CPURequest) != p.CPURequestMillicores || cpuMilli(w.CPULimit) != p.CPULimitMillicores
		if diff {
			replacement++
		}
		instances = append(instances, map[string]any{"worker_id": w.ID, "state": w.State, "rooms_per_instance": w.MaxRooms, "cpu_request_millicores": cpuMilli(w.CPURequest), "cpu_limit_millicores": cpuMilli(w.CPULimit), "policy_revision": w.PolicyRevision, "requires_replacement": diff})
	}
	sort.Slice(instances, func(i, j int) bool { return instances[i]["worker_id"].(string) < instances[j]["worker_id"].(string) })
	return map[string]any{"api_version": "v1", "can_manage": true, "revision": p.Revision, "desired": p, "effective_instances": instances, "requires_replacement_count": replacement, "apply_mode": "future_instances", "max_instances": m.cfg.MaxInstances, "min_instances": m.cfg.MinInstances, "memory_request": m.cfg.Kubernetes.MemoryRequest, "memory_limit": m.cfg.Kubernetes.MemoryLimit, "node_selector": m.cfg.Kubernetes.NodeSelector, "audit": s.PolicyAudit}, nil
}
func (m *Manager) UpdatePolicy(ctx context.Context, in PolicyUpdate) error {
	if err := in.Validate(); err != nil {
		return err
	}
	return m.store.Update(ctx, func(s *state.State) error {
		old := m.policy(s)
		if old.Revision != in.ExpectedRevision {
			return ErrPolicyConflict
		}
		p := state.CapacityPolicy{Revision: old.Revision + 1, RoomsPerInstance: in.RoomsPerInstance, CPURequestMillicores: in.CPURequestMillicores, CPULimitMillicores: in.CPULimitMillicores, UpdatedAt: m.now().Unix(), UpdatedBy: in.Actor}
		s.CapacityPolicy = &p
		s.PolicyAudit = append(s.PolicyAudit, state.PolicyAudit{At: p.UpdatedAt, Actor: in.Actor, Before: old, After: p})
		if len(s.PolicyAudit) > 100 {
			s.PolicyAudit = s.PolicyAudit[len(s.PolicyAudit)-100:]
		}
		return nil
	})
}
func (m *Manager) policyHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var in PolicyUpdate
		if decodeBody(w, r, &in) != nil {
			writeJSON(w, 400, map[string]string{"error": "policy_invalid"})
			return
		}
		if err := m.UpdatePolicy(r.Context(), in); err != nil {
			switch {
			case errors.Is(err, ErrPolicyConflict):
				writeJSON(w, 409, map[string]string{"error": "policy_conflict"})
			case errors.Is(err, ErrPolicyInvalid):
				writeJSON(w, 400, map[string]string{"error": "policy_invalid"})
			default:
				failHTTP(w, err)
			}
			return
		}
	}
	out, err := m.Policy(r.Context())
	if err != nil {
		failHTTP(w, err)
		return
	}
	writeJSON(w, 200, out)
}
