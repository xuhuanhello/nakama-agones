package console

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

type AlertConfig struct {
	Enabled         bool    `json:"enabled"`
	FeishuEnabled   bool    `json:"feishu_enabled"`
	WaitP95MS       float64 `json:"wait_p95_ms"`
	FrameP99MS      float64 `json:"frame_p99_ms"`
	HoldSeconds     int64   `json:"hold_seconds"`
	CooldownSeconds int64   `json:"cooldown_seconds"`
	MinSamples      int     `json:"min_samples"`
}

var timingMetricNames = []string{"client_presentation_to_ready_ms", "client_presentation_to_settlement_ms", "server_first_ack_to_ready_ms", "server_first_ack_to_settlement_ms", "server_last_ack_to_ready_ms", "server_last_ack_to_settlement_ms", "simulation_queue_ms", "simulation_work_ms"}

func defaultAlerts() AlertConfig {
	return AlertConfig{Enabled: true, WaitP95MS: 1000, FrameP99MS: 100, HoldSeconds: 60, CooldownSeconds: 600, MinSamples: 5}
}
func (c AlertConfig) valid() bool {
	return finite(c.WaitP95MS) && finite(c.FrameP99MS) && c.WaitP95MS >= 100 && c.WaitP95MS <= 10000 && c.FrameP99MS >= 10 && c.FrameP99MS <= 1000 && c.HoldSeconds >= 15 && c.HoldSeconds <= 1800 && c.CooldownSeconds >= 60 && c.CooldownSeconds <= 86400 && c.MinSamples >= 1 && c.MinSamples <= 1000
}

type AlertUpdate struct {
	ExpectedRevision int64 `json:"expected_revision"`
	AlertConfig
	FeishuWebhook       *string `json:"feishu_webhook,omitempty"`
	FeishuSigningSecret *string `json:"feishu_signing_secret,omitempty"`
}
type alertSample struct {
	Key, Kind, WorkerID, Region, Source string
	Value, Threshold                    float64
	Known, Breached                     bool
}
type alertCondition struct {
	Key             string  `json:"key"`
	Kind            string  `json:"kind"`
	WorkerID        string  `json:"worker_id"`
	Region          string  `json:"region"`
	Source          string  `json:"source"`
	State           string  `json:"state"`
	Since           int64   `json:"since"`
	LastObservedAt  int64   `json:"last_observed_at"`
	LastNotifiedAt  int64   `json:"last_notified_at"`
	LastAttemptAt   int64   `json:"last_attempt_at"`
	Value           float64 `json:"value"`
	Threshold       float64 `json:"threshold"`
	Firing          bool    `json:"firing"`
	RecoverySince   int64   `json:"recovery_since,omitempty"`
	RecoveryPending bool    `json:"recovery_pending,omitempty"`
}
type alertEvent struct {
	At       int64   `json:"at"`
	Key      string  `json:"key"`
	Kind     string  `json:"kind"`
	Event    string  `json:"event"`
	WorkerID string  `json:"worker_id"`
	Value    float64 `json:"value"`
}
type alertDelivery struct {
	LastSuccessAt int64  `json:"last_success_at"`
	LastErrorCode string `json:"last_error_code"`
}
type alertAudit struct {
	At             int64  `json:"at"`
	Actor          string `json:"actor"`
	Revision       int64  `json:"revision"`
	WebhookChanged bool   `json:"webhook_changed"`
	SigningChanged bool   `json:"signing_changed"`
}
type operationsState struct {
	Schema              int                        `json:"schema"`
	Revision            int64                      `json:"revision"`
	Config              AlertConfig                `json:"config"`
	FeishuWebhook       string                     `json:"feishu_webhook"`
	FeishuSigningSecret string                     `json:"feishu_signing_secret"`
	Active              map[string]*alertCondition `json:"active"`
	History             []alertEvent               `json:"history"`
	Delivery            alertDelivery              `json:"delivery"`
	Audit               []alertAudit               `json:"audit"`
}
type alertSender interface {
	Send(context.Context, string, string, string, time.Time) error
}
type operations struct {
	mu        sync.Mutex
	state     operationsState
	path      string
	closeFile func()
	diskHash  [32]byte
	sender    alertSender
	now       func() time.Time
}
type alertsBackend interface {
	Alerts(context.Context) (any, error)
	UpdateAlerts(context.Context, AlertUpdate, string) (any, error)
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, e := n.Float64()
		return f, e == nil && finite(f)
	case float64:
		return n, finite(n)
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}
func mapObject(v any) map[string]any { m, _ := v.(map[string]any); return m }

