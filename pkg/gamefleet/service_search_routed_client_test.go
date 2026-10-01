package gamefleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func routedServiceSearchTestPool() string { return "gfsp_" + strings.Repeat("a", 64) }

func routedServiceSearchTestData(t *testing.T, search Search, replay bool) []byte {
	t.Helper()
	rawSearch, err := json.Marshal(search)
	if err != nil {
		t.Fatal("marshal routed search fixture")
	}
	return []byte(fmt.Sprintf(`{"version":%q,"search":%s,"matchPoolId":%q,"replay":%t}`,
		ServiceSearchRoutedVersion, rawSearch, routedServiceSearchTestPool(), replay))
}

func routedServiceSearchTestStatusData(t *testing.T, search Search) []byte {
	t.Helper()
	rawSearch, err := json.Marshal(search)
	if err != nil {
		t.Fatal("marshal routed status fixture")
	}
	return []byte(fmt.Sprintf(`{"version":%q,"search":%s,"matchPoolId":%q}`,
		ServiceSearchRoutedVersion, rawSearch, routedServiceSearchTestPool()))
}

func routedServiceSearchTestEnvelope(data []byte) []byte {
	return []byte(fmt.Sprintf(`{"data":%s,"requestId":"routed-service-request-001"}`, data))
}

func routedServiceSearchWriteRaw(w http.ResponseWriter, status int, contentType string, raw []byte) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func TestRoutedServiceSearchClientConfigBoundsScopeAndWire(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/business/v1/history/service" || r.URL.RawQuery != "" ||
			r.Header.Get("Authorization") != "Bearer "+serviceSearchTestKey() || r.Header.Get("Accept") != "application/json" ||
			r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" {
			t.Error("routed scope request did not use the existing private service transport")
		}
		serviceSearchRespond(w, http.StatusOK, map[string]any{
			"serviceId": serviceSearchTestID, "applicationId": serviceSearchTestApp,
			"identityIssuer": serviceSearchTestIssuer, "operations": []string{"read"},
		})
	}))
	defer server.Close()

	cfg := serviceSearchTestConfig(server.URL)
	c, err := NewRoutedServiceSearchClient(cfg)
	if err != nil {
		t.Fatal("valid routed config rejected:", err)
	}
	t.Cleanup(c.Close)
	if err := c.CheckScope(context.Background()); err != nil || calls.Load() != 1 {
		t.Fatalf("routed scope check failed or used wrong request count: err=%v calls=%d", err, calls.Load())
	}

	for name, mutate := range map[string]func(*ServiceSearchConfig){
		"region byte limit":        func(c *ServiceSearchConfig) { c.Region = strings.Repeat("r", 65) },
		"compatibility byte limit": func(c *ServiceSearchConfig) { c.Compatibility = strings.Repeat("c", 65) },
		"wrong caller key class":   func(c *ServiceSearchConfig) { c.Key = "gfbiz_" + strings.Repeat("b", 43) },
		"wrong upload key class":   func(c *ServiceSearchConfig) { c.Key = "gfup_" + strings.Repeat("u", 43) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := cfg
			mutate(&bad)
			if client, err := NewRoutedServiceSearchClient(bad); err == nil {
				client.Close()
				t.Fatal("invalid routed configuration was accepted")
			}
		})
	}
	unicode := cfg
	unicode.Region = strings.Repeat("é", 32) // 64 UTF-8 bytes, not 32.
	if client, err := NewRoutedServiceSearchClient(unicode); err != nil {
		t.Fatal("64-byte UTF-8 region was rejected:", err)
	} else {
		client.Close()
	}
}

