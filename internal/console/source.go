package console

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xuhuanhello/nakama-agones/internal/state"
)

type SourceConfig struct {
	FleetSnapshotFile string         `json:"fleet_snapshot_file,omitempty"`
	AllowManagement   bool           `json:"allow_management,omitempty"`
	FleetURL          string         `json:"fleet_url"`
	FleetTokenFile    string         `json:"fleet_token_file"`
	Regions           []RegionConfig `json:"regions"`
	MaxProcesses      int            `json:"max_processes"`
	RoomsPerProcess   int            `json:"rooms_per_process"`
	LogRetentionDays  int            `json:"log_retention_days"`
}
type RegionConfig struct {
	Name            string `json:"name"`
	APIURL          string `json:"api_url"`
	CAFile          string `json:"ca_file"`
	TokenFile       string `json:"token_file"`
	Namespace       string `json:"namespace"`
	SystemNamespace string `json:"system_namespace"`
	LogNamespace    string `json:"log_namespace"`
	LogService      string `json:"log_service"`
}
type upstream struct {
	base, tokenFile string
	client          *http.Client
}
type regionSource struct {
	cfg RegionConfig
	up  upstream
}
type DataSource struct {
	cfg      SourceConfig
	fleet    upstream
	regions  []regionSource
	mu       sync.Mutex
	cached   any
	cachedAt time.Time
}
type sourceError struct {
	status int
	code   string
}

func (e sourceError) Error() string   { return e.code }
func (e sourceError) HTTPStatus() int { return e.status }
func badQuery() error                 { return sourceError{400, "invalid_query"} }

