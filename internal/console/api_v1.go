package console

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/xuhuanhello/nakama-agones/internal/state"
)

const apiV1Prefix = apiPrefix + "v1/"

type apiWarning struct {
	Source string `json:"source"`
	Code   string `json:"code"`
}
type apiPage struct {
	Offset     int  `json:"offset"`
	Limit      int  `json:"limit"`
	Returned   int  `json:"returned"`
	Total      int  `json:"total"`
	NextOffset *int `json:"next_offset"`
}
type apiResponse struct {
	APIVersion string       `json:"api_version"`
	ObservedAt string       `json:"observed_at"`
	ReadOnly   bool         `json:"read_only"`
	Partial    bool         `json:"partial"`
	Warnings   []apiWarning `json:"warnings"`
	Data       any          `json:"data"`
	Page       *apiPage     `json:"page,omitempty"`
}

func (s *Server) versionedAPI(w http.ResponseWriter, r *http.Request) {
	resource := strings.TrimPrefix(r.URL.Path, apiV1Prefix)
	switch resource {
	case "overview", "rooms", "instances", "nodes", "events", "logs", "policy", "alerts":
	default:
		writeError(w, 404, "not_found")
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, 400, "invalid_query")
		return
	}
	if hasCredentialQuery(query) {
		writeError(w, 400, "credential_query_forbidden")
		return
	}
	mode, ok := s.authenticateReadAPI(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
	defer cancel()
	if resource == "policy" || resource == "alerts" {
		if len(query) != 0 {
			writeError(w, 400, "invalid_query")
			return
		}
		if resource == "policy" {
			b, ok := s.backend.(policyBackend)
			if !ok {
				writeError(w, 503, "policy_unavailable")
				return
			}
			value, err := b.Policy(ctx)
			if p, ok := value.(PolicyView); ok && mode == "api_token" {
				p.CanManage = false
				p.ReadOnly = true
				value = p
			}
			writeBackend(w, value, err)
		} else {
			b, ok := s.backend.(alertsBackend)
			if !ok {
				writeError(w, 403, "alerts_disabled")
				return
			}
			value, err := b.Alerts(ctx)
			if v, ok := value.(map[string]any); ok {
				v["read_only"] = mode == "api_token"
				if mode == "api_token" {
					v["can_configure"] = false
				}
			}
			writeBackend(w, value, err)
		}
		return
	}
	if resource == "logs" {
		if !validAPIQuery(query, "region", "namespace", "pod", "container", "mode", "minutes", "limit", "search", "before") {
			writeError(w, 400, "invalid_query")
			return
		}
		value, err := s.backend.Logs(ctx, query)
		if err != nil {
			writeBackend(w, nil, err)
			return
		}
		data, err := apiObject(value)
		if err != nil {
			writeError(w, 502, "upstream_invalid_response")
			return
		}
		writeJSON(w, 200, apiResponse{APIVersion: "v1", ObservedAt: textField(data, "observed_at"), ReadOnly: mode == "api_token", Warnings: []apiWarning{}, Data: fields(data, "source", "observed_at", "retention_days", "entries", "truncated", "next_before")})
		return
	}
	allowed := map[string][]string{
		"overview":  {"region"},
		"rooms":     {"limit", "offset", "region", "state", "worker_id", "room_id", "user_id"},
		"instances": {"limit", "offset", "region", "state", "worker_id", "node"},
		"nodes":     {"limit", "offset", "region", "name", "role", "ready"},
		"events":    {"limit", "offset", "region", "namespace", "object_name", "type", "reason"},
	}
	if !validAPIQuery(query, allowed[resource]...) {
		writeError(w, 400, "invalid_query")
		return
	}
	limit, offset, ok := apiPagination(query)
	if !ok || (query.Get("ready") != "" && query.Get("ready") != "true" && query.Get("ready") != "false") {
		writeError(w, 400, "invalid_query")
		return
	}
	value, err := s.backend.Snapshot(ctx)
	if err != nil {
		writeBackend(w, nil, err)
		return
	}
	snapshot, err := apiObject(value)
	if err != nil {
		writeError(w, 502, "upstream_invalid_response")
		return
	}
	fleet, ok := snapshot["fleet"].(map[string]any)
	if !ok {
		writeError(w, 502, "upstream_invalid_response")
		return
	}
	regions, ok := snapshot["regions"].([]any)
	if !ok {
		writeError(w, 502, "upstream_invalid_response")
		return
	}
	selected, known := selectRegions(regions, query.Get("region"))
	if !known {
		writeError(w, 400, "unknown_region")
		return
	}
	warnings := snapshotWarnings(fleet, selected)
	response := apiResponse{APIVersion: "v1", ObservedAt: textField(snapshot, "observed_at"), ReadOnly: mode == "api_token", Warnings: warnings, Partial: len(warnings) > 0}
	if resource == "overview" {
		response.Data = overviewProjection(snapshot, fleet, selected, query.Get("region"), mode)
		writeJSON(w, 200, response)
		return
	}
	if (resource == "rooms" || resource == "instances") && fleet["ok"] != true {
		code := textField(fleet, "error")
		if !safeErrorCode(code) {
			code = "fleet_snapshot_unavailable"
		}
		writeError(w, 503, code)
		return
	}
	rows := projectRows(resource, fleet, selected)
	filtered := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if matchRow(resource, row, query) {
			filtered = append(filtered, row)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool { return rowSortKey(resource, filtered[i]) < rowSortKey(resource, filtered[j]) })
	page := apiPage{Offset: offset, Limit: limit, Total: len(filtered)}
	start := min(offset, len(filtered))
	end := min(start+limit, len(filtered))
	page.Returned = end - start
	if end < len(filtered) {
		next := end
		page.NextOffset = &next
	}
	response.Data = filtered[start:end]
	response.Page = &page
	writeJSON(w, 200, response)
}

