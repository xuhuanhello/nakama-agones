package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHistoryCursorIsBoundedAndDoesNotBecomeProxyInput(t *testing.T) {
	boundary := time.Now().Add(-2 * time.Minute).UTC()
	var calls atomic.Int32
	u, _ := testUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		if !strings.HasSuffix(r.URL.Path, "/proxy/loki/api/v1/query_range") || q.Get("direction") != "backward" || q.Get("limit") != "2" {
			t.Error("cursor changed the fixed history request")
		}
		if q.Get("end") != strconv.FormatInt(boundary.Add(-time.Nanosecond).UnixNano(), 10) {
			t.Error("cursor did not exclude its boundary timestamp")
		}
		start, err := strconv.ParseInt(q.Get("start"), 10, 64)
		if err != nil || time.Unix(0, start).Before(time.Now().Add(-61*time.Minute)) {
			t.Error("cursor widened the retention window")
		}
		values := [][]string{{strconv.FormatInt(boundary.Add(-time.Second).UnixNano(), 10), "newer"}, {strconv.FormatInt(boundary.Add(-2*time.Second).UnixNano(), 10), "older"}}
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "streams", "result": []any{map[string]any{"values": values}}}})
	})
	d := &DataSource{cfg: SourceConfig{LogRetentionDays: 7}, regions: []regionSource{{cfg: RegionConfig{Name: "us-west", Namespace: "agones-games", SystemNamespace: "agones-system", LogNamespace: "agones-observability", LogService: "fleet-loki"}, up: u}}}
	q := url.Values{"region": {"us-west"}, "mode": {"history"}, "pod": {"nag-" + strings.Repeat("b", 32)}, "limit": {"2"}, "before": {boundary.Format(time.RFC3339Nano)}}
	value, err := d.Logs(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	result := value.(map[string]any)
	if result["truncated"] != true || result["next_before"] != boundary.Add(-2*time.Second).Format(time.RFC3339Nano) {
		t.Fatal("history cursor did not identify the oldest returned row")
	}
	for _, bad := range []string{"not-a-timestamp", time.Now().Add(time.Hour).Format(time.RFC3339Nano), time.Now().Add(-2 * time.Hour).Format(time.RFC3339Nano)} {
		q.Set("before", bad)
		if _, err := d.Logs(context.Background(), q); err == nil || err.Error() != "invalid_query" {
			t.Error("invalid history boundary was accepted")
		}
	}
	q.Set("before", boundary.Format(time.RFC3339Nano))
	q.Set("mode", "live")
	if _, err := d.Logs(context.Background(), q); err == nil {
		t.Fatal("live logs accepted a history cursor")
	}
	if calls.Load() != 1 {
		t.Fatal("rejected cursor reached upstream")
	}
}