func TestRoutedServiceSearchClientBeginReplayAndStatusWire(t *testing.T) {
	const searchID = "search-routed-0001"
	beginFirst := routedServiceSearchTestEnvelope(routedServiceSearchTestData(t, serviceSearchTestPending(searchID), false))
	beginReplay := routedServiceSearchTestEnvelope(routedServiceSearchTestData(t, serviceSearchTestPending(searchID), true))
	statusResult := routedServiceSearchTestEnvelope(routedServiceSearchTestStatusData(t, serviceSearchTestPending(searchID)))
	var beginCalls, statusCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+serviceSearchTestKey() ||
			r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" ||
			r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" {
			t.Error("routed search request metadata mismatch")
		}
		raw, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/business/v2/service-searches":
			if r.Method != http.MethodPost {
				t.Error("routed begin used a non-POST method")
			}
			fields, err := routedExactObject(raw,
				[]string{"version", "participantId", "requestId", "region", "compatibility"}, nil)
			var version, participant, requestID, region, compatibility string
			if err != nil || json.Unmarshal(fields["version"], &version) != nil || json.Unmarshal(fields["participantId"], &participant) != nil ||
				json.Unmarshal(fields["requestId"], &requestID) != nil || json.Unmarshal(fields["region"], &region) != nil ||
				json.Unmarshal(fields["compatibility"], &compatibility) != nil || version != ServiceSearchRoutedVersion ||
				participant != serviceSearchTestUser || requestID != "stable-routed-request-001" || region != "us-west" || compatibility != "dm-v1" {
				t.Error("routed begin body did not match the exact v2 request schema")
			}
			if beginCalls.Add(1) == 1 {
				routedServiceSearchWriteRaw(w, http.StatusCreated, "application/json", beginFirst)
			} else {
				routedServiceSearchWriteRaw(w, http.StatusOK, "application/json", beginReplay)
			}
		case "/business/v2/service-searches/" + searchID + "/status":
			if r.Method != http.MethodPost {
				t.Error("routed status used a non-POST method")
			}
			fields, err := routedExactObject(raw, []string{"version", "participantId"}, nil)
			var version, participant string
			if err != nil || json.Unmarshal(fields["version"], &version) != nil || json.Unmarshal(fields["participantId"], &participant) != nil ||
				version != ServiceSearchRoutedVersion || participant != serviceSearchTestUser {
				t.Error("routed status body did not match the exact v2 request schema")
			}
			statusCalls.Add(1)
			routedServiceSearchWriteRaw(w, http.StatusOK, "application/json", statusResult)
		default:
			t.Errorf("routed client called unapproved path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewRoutedServiceSearchClient(serviceSearchTestConfig(server.URL))
	if err != nil {
		t.Fatal("create routed client:", err)
	}
	defer client.Close()

	first, err := client.BeginSearch(context.Background(), serviceSearchTestUser, "stable-routed-request-001")
	if err != nil || first.Version != ServiceSearchRoutedVersion || first.Replay || first.Search.ID != searchID || first.MatchPoolID != routedServiceSearchTestPool() {
		t.Fatalf("initial routed Begin response was invalid: result=%+v err=%v", first, err)
	}
	replay, err := client.BeginSearch(context.Background(), serviceSearchTestUser, "stable-routed-request-001")
	if err != nil || !replay.Replay || replay.Search.ID != searchID || replay.MatchPoolID != first.MatchPoolID {
		t.Fatalf("exact routed Begin replay was invalid: result=%+v err=%v", replay, err)
	}
	status, err := client.SearchStatus(context.Background(), searchID, serviceSearchTestUser)
	if err != nil || status.Version != ServiceSearchRoutedVersion || status.Search.ID != searchID || status.MatchPoolID != first.MatchPoolID {
		t.Fatalf("routed status was invalid: result=%+v err=%v", status, err)
	}
	if beginCalls.Load() != 2 || statusCalls.Load() != 1 {
		t.Fatalf("unexpected routed request count: begin=%d status=%d", beginCalls.Load(), statusCalls.Load())
	}
}

