package console

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/xuhuanhello/nakama-agones/internal/state"
)

type PolicyInput struct {
	ExpectedRevision     int64 `json:"expected_revision"`
	RoomsPerInstance     int   `json:"rooms_per_instance"`
	CPURequestMillicores int   `json:"cpu_request_millicores"`
	CPULimitMillicores   int   `json:"cpu_limit_millicores"`
}

func (p PolicyInput) valid() bool {
	return p.ExpectedRevision >= 1 && p.RoomsPerInstance >= 1 && p.RoomsPerInstance <= 512 && p.CPURequestMillicores >= 100 && p.CPURequestMillicores <= 64000 && p.CPURequestMillicores == p.CPULimitMillicores
}

type policyCommand struct {
	PolicyInput
	Actor string `json:"actor"`
}
type EffectiveInstance struct {
	WorkerID             string `json:"worker_id"`
	State                string `json:"state"`
	RoomsPerInstance     int    `json:"rooms_per_instance"`
	CPURequestMillicores int    `json:"cpu_request_millicores"`
	CPULimitMillicores   int    `json:"cpu_limit_millicores"`
	PolicyRevision       int64  `json:"policy_revision"`
	RequiresReplacement  bool   `json:"requires_replacement"`
}
type PolicyView struct {
	APIVersion               string               `json:"api_version"`
	CanManage                bool                 `json:"can_manage"`
	ReadOnly                 bool                 `json:"read_only"`
	Revision                 int64                `json:"revision"`
	Desired                  state.CapacityPolicy `json:"desired"`
	EffectiveInstances       []EffectiveInstance  `json:"effective_instances"`
	RequiresReplacementCount int                  `json:"requires_replacement_count"`
	ApplyMode                string               `json:"apply_mode"`
	MaxInstances             int                  `json:"max_instances"`
	MinInstances             int                  `json:"min_instances"`
	MemoryRequest            string               `json:"memory_request"`
	MemoryLimit              string               `json:"memory_limit"`
	NodeSelector             map[string]string    `json:"node_selector"`
	Audit                    []state.PolicyAudit  `json:"audit"`
}
type policyBackend interface {
	Policy(context.Context) (any, error)
	UpdatePolicy(context.Context, PolicyInput, string) (any, error)
}

func decodePolicy(raw []byte) (PolicyView, error) {
	var p PolicyView
	if json.Unmarshal(raw, &p) != nil || p.APIVersion != "v1" || p.Revision < 1 || p.Revision != p.Desired.Revision || p.ApplyMode != "future_instances" || len(p.EffectiveInstances) > 1000 || len(p.Audit) > 100 {
		return p, sourceError{502, "control_invalid_response"}
	}
	return p, nil
}
func (s *ControlServer) policy(w http.ResponseWriter, r *http.Request) {
	var payload []byte
	if r.Method == http.MethodPost {
		var in policyCommand
		if !readJSON(w, r, &in) {
			return
		}
		if !in.PolicyInput.valid() || !validUsername(in.Actor) {
			writeError(w, 400, "policy_invalid")
			return
		}
		payload, _ = json.Marshal(in)
	} else if r.ContentLength != 0 {
		writeError(w, 400, "invalid_request")
		return
	}
	raw, err := s.upstream(r.Context(), r.Method, "/agones/fleet/v1/admin/policy", payload, 1<<20)
	if err != nil {
		writeBackend(w, nil, err)
		return
	}
	out, err := decodePolicy(raw)
	writeBackend(w, out, err)
}
func (d *DataSource) Policy(ctx context.Context) (any, error) {
	if d.control == nil {
		return nil, sourceError{503, "policy_unavailable"}
	}
	raw, err := d.control.request(ctx, http.MethodGet, "policy", nil)
	if err != nil {
		return nil, err
	}
	out, err := decodePolicy(raw)
	out.CanManage = d.cfg.AllowManagement
	return out, err
}
func (d *DataSource) UpdatePolicy(ctx context.Context, in PolicyInput, actor string) (any, error) {
	if !d.cfg.AllowManagement || d.control == nil {
		return nil, sourceError{403, "management_disabled"}
	}
	if !in.valid() {
		return nil, sourceError{400, "policy_invalid"}
	}
	raw, err := d.control.request(ctx, http.MethodPost, "policy", policyCommand{in, actor})
	if err != nil {
		return nil, err
	}
	out, err := decodePolicy(raw)
	if err == nil {
		d.invalidateSnapshot()
	}
	return out, err
}