var dnsName = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$`)
var workerName = regexp.MustCompile(`^[a-f0-9]{32}$`)
var credentialLine = regexp.MustCompile(`(?i)(authorization\s*[:=]|bearer\s+[a-z0-9._-]+|(?:password|passwd|secret|(?:access|refresh|bootstrap|admission|api)[_-]?(?:token|key)|signing[_-]?key)\s*["']?\s*[:=]|postgres(?:ql)?://|[a-z][a-z0-9+.-]*://[^/\s@:]+:[^/\s@]+@|eyJ[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]+\.)`)

func NewDataSource(cfg SourceConfig) (*DataSource, error) {
	if cfg.LogRetentionDays == 0 {
		cfg.LogRetentionDays = 7
	}
	if cfg.LogRetentionDays < 1 || cfg.LogRetentionDays > 30 || len(cfg.Regions) == 0 || len(cfg.Regions) > 16 {
		return nil, errors.New("invalid source configuration")
	}
	var f upstream
	if cfg.FleetSnapshotFile != "" {
		if !filepath.IsAbs(cfg.FleetSnapshotFile) || cfg.FleetURL != "" || cfg.FleetTokenFile != "" || cfg.AllowManagement {
			return nil, errors.New("snapshot source must be read-only and exclusive")
		}
	} else {
		var err error
		f, err = newUpstream(cfg.FleetURL, "", cfg.FleetTokenFile, true)
		if err != nil {
			return nil, err
		}
	}
	d := &DataSource{cfg: cfg, fleet: f}
	seen := map[string]bool{}
	for _, r := range cfg.Regions {
		if r.Namespace == "" {
			r.Namespace = "agones-games"
		}
		if r.SystemNamespace == "" {
			r.SystemNamespace = "agones-system"
		}
		if r.LogNamespace == "" {
			r.LogNamespace = "agones-observability"
		}
		if r.LogService == "" {
			r.LogService = "fleet-loki"
		}
		for _, v := range []string{r.Name, r.Namespace, r.SystemNamespace, r.LogNamespace, r.LogService} {
			if !dnsName.MatchString(v) || strings.Contains(v, "..") {
				return nil, errors.New("invalid region configuration")
			}
		}
		if seen[r.Name] {
			return nil, errors.New("duplicate region")
		}
		seen[r.Name] = true
		u, err := newUpstream(r.APIURL, r.CAFile, r.TokenFile, false)
		if err != nil {
			return nil, err
		}
		d.regions = append(d.regions, regionSource{r, u})
	}
	return d, nil
}
func newUpstream(raw, ca, tokenFile string, localHTTP bool) (upstream, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || tokenFile == "" {
		return upstream{}, errors.New("invalid upstream configuration")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := ip != nil && ip.IsLoopback()
	if u.Scheme != "https" && !(localHTTP && u.Scheme == "http" && loopback) {
		return upstream{}, errors.New("upstream TLS or loopback required")
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if ca != "" {
		p, err := os.ReadFile(ca)
		if err != nil {
			return upstream{}, errors.New("cannot read upstream CA")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(p) {
			return upstream{}, errors.New("invalid upstream CA")
		}
		tc.RootCAs = pool
	}
	tr := &http.Transport{TLSClientConfig: tc, DialContext: (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 32, MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 3 * time.Second}
	return upstream{strings.TrimRight(raw, "/"), tokenFile, &http.Client{Timeout: 7 * time.Second, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (u upstream) request(ctx context.Context, method, path string, body []byte, max int64) ([]byte, error) {
	token, err := os.ReadFile(u.tokenFile)
	if err != nil || len(token) == 0 || len(token) > 32768 {
		return nil, sourceError{502, "upstream_credential_unavailable"}
	}
	req, err := http.NewRequestWithContext(ctx, method, u.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, sourceError{502, "upstream_request_failed"}
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := u.client.Do(req)
	if err != nil {
		return nil, sourceError{502, "upstream_unavailable"}
	}
	defer res.Body.Close()
	if res.StatusCode == 404 {
		return nil, sourceError{404, "resource_not_found"}
	}
	if res.StatusCode == 401 || res.StatusCode == 403 {
		return nil, sourceError{502, "upstream_access_denied"}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, sourceError{502, "upstream_request_failed"}
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, sourceError{502, "upstream_response_limit"}
	}
	return b, nil
}
func (u upstream) get(ctx context.Context, path string, v any) error {
	b, err := u.request(ctx, http.MethodGet, path, nil, 8<<20)
	if err != nil {
		return err
	}
	if json.Unmarshal(b, v) != nil {
		return sourceError{502, "upstream_invalid_response"}
	}
	return nil
}
func safeText(s string) string {
	if credentialLine.MatchString(s) {
		return "[REDACTED: sensitive log line]"
	}
	if len(s) > 16384 {
		return s[:16384] + " [truncated]"
	}
	return s
}
func sourceCode(err error) string {
	var e sourceError
	if errors.As(err, &e) {
		return e.code
	}
	return "upstream_unavailable"
}

func (d *DataSource) Snapshot(ctx context.Context) (any, error) {
	// Collapse concurrent browser refreshes into one bounded set of upstream reads.
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cached != nil && time.Since(d.cachedAt) < 3*time.Second {
		return d.cached, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	fleet := map[string]any{"ok": false, "workers": []any{}, "rooms": []any{}, "max_processes": d.cfg.MaxProcesses, "rooms_per_process": d.cfg.RoomsPerProcess}
	regions := make([]any, len(d.regions))
	var wg sync.WaitGroup
	wg.Add(1 + len(d.regions))
	go func() {
		defer wg.Done()
		if d.cfg.FleetSnapshotFile != "" {
			projected, err := readFleetSnapshot(d.cfg.FleetSnapshotFile)
			if err != nil {
				fleet["error"] = sourceCode(err)
				return
			}
			for key, value := range projected {
				fleet[key] = value
			}
			fleet["can_manage"] = false
			return
		}
		s := state.New()
		if err := d.fleet.get(ctx, "/agones/fleet/v1/admin/status", s); err != nil {
			fleet["error"] = sourceCode(err)
			return
		}
		s.Normalize()
		projectFleet(fleet, s)
		fleet["can_manage"] = d.cfg.AllowManagement
	}()
	for i, r := range d.regions {
		go func(i int, r regionSource) { defer wg.Done(); regions[i] = r.snapshot(ctx) }(i, r)
	}
	wg.Wait()
	out := map[string]any{"observed_at": time.Now().UTC().Format(time.RFC3339), "fleet": fleet, "regions": regions, "log_retention_days": d.cfg.LogRetentionDays}
	d.cached = out
	d.cachedAt = time.Now()
	return out, nil
}
func projectFleet(f map[string]any, s *state.State) {
	f["ok"] = true
	f["revision"] = s.Revision
	f["creation_blocked_reason"] = safeText(s.CreationBlockedReason)
	workers := []any{}
	keys := make([]string, 0, len(s.Workers))
	for k := range s.Workers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w := s.Workers[k]
		if w == nil {
			continue
		}
		pod := ""
		if workerName.MatchString(w.ID) {
			pod = "nag-" + w.ID
		}
		var metrics any
		if w.LastHeartbeat > 0 {
			metrics = w.Metrics
		}
		workers = append(workers, map[string]any{"id": w.ID, "pod": pod, "region": w.Region, "build_hash": w.BuildHash, "state": w.State, "host": w.Host, "port": w.Port, "max_rooms": w.MaxRooms, "occupied_rooms": state.Occupied(s, w.ID), "player_count": w.PlayerCount, "ready": w.Ready, "draining": w.Draining, "created_at": w.CreatedAt, "last_heartbeat": w.LastHeartbeat, "metrics": metrics, "error": safeText(w.Error)})
	}
	f["workers"] = workers
	rooms := []any{}
	allocations := make([]*state.Allocation, 0, len(s.Allocations))
	for _, a := range s.Allocations {
		if a != nil {
			allocations = append(allocations, a)
		}
	}
	sort.Slice(allocations, func(i, j int) bool { return allocations[i].CreatedAt > allocations[j].CreatedAt })
	for _, a := range allocations {
		players := []any{}
		seen := map[string]bool{}
		for _, p := range a.Sessions {
			seen[p.UserID] = true
			players = append(players, map[string]any{"user_id": p.UserID, "seat": p.Seat, "connected": p.Connected, "ever_connected": p.EverConnected, "reconnect_until": p.ReconnectUntil})
		}
		for i, id := range a.UserIDs {
			if !seen[id] {
				players = append(players, map[string]any{"user_id": id, "seat": i, "connected": false, "ever_connected": false, "reconnect_until": 0})
			}
		}
		region := ""
		if w := s.Workers[a.WorkerID]; w != nil {
			region = w.Region
		}
		rooms = append(rooms, map[string]any{"id": a.RoomID, "allocation_id": a.ID, "worker_id": a.WorkerID, "region": region, "state": a.State, "epoch": a.Epoch, "created_at": a.CreatedAt, "expires_at": a.ExpiresAt, "terminal_at": a.TerminalAt, "players": players, "error": safeText(a.Error)})
	}
	f["rooms"] = rooms
}

type meta struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels"`
}
type condition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}
type podObject struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		NodeName   string `json:"nodeName"`
		Containers []struct {
			Name string `json:"name"`
		} `json:"containers"`
		InitContainers []struct {
			Name string `json:"name"`
		} `json:"initContainers"`
	} `json:"spec"`
	Status struct {
		Phase                 string            `json:"phase"`
		Reason                string            `json:"reason"`
		Conditions            []condition       `json:"conditions"`
		ContainerStatuses     []containerStatus `json:"containerStatuses"`
		InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
	} `json:"status"`
}
type containerStatus struct {
	Name         string `json:"name"`
	RestartCount int    `json:"restartCount"`
	State        struct {
		Waiting *struct {
			Reason string `json:"reason"`
		} `json:"waiting"`
		Terminated *struct {
			Reason string `json:"reason"`
		} `json:"terminated"`
	} `json:"state"`
}
type nodeObject struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		Unschedulable bool `json:"unschedulable"`
	} `json:"spec"`
	Status struct {
		Capacity    map[string]string `json:"capacity"`
		Allocatable map[string]string `json:"allocatable"`
		Conditions  []condition       `json:"conditions"`
	} `json:"status"`
}
type resourceMetric struct {
	Metadata   meta              `json:"metadata"`
	Usage      map[string]string `json:"usage"`
	Containers []struct {
		Usage map[string]string `json:"usage"`
	} `json:"containers"`
}
type eventObject struct {
	Metadata       meta   `json:"metadata"`
	Type           string `json:"type"`
	Reason         string `json:"reason"`
	Message        string `json:"message"`
	LastTimestamp  string `json:"lastTimestamp"`
	EventTime      string `json:"eventTime"`
	InvolvedObject struct {
		Name string `json:"name"`
	} `json:"involvedObject"`
}

func itemsPath(ns, resource string) string { return "/api/v1/namespaces/" + ns + "/" + resource }
func isReady(cs []condition) bool {
	for _, c := range cs {
		if c.Type == "Ready" && c.Status == "True" {
			return true
		}
	}
	return false
}

// Kubernetes resource.Quantity uses both binary and decimal SI suffixes.
func quantity(s string) float64 {
	units := []struct {
		s string
		m float64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50}, {"Ei", 1 << 60}, {"n", 1e-9}, {"u", 1e-6}, {"m", 1e-3}, {"k", 1e3}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"P", 1e15}, {"E", 1e18}}
	m := 1.0
	for _, u := range units {
		if strings.HasSuffix(s, u.s) {
			s = strings.TrimSuffix(s, u.s)
			m = u.m
			break
		}
	}
	f, e := strconv.ParseFloat(s, 64)
	if e != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0
	}
	return f * m
}
func addUsage(m map[string]any, u map[string]string) {
	if c, ok := u["cpu"]; ok {
		m["cpu_millicores"] = quantity(c) * 1000
	}
	if b, ok := u["memory"]; ok {
		m["memory_bytes"] = int64(quantity(b))
	}
}

func (r regionSource) snapshot(ctx context.Context) any {
	out := map[string]any{"name": r.cfg.Name, "ok": true, "metrics_ok": true, "nodes": []any{}, "pods": []any{}, "events": []any{}}
	var nodes struct {
		Items []nodeObject `json:"items"`
	}
	var nm struct {
		Items []resourceMetric `json:"items"`
	}
	namespaces := []string{r.cfg.Namespace, r.cfg.SystemNamespace}
	pods := make([][]podObject, 2)
	metrics := make([][]resourceMetric, 2)
	events := make([][]eventObject, 2)
	errs := make([]error, 8)
	var wg sync.WaitGroup
	wg.Add(8)
	go func() { defer wg.Done(); errs[0] = r.up.get(ctx, "/api/v1/nodes", &nodes) }()
	go func() { defer wg.Done(); errs[1] = r.up.get(ctx, "/apis/metrics.k8s.io/v1beta1/nodes", &nm) }()
	for i, ns := range namespaces {
		go func(i int, ns string) {
			defer wg.Done()
			var v struct {
				Items []podObject `json:"items"`
			}
			errs[2+i*3] = r.up.get(ctx, itemsPath(ns, "pods"), &v)
			pods[i] = v.Items
		}(i, ns)
		go func(i int, ns string) {
			defer wg.Done()
			var v struct {
				Items []resourceMetric `json:"items"`
			}
			errs[3+i*3] = r.up.get(ctx, "/apis/metrics.k8s.io/v1beta1/namespaces/"+ns+"/pods", &v)
			metrics[i] = v.Items
		}(i, ns)
		go func(i int, ns string) {
			defer wg.Done()
			var v struct {
				Items []eventObject `json:"items"`
			}
			errs[4+i*3] = r.up.get(ctx, itemsPath(ns, "events")+"?limit=200", &v)
			events[i] = v.Items
		}(i, ns)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			if i == 1 || i == 3 || i == 6 {
				out["metrics_ok"] = false
			} else {
				out["ok"] = false
			}
			out["error"] = sourceCode(e)
		}
	}
	nodeMetrics := map[string]map[string]string{}
	for _, m := range nm.Items {
		nodeMetrics[m.Metadata.Name] = m.Usage
	}
	nsOut := []any{}
	for _, n := range nodes.Items {
		m := map[string]any{"name": n.Metadata.Name, "ready": isReady(n.Status.Conditions), "unschedulable": n.Spec.Unschedulable, "role": n.Metadata.Labels["nakama-agones.io/role"], "cpu_capacity_millicores": quantity(n.Status.Capacity["cpu"]) * 1000, "memory_capacity_bytes": int64(quantity(n.Status.Capacity["memory"])), "cpu_allocatable_millicores": quantity(n.Status.Allocatable["cpu"]) * 1000, "memory_allocatable_bytes": int64(quantity(n.Status.Allocatable["memory"]))}
		addUsage(m, nodeMetrics[n.Metadata.Name])
		nsOut = append(nsOut, m)
	}
	out["nodes"] = nsOut
	psOut := []any{}
	esOut := []any{}
	for i := range namespaces {
		pm := map[string]map[string]string{}
		for _, p := range metrics[i] {
			cpu, mem := 0.0, 0.0
			for _, c := range p.Containers {
				cpu += quantity(c.Usage["cpu"])
				mem += quantity(c.Usage["memory"])
			}
			pm[p.Metadata.Name] = map[string]string{"cpu": strconv.FormatFloat(cpu, 'f', 9, 64), "memory": strconv.FormatFloat(mem, 'f', 0, 64)}
		}
		for _, p := range pods[i] {
			if i == 0 && p.Metadata.Labels["app.kubernetes.io/managed-by"] != "nakama-agones" {
				continue
			}
			containers := []string{}
			for _, c := range p.Spec.Containers {
				containers = append(containers, c.Name)
			}
			for _, c := range p.Spec.InitContainers {
				containers = append(containers, c.Name)
			}
			restarts := 0
			reason := p.Status.Reason
			for _, c := range append(p.Status.ContainerStatuses, p.Status.InitContainerStatuses...) {
				restarts += c.RestartCount
				if c.State.Waiting != nil {
					reason = c.State.Waiting.Reason
				}
			}
			m := map[string]any{"name": p.Metadata.Name, "namespace": p.Metadata.Namespace, "node": p.Spec.NodeName, "phase": p.Status.Phase, "worker_id": p.Metadata.Labels["nakama-agones.io/worker"], "ready": isReady(p.Status.Conditions), "restarts": restarts, "reason": safeText(reason), "containers": containers}
			addUsage(m, pm[p.Metadata.Name])
			psOut = append(psOut, m)
		}
		for _, e := range events[i] {
			ts := e.LastTimestamp
			if ts == "" {
				ts = e.EventTime
			}
			esOut = append(esOut, map[string]any{"namespace": e.Metadata.Namespace, "object_name": e.InvolvedObject.Name, "type": e.Type, "reason": e.Reason, "message": safeText(e.Message), "time": ts})
		}
	}
	sort.Slice(esOut, func(i, j int) bool {
		return esOut[i].(map[string]any)["time"].(string) > esOut[j].(map[string]any)["time"].(string)
	})
	if len(esOut) > 100 {
		esOut = esOut[:100]
	}
	out["pods"] = psOut
	out["events"] = esOut
	return out
}