func apiObject(value any) (map[string]any, error) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > controlMaxResponse {
		return nil, sourceError{502, "upstream_invalid_response"}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var result map[string]any
	if decoder.Decode(&result) != nil || result == nil {
		return nil, sourceError{502, "upstream_invalid_response"}
	}
	return result, nil
}

func validAPIQuery(q url.Values, allowed ...string) bool {
	known := map[string]bool{}
	for _, key := range allowed {
		known[key] = true
	}
	for key, values := range q {
		if !known[key] || len(values) != 1 || len(values[0]) > 200 || !utf8.ValidString(values[0]) {
			return false
		}
		for _, c := range values[0] {
			if c < 32 || c == 127 {
				return false
			}
		}
	}
	return true
}

func apiPagination(q url.Values) (limit, offset int, ok bool) {
	limit = 50
	for _, field := range []string{"limit", "offset"} {
		value := q.Get(field)
		if value == "" {
			continue
		}
		parsed, err := strconv.Atoi(value)
		if err != nil || strconv.Itoa(parsed) != value {
			return 0, 0, false
		}
		if field == "limit" {
			limit = parsed
		} else {
			offset = parsed
		}
	}
	return limit, offset, limit >= 1 && limit <= 200 && offset >= 0 && offset <= 100000
}

func textField(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}
func objects(value any) []map[string]any {
	result := []map[string]any{}
	if items, ok := value.([]any); ok {
		for _, item := range items {
			if object, ok := item.(map[string]any); ok {
				result = append(result, object)
			}
		}
	}
	return result
}
func fields(source map[string]any, names ...string) map[string]any {
	result := map[string]any{}
	for _, name := range names {
		if value, ok := source[name]; ok {
			result[name] = value
		}
	}
	return result
}

func selectRegions(regions []any, filter string) ([]map[string]any, bool) {
	selected := []map[string]any{}
	for _, item := range regions {
		if region, ok := item.(map[string]any); ok && (filter == "" || textField(region, "name") == filter) {
			selected = append(selected, region)
		}
	}
	return selected, filter == "" || len(selected) > 0
}

func snapshotWarnings(fleet map[string]any, regions []map[string]any) []apiWarning {
	warnings := []apiWarning{}
	appendCode := func(source, code string) {
		if !safeErrorCode(code) {
			code = "upstream_unavailable"
		}
		warnings = append(warnings, apiWarning{source, code})
	}
	if fleet["ok"] != true {
		appendCode("fleet", textField(fleet, "error"))
	}
	for _, region := range regions {
		if region["ok"] != true {
			appendCode("region:"+textField(region, "name"), textField(region, "error"))
		}
		if region["metrics_ok"] != true {
			appendCode("metrics:"+textField(region, "name"), "metrics_unavailable")
		}
	}
	return warnings
}

func overviewProjection(snapshot, fleet map[string]any, regions []map[string]any, regionFilter, mode string) any {
	view := fields(fleet, "ok", "revision", "sampled_at", "max_processes", "rooms_per_process", "creation_blocked_reason", "error", "management_error")
	view["can_manage"] = mode == "session" && fleet["can_manage"] == true
	view["instances_total"], view["instances_active"], view["rooms_total"], view["rooms_active"], view["players_connected"] = nil, nil, nil, nil, nil
	if fleet["ok"] == true {
		workers, activeWorkers, rooms, activeRooms, players := 0, 0, 0, 0, 0
		for _, worker := range objects(fleet["workers"]) {
			if regionFilter != "" && textField(worker, "region") != regionFilter {
				continue
			}
			workers++
			if state.LiveWorker(textField(worker, "state")) {
				activeWorkers++
			}
		}
		for _, room := range objects(fleet["rooms"]) {
			if regionFilter != "" && textField(room, "region") != regionFilter {
				continue
			}
			rooms++
			if state.Terminal(textField(room, "state")) {
				continue
			}
			activeRooms++
			for _, player := range objects(room["players"]) {
				if player["connected"] == true {
					players++
				}
			}
		}
		view["instances_total"], view["instances_active"], view["rooms_total"], view["rooms_active"], view["players_connected"] = workers, activeWorkers, rooms, activeRooms, players
	}
	regionViews := []any{}
	for _, region := range regions {
		item := fields(region, "name", "ok", "metrics_ok", "error", "observed_namespaces", "pod_count_scope")
		item["nodes_sampled"], item["pods_sampled"], item["events_sampled"] = len(objects(region["nodes"])), len(objects(region["pods"])), len(objects(region["events"]))
		regionViews = append(regionViews, item)
	}
	capacityRegions := make([]any, 0, len(regions))
	for _, region := range regions {
		capacityRegions = append(capacityRegions, region)
	}
	return map[string]any{"fleet": view, "regions": regionViews, "capacity": capacityProjection(fleet, capacityRegions), "log_retention_days": snapshot["log_retention_days"], "drain_scope": "instance"}
}