func TestRoutedServiceSearchClientRejectsClosedShapeAndCaseAliases(t *testing.T) {
	const searchID = "search-routed-0002"
	valid := routedServiceSearchTestData(t, serviceSearchTestPending(searchID), false)
	validEnvelope := routedServiceSearchTestEnvelope(valid)
	searchRaw, err := json.Marshal(serviceSearchTestPending(searchID))
	if err != nil {
		t.Fatal(err)
	}
	searchJSON := string(searchRaw)
	validJSON := string(valid)
	pool := routedServiceSearchTestPool()
	cases := map[string][]byte{
		"wrong version":           []byte(strings.Replace(validJSON, ServiceSearchRoutedVersion, "gamefleet.service-player-search.v1", 1)),
		"missing version":         []byte(strings.Replace(validJSON, `"version":"`+ServiceSearchRoutedVersion+`",`, "", 1)),
		"null version":            []byte(strings.Replace(validJSON, `"version":"`+ServiceSearchRoutedVersion+`"`, `"version":null`, 1)),
		"version case alias":      []byte(strings.Replace(validJSON, `"version"`, `"Version"`, 1)),
		"unknown caller":          []byte(strings.TrimSuffix(validJSON, "}") + `,"callerId":"forged"}`),
		"unknown placement":       []byte(strings.TrimSuffix(validJSON, "}") + `,"placementId":"forged"}`),
		"unknown additional pool": []byte(strings.TrimSuffix(validJSON, "}") + `,"otherPoolId":"forged"}`),
		"duplicate key":           []byte(strings.TrimSuffix(validJSON, "}") + `,"version":"` + ServiceSearchRoutedVersion + `"}`),
		"missing pool":            []byte(strings.Replace(validJSON, `,"matchPoolId":"`+pool+`"`, "", 1)),
		"null pool":               []byte(strings.Replace(validJSON, `"matchPoolId":"`+pool+`"`, `"matchPoolId":null`, 1)),
		"malformed pool":          []byte(strings.Replace(validJSON, pool, "gfsp_INVALID", 1)),
		"missing replay":          []byte(strings.Replace(validJSON, `,"replay":false`, "", 1)),
		"null replay":             []byte(strings.Replace(validJSON, `"replay":false`, `"replay":null`, 1)),
		"nonboolean replay":       []byte(strings.Replace(validJSON, `"replay":false`, `"replay":"false"`, 1)),
		"search field case alias": []byte(strings.Replace(validJSON, `"searchId"`, `"SearchId"`, 1)),
		"search unknown field":    []byte(strings.Replace(validJSON, `"search":`+searchJSON, `"search":`+strings.TrimSuffix(searchJSON, "}")+`,"callerId":"forged"}`, 1)),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				routedServiceSearchWriteRaw(w, http.StatusCreated, "application/json", routedServiceSearchTestEnvelope(data))
			}))
			defer server.Close()
			client, err := NewRoutedServiceSearchClient(serviceSearchTestConfig(server.URL))
			if err != nil {
				t.Fatal("create routed client")
			}
			defer client.Close()
			got, err := client.BeginSearch(context.Background(), serviceSearchTestUser, "routed-malformed-response-01")
			serviceSearchAssertError(t, err, http.StatusBadGateway, "")
			if got != (RoutedSearchResult{}) {
				t.Fatalf("malformed routed response returned partial result: %+v", got)
			}
		})
	}

	badEnvelopes := map[string][]byte{
		"envelope case alias": []byte(strings.Replace(string(validEnvelope), `"requestId"`, `"RequestId"`, 1)),
		"envelope unknown":    []byte(strings.TrimSuffix(string(validEnvelope), "}") + `,"extra":true}`),
		"request id null":     []byte(strings.Replace(string(validEnvelope), `"routed-service-request-001"`, "null", 1)),
	}
	for name, envelope := range badEnvelopes {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				routedServiceSearchWriteRaw(w, http.StatusCreated, "application/json", envelope)
			}))
			defer server.Close()
			client, err := NewRoutedServiceSearchClient(serviceSearchTestConfig(server.URL))
			if err != nil {
				t.Fatal("create routed client")
			}
			defer client.Close()
			_, err = client.BeginSearch(context.Background(), serviceSearchTestUser, "routed-malformed-envelope-01")
			serviceSearchAssertError(t, err, http.StatusBadGateway, "")
		})
	}
}

