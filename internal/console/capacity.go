package console

import "github.com/xuhuanhello/nakama-agones/internal/state"

// Node allocatable already excludes kube/system reserved resources. This view
// deliberately does not claim the remaining allocatable CPU is unreserved:
// observer RBAC covers configured namespaces, not every pod on the cluster.
func capacityProjection(fleet map[string]any, regions []any) map[string]any {
	policy := mapObject(fleet["capacity_policy"])
	request, _ := number(policy["cpu_request_millicores"])
	if p, ok := fleet["capacity_policy"].(*state.CapacityPolicy); ok && p != nil {
		request = float64(p.CPURequestMillicores)
	}
	nodes := []any{}
	shortages := 0
	known := true
	for _, raw := range regions {
		r := mapObject(raw)
		if r["ok"] != true {
			known = false
		}
		for _, p := range objects(r["pods"]) {
			if p["phase"] == "Pending" && p["scheduling_capacity_shortage"] == true {
				shortages++
			}
		}
		for _, n := range objects(r["nodes"]) {
			if n["game_node"] != true {
				continue
			}
			alloc, ok := number(n["cpu_allocatable_millicores"])
			capacity, _ := number(n["cpu_capacity_millicores"])
			fits := any(nil)
			if ok && request > 0 {
				fits = alloc > request
			}
			nodes = append(nodes, map[string]any{"region": r["name"], "name": n["name"], "ready": n["ready"], "unschedulable": n["unschedulable"], "cpu_capacity_millicores": capacity, "cpu_allocatable_millicores": alloc, "cpu_reserved_millicores": capacity - alloc, "desired_game_cpu_millicores": request, "fits_before_sidecar_and_other_pods": fits})
		}
	}
	return map[string]any{"known": known, "nodes": nodes, "pending_resource_shortage_pods": shortages, "physical_shortage_confirmed": known && shortages > 0, "scope": "configured_namespaces", "availability": "scheduler_decides", "note": "CPU request equals limit for new policies. Allocatable excludes node reservations; Agones sidecar and all other pod requests still consume capacity. This console does not claim complete cluster pod coverage."}
}