func (d *DataSource) Drain(ctx context.Context, worker string) error {
	if !d.cfg.AllowManagement || d.cfg.FleetSnapshotFile != "" {
		return sourceError{403, "management_disabled"}
	}
	if !workerName.MatchString(worker) {
		return badQuery()
	}
	body, _ := json.Marshal(map[string]string{"worker_id": worker})
	_, err := d.fleet.request(ctx, http.MethodPost, "/agones/fleet/v1/admin/drain", body, 65536)
	if err == nil {
		d.mu.Lock()
		d.cached = nil
		d.mu.Unlock()
	}
	return err
}
func (d *DataSource) RetryCreation(ctx context.Context) error {
	if !d.cfg.AllowManagement || d.cfg.FleetSnapshotFile != "" {
		return sourceError{403, "management_disabled"}
	}
	_, err := d.fleet.request(ctx, http.MethodPost, "/agones/fleet/v1/admin/retry-creation", []byte("{}"), 65536)
	if err == nil {
		d.mu.Lock()
		d.cached = nil
		d.mu.Unlock()
	}
	return err
}

type logEntry struct {
	Timestamp string `json:"timestamp"`
	Text      string `json:"text"`
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Container string `json:"container"`
}

func (d *DataSource) Logs(ctx context.Context, q url.Values) (any, error) {
	allowed := map[string]bool{"region": true, "namespace": true, "pod": true, "container": true, "mode": true, "minutes": true, "limit": true, "search": true}
	for k, v := range q {
		if !allowed[k] || len(v) != 1 {
			return nil, badQuery()
		}
	}
	var r *regionSource
	for i := range d.regions {
		if d.regions[i].cfg.Name == q.Get("region") {
			r = &d.regions[i]
			break
		}
	}
	if r == nil {
		return nil, badQuery()
	}
	ns := q.Get("namespace")
	if ns == "" {
		ns = r.cfg.Namespace
	}
	if ns != r.cfg.Namespace && ns != r.cfg.SystemNamespace {
		return nil, badQuery()
	}
	pod, container, mode := q.Get("pod"), q.Get("container"), q.Get("mode")
	if mode == "" {
		mode = "live"
	}
	if container == "" {
		container = "game"
	}
	if !dnsName.MatchString(pod) || strings.Contains(pod, "..") || !dnsName.MatchString(container) || strings.Contains(container, "..") || len(container) > 63 || (mode != "live" && mode != "history") {
		return nil, badQuery()
	}
	if ns == r.cfg.Namespace && !regexp.MustCompile(`^nag-[a-f0-9]{32}$`).MatchString(pod) {
		return nil, badQuery()
	}
	minutes, limit := 60, 500
	var err error
	if q.Get("minutes") != "" {
		minutes, err = strconv.Atoi(q.Get("minutes"))
		if err != nil {
			return nil, badQuery()
		}
	}
	if q.Get("limit") != "" {
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil {
			return nil, badQuery()
		}
	}
	search := q.Get("search")
	if minutes < 1 || minutes > d.cfg.LogRetentionDays*24*60 || limit < 1 || limit > 1000 || len(search) > 200 || strings.IndexByte(search, 0) >= 0 {
		return nil, badQuery()
	}
	if mode == "live" && minutes > 1440 {
		return nil, badQuery()
	}
	entries := []logEntry{}
	source := "kubernetes"
	truncated := false
	if mode == "live" {
		var p podObject
		if err := r.up.get(ctx, itemsPath(ns, "pods")+"/"+pod, &p); err != nil {
			return nil, err
		}
		if ns == r.cfg.Namespace && p.Metadata.Labels["app.kubernetes.io/managed-by"] != "nakama-agones" {
			return nil, sourceError{404, "resource_not_found"}
		}
		found := false
		for _, c := range p.Spec.Containers {
			found = found || c.Name == container
		}
		for _, c := range p.Spec.InitContainers {
			found = found || c.Name == container
		}
		if !found {
			return nil, badQuery()
		}
		params := url.Values{"container": {container}, "timestamps": {"true"}, "sinceSeconds": {strconv.Itoa(minutes * 60)}, "tailLines": {strconv.Itoa(limit)}, "limitBytes": {"1048576"}}
		raw, err := r.up.request(ctx, http.MethodGet, itemsPath(ns, "pods")+"/"+pod+"/log?"+params.Encode(), nil, 1<<20)
		if err != nil {
			return nil, err
		}
		lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
		truncated = len(lines) >= limit || len(raw) >= 1<<20
		for _, line := range lines {
			if line == "" {
				continue
			}
			ts, text, ok := strings.Cut(line, " ")
			if !ok {
				ts = ""
				text = line
			}
			text = safeText(text)
			if search != "" && !strings.Contains(strings.ToLower(text), strings.ToLower(search)) {
				continue
			}
			entries = append(entries, logEntry{ts, text, ns, pod, container})
		}
	} else {
		source = "loki"
		selector := fmt.Sprintf(`{cluster=%s,namespace=%s,pod=%s,container=%s}`, strconv.Quote(r.cfg.Name), strconv.Quote(ns), strconv.Quote(pod), strconv.Quote(container))
		if search != "" {
			selector += " |= " + strconv.Quote(search)
		}
		now := time.Now()
		params := url.Values{"query": {selector}, "start": {strconv.FormatInt(now.Add(-time.Duration(minutes)*time.Minute).UnixNano(), 10)}, "end": {strconv.FormatInt(now.UnixNano(), 10)}, "limit": {strconv.Itoa(limit)}, "direction": {"backward"}}
		var result struct {
			Status string `json:"status"`
			Data   struct {
				ResultType string `json:"resultType"`
				Result     []struct {
					Values [][]string `json:"values"`
				} `json:"result"`
			} `json:"data"`
		}
		if err := r.up.get(ctx, itemsPath(r.cfg.LogNamespace, "services")+"/http:"+r.cfg.LogService+":http/proxy/loki/api/v1/query_range?"+params.Encode(), &result); err != nil {
			return nil, err
		}
		if result.Status != "success" || result.Data.ResultType != "streams" {
			return nil, sourceError{502, "upstream_invalid_response"}
		}
		for _, stream := range result.Data.Result {
			for _, v := range stream.Values {
				if len(v) != 2 {
					continue
				}
				nsTime, err := strconv.ParseInt(v[0], 10, 64)
				if err != nil {
					continue
				}
				entries = append(entries, logEntry{time.Unix(0, nsTime).UTC().Format(time.RFC3339Nano), safeText(v[1]), ns, pod, container})
			}
		}
		sort.Slice(entries, func(i, j int) bool {
			left, _ := time.Parse(time.RFC3339Nano, entries[i].Timestamp)
			right, _ := time.Parse(time.RFC3339Nano, entries[j].Timestamp)
			return left.Before(right)
		})
		truncated = len(entries) >= limit
		if len(entries) > limit {
			entries = entries[len(entries)-limit:]
		}
	}
	return map[string]any{"source": source, "observed_at": time.Now().UTC().Format(time.RFC3339), "retention_days": d.cfg.LogRetentionDays, "entries": entries, "truncated": truncated}, nil
}

// A root-owned exporter publishes a credential-free projection. An expired
// snapshot is reported unavailable; it must never look like an empty fleet.
func readFleetSnapshot(path string) (map[string]any, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0022 != 0 || before.Size() > 8<<20 {
		return nil, sourceError{502, "fleet_snapshot_unavailable"}
	}
	age := time.Since(before.ModTime())
	if age > 20*time.Second || age < -5*time.Second {
		return nil, sourceError{502, "fleet_snapshot_stale"}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, sourceError{502, "fleet_snapshot_unavailable"}
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		return nil, sourceError{502, "fleet_snapshot_unavailable"}
	}
	raw, err := io.ReadAll(io.LimitReader(f, (8<<20)+1))
	if err != nil || len(raw) > 8<<20 {
		return nil, sourceError{502, "fleet_snapshot_unavailable"}
	}
	var result map[string]any
	if json.Unmarshal(raw, &result) != nil || result["ok"] != true {
		return nil, sourceError{502, "fleet_snapshot_invalid"}
	}
	result["sampled_at"] = before.ModTime().UTC().Format(time.RFC3339)
	return result, nil
}