func TestRoutedServiceSearchClientRejectsSemanticMismatchAndStatusCodeMismatch(t *testing.T) {
	const searchID = "search-routed-0003"
	base := serviceSearchTestPending(searchID)
	cases := []struct {
		name   string
		search Search
		status int
		replay bool
	}{
		{"wrong region", func() Search { s := base; s.Region = "eu-west"; return s }(), http.StatusCreated, false},
		{"wrong compatibility", func() Search { s := base; s.Compatibility = "other-build"; return s }(), http.StatusCreated, false},
		{"TTL too short", func() Search { s := base; s.ExpiresAt = s.CreatedAt.Add(29 * time.Second); return s }(), http.StatusCreated, false},
		{"TTL too long", func() Search { s := base; s.ExpiresAt = s.CreatedAt.Add(601 * time.Second); return s }(), http.StatusCreated, false},
		{"created replay mismatch", base, http.StatusCreated, true},
		{"ok but not replay", base, http.StatusOK, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := routedServiceSearchTestData(t, tc.search, tc.replay)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				routedServiceSearchWriteRaw(w, tc.status, "application/json", routedServiceSearchTestEnvelope(data))
			}))
			defer server.Close()
			client, err := NewRoutedServiceSearchClient(serviceSearchTestConfig(server.URL))
			if err != nil {
				t.Fatal("create routed client")
			}
			defer client.Close()
			got, err := client.BeginSearch(context.Background(), serviceSearchTestUser, "routed-semantic-check-01")
			serviceSearchAssertError(t, err, http.StatusBadGateway, "")
			if got != (RoutedSearchResult{}) {
				t.Fatalf("semantic mismatch returned partial result: %+v", got)
			}
		})
	}

	for name, search := range map[string]Search{
		"status search id mismatch": func() Search { s := base; s.ID = "search-routed-different"; return s }(),
		"status wrong profile":      func() Search { s := base; s.Region = "eu-west"; return s }(),
		"status invalid ttl":        func() Search { s := base; s.ExpiresAt = s.CreatedAt.Add(601 * time.Second); return s }(),
	} {
		t.Run(name, func(t *testing.T) {
			data := routedServiceSearchTestStatusData(t, search)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				routedServiceSearchWriteRaw(w, http.StatusOK, "application/json", routedServiceSearchTestEnvelope(data))
			}))
			defer server.Close()
			client, err := NewRoutedServiceSearchClient(serviceSearchTestConfig(server.URL))
			if err != nil {
				t.Fatal("create routed client")
			}
			defer client.Close()
			got, err := client.SearchStatus(context.Background(), base.ID, serviceSearchTestUser)
			serviceSearchAssertError(t, err, http.StatusBadGateway, "")
			if got != (RoutedSearchStatus{}) {
				t.Fatalf("status mismatch returned partial result: %+v", got)
			}
		})
	}
}