func projectRows(resource string, fleet map[string]any, regions []map[string]any) []map[string]any {
	rows := []map[string]any{}
	if resource == "rooms" {
		for _, room := range objects(fleet["rooms"]) {
			row := fields(room, "id", "allocation_id", "worker_id", "region", "state", "epoch", "created_at", "expires_at", "terminal_at", "error")
			players := []any{}
			for _, player := range objects(room["players"]) {
				players = append(players, fields(player, "user_id", "seat", "connected", "ever_connected", "reconnect_until"))
			}
			row["players"] = players
			rows = append(rows, row)
		}
		return rows
	}
	if resource == "instances" {
		for _, worker := range objects(fleet["workers"]) {
			row := fields(worker, "id", "pod", "region", "build_hash", "state", "host", "port", "max_rooms", "occupied_rooms", "player_count", "ready", "draining", "created_at", "last_heartbeat", "metrics", "error")
			if metrics, ok := worker["metrics"].(map[string]any); ok {
				projected := fields(metrics, "simulation_pending", "simulation_active", "simulation_oldest_seconds", "memory_bytes", "frame_p99_ms", "audit_pending", "audit_active", "pending_results", "simulation_workers", "audit_workers")
				for _, key := range timingMetricNames {
					if raw, exists := metrics[key]; exists {
						projected[key] = nil
						if window, ok := raw.(map[string]any); ok {
							projected[key] = fields(window, "window_seconds", "count", "p50", "p95", "p99", "max", "last_sample_age_seconds")
						}
					}
				}
				row["metrics"] = projected
			}
			row["pod_status"] = nil
			for _, region := range regions {
				if textField(region, "name") != textField(worker, "region") {
					continue
				}
				for _, pod := range objects(region["pods"]) {
					if textField(pod, "worker_id") == textField(worker, "id") {
						row["node"] = pod["node"]
						row["namespace"] = pod["namespace"]
						row["pod_status"] = fields(pod, "name", "namespace", "node", "phase", "ready", "restarts", "reason", "containers", "cpu_millicores", "memory_bytes", "scheduling_capacity_shortage")
					}
				}
			}
			rows = append(rows, row)
		}
		return rows
	}
	for _, region := range regions {
		for _, row := range objects(region[resource]) {
			var projected map[string]any
			if resource == "nodes" {
				projected = fields(row, "name", "ready", "internal_ips", "external_ips", "operator_public_ip", "ready_status", "ready_last_transition_at", "unschedulable", "role", "cpu_capacity_millicores", "memory_capacity_bytes", "cpu_allocatable_millicores", "memory_allocatable_bytes", "cpu_millicores", "memory_bytes")
			} else {
				projected = fields(row, "namespace", "object_name", "type", "reason", "message", "time")
			}
			projected["region"] = region["name"]
			rows = append(rows, projected)
		}
	}
	return rows
}

func matchRow(resource string, row map[string]any, q url.Values) bool {
	for key, values := range q {
		value := values[0]
		if value == "" || key == "limit" || key == "offset" {
			continue
		}
		if key == "ready" {
			if (row["ready"] == true) != (value == "true") {
				return false
			}
			continue
		}
		if key == "user_id" {
			found := false
			for _, player := range objects(row["players"]) {
				found = found || textField(player, "user_id") == value
			}
			if !found {
				return false
			}
			continue
		}
		field := key
		if key == "room_id" || (key == "worker_id" && resource == "instances") {
			field = "id"
		}
		if textField(row, field) != value {
			return false
		}
	}
	return true
}

func rowSortKey(resource string, row map[string]any) string {
	// Stable identity ordering makes offset pages reproducible within a snapshot.
	switch resource {
	case "rooms":
		return textField(row, "region") + "/" + textField(row, "id") + "/" + textField(row, "allocation_id")
	case "instances":
		return textField(row, "region") + "/" + textField(row, "id")
	case "nodes":
		return textField(row, "region") + "/" + textField(row, "name")
	default:
		return textField(row, "region") + "/" + textField(row, "time") + "/" + textField(row, "namespace") + "/" + textField(row, "object_name") + "/" + textField(row, "reason")
	}
}
