package console

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recordingSender struct {
	messages []string
	fail     bool
}

func (s *recordingSender) Send(_ context.Context, _, _, text string, _ time.Time) error {
	s.messages = append(s.messages, text)
	if s.fail {
		return errors.New("must-not-leak-webhook")
	}
	return nil
}

const fakeWebhook = "https://open.feishu.cn/open-apis/bot/v2/hook/fixture-not-a-real-webhook"

func alertFixture() (*operations, *recordingSender, *int64) {
	now := int64(1700000000)
	sender := &recordingSender{}
	cfg := defaultAlerts()
	cfg.Enabled = true
	cfg.FeishuEnabled = true
	cfg.HoldSeconds = 20
	cfg.CooldownSeconds = 60
	o := &operations{state: operationsState{Schema: 1, Revision: 1, Config: cfg, FeishuWebhook: fakeWebhook, Active: map[string]*alertCondition{}}, sender: sender, now: func() time.Time { return time.Unix(now, 0) }}
	return o, sender, &now
}
func sampleSnapshot(now int64, value float64, count int) map[string]any {
	return map[string]any{"fleet": map[string]any{"ok": true, "workers": []any{map[string]any{"id": strings.Repeat("a", 32), "region": "test", "last_heartbeat": now, "metrics": map[string]any{"client_presentation_to_ready_ms": map[string]any{"window_seconds": 60, "count": count, "p95": value, "last_sample_age_seconds": 0}, "frame_p99_ms": 10}}}}, "regions": []any{map[string]any{"name": "test", "ok": true, "pods": []any{}}}}
}
func TestAlertHoldCooldownMergeAndRecovery(t *testing.T) {
	o, s, now := alertFixture()
	for i := 0; i < 2; i++ {
		o.evaluate(context.Background(), sampleSnapshot(*now, 1100, 5))
		*now += 10
	}
	if len(s.messages) != 0 {
		t.Fatal("alert fired without sustained threshold")
	}
	o.evaluate(context.Background(), sampleSnapshot(*now, 1100, 5))
	if len(s.messages) != 1 || !strings.Contains(s.messages[0], "client_wait_high") {
		t.Fatal("sustained alert missing")
	}
	for i := 0; i < 5; i++ {
		*now += 10
		o.evaluate(context.Background(), sampleSnapshot(*now, 1300, 8))
	}
	if len(s.messages) != 1 {
		t.Fatal("cooldown spam")
	}
	*now += 10
	o.evaluate(context.Background(), sampleSnapshot(*now, 1300, 8))
	if len(s.messages) != 2 {
		t.Fatal("cooldown reminder missing")
	}
	for i := 0; i < 3; i++ {
		*now += 10
		o.evaluate(context.Background(), sampleSnapshot(*now, 100, 8))
	}
	if len(s.messages) != 3 || !strings.Contains(s.messages[2], "recovered") {
		t.Fatal("recovery not delivered after healthy hold")
	}
}
func TestMissingOrStaleTelemetryDoesNotFireOrRecover(t *testing.T) {
	o, s, now := alertFixture()
	for i := 0; i < 5; i++ {
		o.evaluate(context.Background(), sampleSnapshot(*now, 1400, 0))
		*now += 10
	}
	if len(s.messages) != 0 {
		t.Fatal("no samples triggered")
	}
	for i := 0; i < 3; i++ {
		o.evaluate(context.Background(), sampleSnapshot(*now, 1400, 5))
		*now += 10
	}
	if len(s.messages) != 1 {
		t.Fatal("valid alarm missing")
	}
	for i := 0; i < 5; i++ {
		snapshot := sampleSnapshot(*now-70, 0, 10)
		o.evaluate(context.Background(), snapshot)
		*now += 10
	}
	if len(s.messages) != 1 {
		t.Fatal("stale zero falsely recovered")
	}
	found := false
	for _, a := range o.state.Active {
		if a.Kind == "client_wait_high" {
			found = a.State == "unknown" && a.Firing
		}
	}
	if !found {
		t.Fatal("firing incident disappeared during telemetry outage")
	}
}
func TestFailedDeliveryIsBoundedAndSecretFree(t *testing.T) {
	o, s, now := alertFixture()
	s.fail = true
	for i := 0; i < 8; i++ {
		o.evaluate(context.Background(), sampleSnapshot(*now, 1200, 5))
		*now += 10
	}
	if len(s.messages) != 2 {
		t.Fatalf("retry pacing calls=%d", len(s.messages))
	}
	raw, _ := json.Marshal(o.view())
	if strings.Contains(string(raw), fakeWebhook) || strings.Contains(string(raw), "must-not-leak") || !strings.Contains(string(raw), "feishu_delivery_failed") {
		t.Fatal("unsafe alert status")
	}
}
func TestOperationsCASPrivateStorageAndRestartPreserveCooldown(t *testing.T) {
	dir := t.TempDir()
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("chmod")
	}
	path := filepath.Join(dir, "operations.json")
	o, err := openOperations(path)
	if err != nil {
		t.Fatal(err)
	}
	now := int64(1700000000)
	o.now = func() time.Time { return time.Unix(now, 0) }
	sender := &recordingSender{}
	o.sender = sender
	cfg := defaultAlerts()
	cfg.Enabled = true
	cfg.FeishuEnabled = true
	cfg.HoldSeconds = 20
	cfg.CooldownSeconds = 60
	hook := fakeWebhook
	secret := "fixture-signing-secret"
	if o.update(AlertUpdate{ExpectedRevision: 1, AlertConfig: cfg, FeishuWebhook: &hook, FeishuSigningSecret: &secret}, "admin") != nil {
		t.Fatal("configuration save failed")
	}
	if o.update(AlertUpdate{ExpectedRevision: 1, AlertConfig: cfg}, "admin") == nil {
		t.Fatal("stale alert CAS accepted")
	}
	for i := 0; i < 3; i++ {
		o.evaluate(context.Background(), sampleSnapshot(now, 1200, 6))
		now += 10
	}
	if len(sender.messages) != 1 {
		t.Fatal("fixture incident missing")
	}
	if _, err := openOperations(path); err == nil {
		t.Fatal("two writers allowed")
	}
	o.closeFile()
	restored, err := openOperations(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.closeFile()
	restored.now = func() time.Time { return time.Unix(now, 0) }
	restored.sender = sender
	restored.evaluate(context.Background(), sampleSnapshot(now, 1200, 6))
	if len(sender.messages) != 1 {
		t.Fatal("restart forgot cooldown")
	}
	view, _ := json.Marshal(restored.view())
	if strings.Contains(string(view), hook) || strings.Contains(string(view), secret) {
		t.Fatal("API leaked notification credential")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("unsafe state permissions")
	}
	_ = os.WriteFile(path, []byte(`{"changed":true}`), 0600)
	if restored.update(AlertUpdate{ExpectedRevision: 2, AlertConfig: cfg}, "admin") == nil {
		t.Fatal("external edit overwritten")
	}
}
func TestFeishuConfigurationRejectsSSRFAndDisabledByDefault(t *testing.T) {
	o, _, _ := alertFixture()
	for _, bad := range []string{"http://open.feishu.cn/open-apis/bot/v2/hook/1234567890123456", "https://127.0.0.1/private", "https://open.feishu.cn.evil.test/open-apis/bot/v2/hook/1234567890123456", fakeWebhook + "?target=private", fakeWebhook + "#fragment", "https://open.feishu.cn@evil.test/open-apis/bot/v2/hook/1234567890123456"} {
		if validFeishuWebhook(bad) {
			t.Fatal("unsafe webhook allowed")
		}
	}
	cfg := defaultAlerts()
	if !cfg.Enabled || cfg.FeishuEnabled {
		t.Fatal("monitoring must default enabled, delivery disabled")
	}
	enabled := cfg
	enabled.Enabled = true
	enabled.FeishuEnabled = true
	empty := ""
	if o.update(AlertUpdate{ExpectedRevision: 1, AlertConfig: enabled, FeishuWebhook: &empty}, "admin") == nil {
		t.Fatal("enabled without destination")
	}
}