func TestRoutedServiceSearchClientBoundsResponsesAndKeepsV1DTOClosed(t *testing.T) {
	const searchID = "search-routed-0004"
	validV2 := routedServiceSearchTestEnvelope(routedServiceSearchTestData(t, serviceSearchTestPending(searchID), false))
	for name, handler := range map[string]http.HandlerFunc{
		"oversized success": func(w http.ResponseWriter, _ *http.Request) {
			routedServiceSearchWriteRaw(w, http.StatusCreated, "application/json", bytes.Repeat([]byte(" "), serviceSearchBodyLimit+1))
		},
		"wrong content type": func(w http.ResponseWriter, _ *http.Request) {
			routedServiceSearchWriteRaw(w, http.StatusCreated, "text/plain", validV2)
		},
		"redirect is not followed": func(w http.ResponseWriter, _ *http.Request) {
			http.Redirect(w, httptest.NewRequest(http.MethodPost, "/elsewhere", nil), "/elsewhere", http.StatusFound)
		},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				handler(w, r)
			}))
			defer server.Close()
			client, err := NewRoutedServiceSearchClient(serviceSearchTestConfig(server.URL))
			if err != nil {
				t.Fatal("create routed client")
			}
			defer client.Close()
			_, err = client.BeginSearch(context.Background(), serviceSearchTestUser, "routed-response-bounds-01")
			if err == nil || calls.Load() != 1 {
				t.Fatalf("invalid response accepted or redirect followed: err=%v calls=%d", err, calls.Load())
			}
		})
	}

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/business/v1/service-searches" {
			t.Errorf("legacy v1 client changed path to %q", r.URL.Path)
		}
		serviceSearchRespond(w, http.StatusCreated, map[string]any{
			"version": ServiceSearchRoutedVersion, "search": serviceSearchTestPending(searchID),
			"matchPoolId": routedServiceSearchTestPool(), "replay": false,
		})
	}))
	defer server.Close()
	v1, err := NewServiceSearchClient(serviceSearchTestConfig(server.URL))
	if err != nil {
		t.Fatal("create v1 client")
	}
	defer v1.Close()
	got, err := v1.BeginSearch(context.Background(), serviceSearchTestUser, "legacy-v1-request-0001")
	serviceSearchAssertError(t, err, http.StatusBadGateway, "")
	if got != (SearchResult{}) || calls.Load() != 1 {
		t.Fatalf("v1 DTO adopted routed response: result=%+v calls=%d", got, calls.Load())
	}
}

func TestRoutedServiceSearchClientRejectsInvalidLocalInputsWithoutNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		serviceSearchRespond(w, http.StatusCreated, map[string]any{
			"version": ServiceSearchRoutedVersion, "search": serviceSearchTestPending("search-routed-0005"),
			"matchPoolId": routedServiceSearchTestPool(), "replay": false,
		})
	}))
	defer server.Close()
	client, err := NewRoutedServiceSearchClient(serviceSearchTestConfig(server.URL))
	if err != nil {
		t.Fatal("create routed client")
	}
	defer client.Close()
	for _, input := range []struct {
		participant string
		requestID   string
	}{
		{strings.Repeat("p", 257), "routed-local-input-01"},
		{"player-one", "short"},
		{"", "routed-local-input-02"},
	} {
		if _, err := client.BeginSearch(context.Background(), input.participant, input.requestID); err == nil {
			t.Fatal("invalid routed begin input reached success")
		}
	}
	for _, searchID := range []string{"bad/id", "short", strings.Repeat("i", 129)} {
		if _, err := client.SearchStatus(context.Background(), searchID, serviceSearchTestUser); err == nil {
			t.Fatal("invalid routed status ID reached success")
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid local input made %d network calls", calls.Load())
	}
}

func TestRoutedServiceSearchErrorDoesNotExposeWireBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serviceSearchRespondError(w, http.StatusUnauthorized, "credential-secret-marker")
	}))
	defer server.Close()
	client, err := NewRoutedServiceSearchClient(serviceSearchTestConfig(server.URL))
	if err != nil {
		t.Fatal("create routed client")
	}
	defer client.Close()
	_, err = client.BeginSearch(context.Background(), serviceSearchTestUser, "routed-credential-error-01")
	serviceSearchAssertError(t, err, http.StatusUnauthorized, "credential-secret-marker")
	if err == nil || strings.Contains(err.Error(), "credential-secret-marker") {
		t.Fatal("routed error exposed response body")
	}
}