func (d *DataSource) Alerts(context.Context) (any, error) {
	if d.operations == nil {
		disabled := defaultAlerts()
		disabled.Enabled = false
		return map[string]any{"api_version": "v1", "can_configure": false, "revision": 0, "config": disabled, "feishu_configured": false, "feishu_signing_configured": false, "active": []any{}, "history": []any{}, "delivery": alertDelivery{}}, nil
	}
	return d.operations.view(), nil
}
func (d *DataSource) UpdateAlerts(_ context.Context, in AlertUpdate, actor string) (any, error) {
	if d.operations == nil {
		return nil, sourceError{403, "alerts_disabled"}
	}
	if err := d.operations.update(in, actor); err != nil {
		return nil, err
	}
	return d.operations.view(), nil
}
func (o *operations) view() map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	active := []alertCondition{}
	for _, a := range o.state.Active {
		if a.State != "healthy" {
			active = append(active, *a)
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i].Key < active[j].Key })
	return map[string]any{"api_version": "v1", "can_configure": true, "revision": o.state.Revision, "config": o.state.Config, "feishu_configured": o.state.FeishuWebhook != "", "feishu_signing_configured": o.state.FeishuSigningSecret != "", "active": active, "history": append([]alertEvent{}, o.state.History...), "delivery": o.state.Delivery, "audit": append([]alertAudit{}, o.state.Audit...), "observed_at": o.now().UTC().Format(time.RFC3339)}
}
func (o *operations) update(in AlertUpdate, actor string) error {
	if !in.AlertConfig.valid() || !validUsername(actor) {
		return sourceError{400, "alerts_invalid"}
	}
	if in.FeishuWebhook != nil && !validFeishuWebhook(*in.FeishuWebhook) {
		return sourceError{400, "alerts_invalid"}
	}
	if in.FeishuSigningSecret != nil && !validFeishuSecret(*in.FeishuSigningSecret) {
		return sourceError{400, "alerts_invalid"}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if in.ExpectedRevision != o.state.Revision {
		return sourceError{409, "alerts_conflict"}
	}
	next := cloneOperations(o.state)
	next.Revision++
	next.Config = in.AlertConfig
	if in.FeishuWebhook != nil {
		next.FeishuWebhook = *in.FeishuWebhook
	}
	if in.FeishuSigningSecret != nil {
		next.FeishuSigningSecret = *in.FeishuSigningSecret
	}
	if next.Config.FeishuEnabled && next.FeishuWebhook == "" {
		return sourceError{400, "feishu_not_configured"}
	}
	next.Audit = append(next.Audit, alertAudit{At: o.now().Unix(), Actor: actor, Revision: next.Revision, WebhookChanged: in.FeishuWebhook != nil, SigningChanged: in.FeishuSigningSecret != nil})
	if len(next.Audit) > 100 {
		next.Audit = next.Audit[len(next.Audit)-100:]
	}
	return o.save(next)
}
func cloneOperations(s operationsState) operationsState {
	b, _ := json.Marshal(s)
	var n operationsState
	_ = json.Unmarshal(b, &n)
	if n.Active == nil {
		n.Active = map[string]*alertCondition{}
	}
	return n
}

func alertSamples(snapshot map[string]any, cfg AlertConfig, now int64) map[string]alertSample {
	out := map[string]alertSample{}
	fleet := mapObject(snapshot["fleet"])
	if fleet["ok"] == true {
		for _, w := range objects(fleet["workers"]) {
			id := textField(w, "id")
			if !workerName.MatchString(id) {
				continue
			}
			metrics := mapObject(w["metrics"])
			hb, _ := number(w["last_heartbeat"])
			fresh := hb > 0 && float64(now)-hb <= 15 && float64(now) >= hb-5
			for _, rule := range []struct {
				field, kind, source string
				threshold           float64
				window              bool
			}{{"client_presentation_to_ready_ms", "client_wait_high", "client_reported", cfg.WaitP95MS, true}, {"client_presentation_to_settlement_ms", "client_settlement_wait_high", "client_reported", cfg.WaitP95MS, true}, {"frame_p99_ms", "frame_p99_high", "game_server", cfg.FrameP99MS, false}} {
				s := alertSample{Key: rule.kind + ":" + id, Kind: rule.kind, WorkerID: id, Region: textField(w, "region"), Source: rule.source, Threshold: rule.threshold}
				if rule.window {
					window := mapObject(metrics[rule.field])
					count, ok := number(window["count"])
					age, ageOK := number(window["last_sample_age_seconds"])
					value, valueOK := number(window["p95"])
					seconds, _ := number(window["window_seconds"])
					s.Value = value
					s.Known = fresh && ok && count >= float64(cfg.MinSamples) && seconds == 60 && ageOK && age >= 0 && age+float64(now)-hb <= 60 && valueOK && value >= 0
				} else {
					value, ok := number(metrics[rule.field])
					s.Value = value
					s.Known = fresh && ok && value >= 0
				}
				s.Breached = s.Known && s.Value > s.Threshold
				out[s.Key] = s
			}
		}
	}
	for _, region := range objects(snapshot["regions"]) {
		name := textField(region, "name")
		if !dnsName.MatchString(name) {
			continue
		}
		key := "physical_capacity_shortage:" + name
		s := alertSample{Key: key, Kind: "physical_capacity_shortage", Region: name, Source: "kubernetes_scheduler", Threshold: 0, Known: region["ok"] == true}
		for _, p := range objects(region["pods"]) {
			if p["phase"] == "Pending" && p["scheduling_capacity_shortage"] == true {
				s.Value++
			}
		}
		s.Breached = s.Known && s.Value > 0
		out[key] = s
	}
	return out
}

// evaluate persists hold/cooldown/recovery state before sending. Browser reads
// never trigger notifications. A failed/absent sample is unknown, not recovery.
func (o *operations) evaluate(ctx context.Context, snapshot map[string]any) {
	o.mu.Lock()
	now := o.now()
	next := cloneOperations(o.state)
	if !next.Config.Enabled {
		o.mu.Unlock()
		return
	}
	samples := alertSamples(snapshot, next.Config, now.Unix())
	messages := []string{}
	keys := []string{}
	recoveries := map[string]bool{}
	for key, a := range next.Active {
		if _, ok := samples[key]; !ok {
			a.State = "unknown"
			if !a.Firing {
				a.Since = 0
			}
			a.RecoverySince = 0
		}
	}
	sampleKeys := make([]string, 0, len(samples))
	for key := range samples {
		sampleKeys = append(sampleKeys, key)
	}
	sort.Strings(sampleKeys)
	for _, key := range sampleKeys {
		s := samples[key]
		a := next.Active[key]
		if a == nil {
			a = &alertCondition{Key: key, Kind: s.Kind, WorkerID: s.WorkerID, Region: s.Region, Source: s.Source}
			next.Active[key] = a
		}
		if !s.Known {
			a.State = "unknown"
			if !a.Firing {
				a.Since = 0
			}
			a.RecoverySince = 0
			continue
		}
		gap := a.LastObservedAt > 0 && now.Unix()-a.LastObservedAt > 20
		a.LastObservedAt = now.Unix()
		a.Value = s.Value
		a.Threshold = s.Threshold
		if s.Breached {
			a.RecoverySince = 0
			a.RecoveryPending = false
			if !a.Firing {
				if a.Since == 0 || gap {
					a.Since = now.Unix()
				}
				a.State = "pending"
				if now.Unix()-a.Since >= next.Config.HoldSeconds {
					a.Firing = true
					a.State = "firing"
					next.History = append(next.History, alertEvent{now.Unix(), key, a.Kind, "firing", a.WorkerID, a.Value})
				}
			} else {
				a.State = "firing"
			}
		} else if a.Firing {
			if a.RecoverySince == 0 || gap {
				a.RecoverySince = now.Unix()
			}
			a.State = "recovering"
			if now.Unix()-a.RecoverySince >= next.Config.HoldSeconds {
				a.Firing = false
				a.State = "healthy"
				a.Since = 0
				a.RecoveryPending = a.LastNotifiedAt > 0
				next.History = append(next.History, alertEvent{now.Unix(), key, a.Kind, "recovered", a.WorkerID, a.Value})
			}
		} else {
			a.State = "healthy"
			a.Since = 0
		}
		fire := a.Firing && a.State == "firing" && (a.LastNotifiedAt == 0 || now.Unix()-a.LastNotifiedAt >= next.Config.CooldownSeconds)
		if next.Config.FeishuEnabled && (fire || a.RecoveryPending) && now.Unix()-a.LastAttemptAt >= 30 && len(keys) < 20 {
			a.LastAttemptAt = now.Unix()
			keys = append(keys, key)
			event := "firing"
			if a.RecoveryPending {
				event = "recovered"
				recoveries[key] = true
			}
			messages = append(messages, fmt.Sprintf("%s %s region=%s worker=%s value=%.1f threshold=%.1f source=%s", event, a.Kind, a.Region, a.WorkerID, a.Value, a.Threshold, a.Source))
		}
	}
	for key, a := range next.Active {
		if (a.State == "healthy" || a.State == "unknown") && !a.Firing && !a.RecoveryPending && now.Unix()-a.LastObservedAt > 86400 {
			delete(next.Active, key)
		}
	}
	if len(next.History) > 200 {
		next.History = next.History[len(next.History)-200:]
	}
	if len(next.Active) > 5000 {
		next.Delivery.LastErrorCode = "alerts_state_limit"
		o.mu.Unlock()
		return
	}
	if o.save(next) != nil {
		o.mu.Unlock()
		return
	}
	webhook, secret, revision := next.FeishuWebhook, next.FeishuSigningSecret, next.Revision
	o.mu.Unlock()
	if len(keys) == 0 || webhook == "" || !next.Config.FeishuEnabled {
		return
	}
	err := o.sender.Send(ctx, webhook, secret, "Nakama Agones alerts\n"+strings.Join(messages, "\n"), now)
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.state.Revision != revision {
		return
	}
	next = cloneOperations(o.state)
	if err != nil {
		next.Delivery.LastErrorCode = "feishu_delivery_failed"
	} else {
		next.Delivery.LastErrorCode = ""
		next.Delivery.LastSuccessAt = now.Unix()
		for _, key := range keys {
			if a := next.Active[key]; a != nil {
				a.LastNotifiedAt = now.Unix()
				if recoveries[key] {
					a.RecoveryPending = false
				}
			}
		}
	}
	_ = o.save(next)
}