func TestMonitorWithoutBotTracksHoldAndRecoveryWithoutPretendingDelivery(t *testing.T) {
	o, s, now := alertFixture()
	o.state.Config.FeishuEnabled = false
	o.state.FeishuWebhook = ""
	for i := 0; i < 3; i++ {
		o.evaluate(context.Background(), sampleSnapshot(*now, 1400, 5))
		*now += 10
	}
	a := o.state.Active["client_wait_high:"+strings.Repeat("a", 32)]
	if a == nil || !a.Firing || len(s.messages) != 0 || a.LastNotifiedAt != 0 || a.LastAttemptAt != 0 || o.state.Delivery.LastErrorCode != "" {
		t.Fatal("monitor without bot did not retain local firing incident")
	}
	if len(o.state.History) != 1 || o.state.History[0].Event != "firing" {
		t.Fatal("local alert transition missing")
	}
	for i := 0; i < 3; i++ {
		o.evaluate(context.Background(), sampleSnapshot(*now, 100, 5))
		*now += 10
	}
	a = o.state.Active["client_wait_high:"+strings.Repeat("a", 32)]
	if a.Firing || a.State != "healthy" || a.RecoveryPending || len(s.messages) != 0 || len(o.state.History) != 2 {
		t.Fatal("local recovery generated a fake delivery")
	}
	cfg := defaultAlerts()
	cfg.HoldSeconds = 20
	cfg.CooldownSeconds = 60
	if o.update(AlertUpdate{ExpectedRevision: 1, AlertConfig: cfg}, "admin") != nil {
		t.Fatal("no-bot monitor settings rejected")
	}
	cfg.FeishuEnabled = true
	if o.update(AlertUpdate{ExpectedRevision: 2, AlertConfig: cfg}, "admin") == nil {
		t.Fatal("delivery enabled without a bot")
	}
	hook := fakeWebhook
	cfg.Enabled = false
	if o.update(AlertUpdate{ExpectedRevision: 2, AlertConfig: cfg, FeishuWebhook: &hook}, "admin") != nil {
		t.Fatal("cannot retain notification settings while monitoring disabled")
	}
	o.evaluate(context.Background(), sampleSnapshot(*now, 1400, 5))
	if len(s.messages) != 0 {
		t.Fatal("disabled monitoring sent a message")
	}
}
