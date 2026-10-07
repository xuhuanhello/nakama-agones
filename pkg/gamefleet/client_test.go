package gamefleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func clientConfig(origin string) Config {
	return Config{TLS: fixtureTLS, URL: origin, Key: "gfbiz_" + strings.Repeat("a", 43), ApplicationID: "app-one", PlacementID: "placement-one", RevisionID: "revision-one", Region: "us-west", Compatibility: "dm-v1"}
}
func testReservation() Reservation {
	now := time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC)
	return Reservation{ReservationID: "reservation-one", AllocationID: "allocation-one", RoomID: "room-one", ApplicationID: "app-one", PlacementID: "placement-one", RevisionID: "revision-one", Region: "us-west", State: "prepared", CreatedAt: now, UpdatedAt: now}
}
func testAssignment(previous int64) Assignment {
	return Assignment{Ticket: Ticket{ID: "ticket-one", State: "issued", Token: "gft1.YQ." + strings.Repeat("a", 86), Generation: previous + 1, ExpiresAt: time.Now().Add(time.Minute)}, Endpoint: &Endpoint{Address: "203.0.113.7", Ports: []Port{{Name: "game", Protocol: "UDP", Port: 20000}}}}
}
func respond(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": data, "requestId": "http-test"})
}
func mustClient(t *testing.T, origin string) *Client {
	t.Helper()
	c, err := NewClient(clientConfig(origin))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}
func statusError(t *testing.T, err error, want int) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Status != want {
		t.Fatalf("error = %v, want status %d", err, want)
	}
}

func TestClientRejectsNonLoopbackAndAmbiguousOrigins(t *testing.T) {
	for _, origin := range []string{"", "http://127.0.0.1:17682", "http://localhost:17682", "http://192.0.2.1:17682", "http://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:99999", "http://127.0.0.1:017682", "http://user:pass@127.0.0.1:17682", "http://127.0.0.1:17682/business", "http://127.0.0.1:17682?x=1", "http://127.0.0.1:17682?", "http://127.0.0.1:17682/#x", "http://[::1%25lo0]:17682", "http://127.0.0.1:17682/%2f"} {
		t.Run(origin, func(t *testing.T) {
			if c, err := NewClient(clientConfig(origin)); err == nil {
				c.Close()
				t.Fatal("invalid origin accepted")
			}
		})
	}
	for _, origin := range []string{"https://127.0.0.1:17682", "https://[::1]:17682/", "https://business.example.net"} {
		c, err := NewClient(clientConfig(origin))
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	for _, change := range []func(*Config){func(c *Config) { c.ApplicationID = "../app" }, func(c *Config) { c.PlacementID = "" }, func(c *Config) { c.RevisionID = "bad revision" }, func(c *Config) { c.Region = " west" }, func(c *Config) { c.Compatibility = "v1\n" }, func(c *Config) { c.Key = "secret\nheader" }} {
		cfg := clientConfig("https://127.0.0.1:17682")
		change(&cfg)
		if c, err := NewClient(cfg); err == nil {
			c.Close()
			t.Fatal("invalid config accepted")
		}
	}
}

func TestClientHTTPContractAndExactReplay(t *testing.T) {
	var bodies [][]byte
	server := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+clientConfig("").Key || r.URL.RawQuery != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" {
			t.Error("unexpected credential or ambient authority")
		}
		if r.Method != "GET" && r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing content type")
		}
		var payload map[string]any
		if r.Method != "GET" {
			if json.NewDecoder(r.Body).Decode(&payload) != nil {
				t.Fatal("invalid request JSON")
			}
		}
		switch r.URL.Path {
		case "/business/v1/caller":
			respond(w, Scope{CallerID: "caller-one", ApplicationID: "app-one", PlacementID: "placement-one", RevisionID: "revision-one", Region: "us-west", Operations: []string{"reserve", "read", "cancel", "assignment", "resume"}})
		case "/business/v1/reservations/current":
			if !reflect.DeepEqual(payload, map[string]any{"version": RoomVersion, "participantId": "player-one"}) {
				t.Errorf("current payload: %v", payload)
			}
			respond(w, CurrentResult{Current: &Current{Reservation: testReservation(), ConnectionGeneration: 2}})
		case "/business/v1/reservations/allocation-one/status":
			if r.Method != http.MethodPost || !reflect.DeepEqual(payload, map[string]any{"version": RoomVersion, "participantId": "player-one"}) {
				t.Errorf("status request: method=%s payload=%v", r.Method, payload)
			}
			respond(w, ReservationStatus{Reservation: testReservation()})
		case "/business/v1/searches/match":
			expected := map[string]any{
				"version": SearchVersion, "compatibility": "dm-v1", "idempotencyKey": "request-1234",
				"members": []any{
					map[string]any{"participantId": "player-one", "searchId": "search-player-one", "nakamaTicket": "ticket-one"},
					map[string]any{"participantId": "player-two", "searchId": "search-player-two", "nakamaTicket": "ticket-two"},
				},
			}
			if !reflect.DeepEqual(payload, expected) {
				t.Errorf("search match payload: %v", payload)
			}
			respond(w, ReservationResult{Reservation: testReservation()})
		case "/business/v1/reservations/allocation-one/resume":
			expected := map[string]any{"version": TicketVersion, "participantId": "player-one", "idempotencyKey": "attempt-1234", "previousConnectionGeneration": float64(2)}
			if !reflect.DeepEqual(payload, expected) {
				t.Errorf("resume payload: %v", payload)
			}
			raw, _ := json.Marshal(payload)
			bodies = append(bodies, raw)
			respond(w, AssignmentResult{Assignment: testAssignment(2), Replay: len(bodies) > 1})
		case "/business/v1/reservations/allocation-one/cancel":
			if len(payload) != 0 {
				t.Error("cancel body must be empty")
			}
			r := testReservation()
			r.CancellationRequested = true
			respond(w, ReservationResult{Reservation: r})
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := mustClient(t, server.URL)
	ctx := context.Background()
	if err := c.CheckScope(ctx); err != nil {
		t.Fatal(err)
	}
	cur, err := c.Current(ctx, "player-one")
	if err != nil || cur.Current.ConnectionGeneration != 2 {
		t.Fatalf("current: %v", err)
	}
	status, err := c.Status(ctx, "allocation-one", "player-one")
	if err != nil || status.Reservation.State != "prepared" {
		t.Fatalf("status: %+v %v", status, err)
	}
	members := []SearchMatchMember{
		{ParticipantID: "player-one", SearchID: "search-player-one", NakamaTicket: "ticket-one"},
		{ParticipantID: "player-two", SearchID: "search-player-two", NakamaTicket: "ticket-two"},
	}
	if _, err = c.MatchSearches(ctx, "request-1234", members); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = c.Issue(ctx, "allocation-one", "player-one", "attempt-1234", 2, true); err != nil {
			t.Fatal(err)
		}
	}
	if len(bodies) != 2 || string(bodies[0]) != string(bodies[1]) {
		t.Fatal("retry changed wire identity")
	}
	if _, err = c.Cancel(ctx, "allocation-one"); err != nil {
		t.Fatal(err)
	}
}

func TestClientRejectsRedirectAndIgnoresProxy(t *testing.T) {
	var captured atomic.Int64
	sink := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { captured.Add(1); respond(w, CurrentResult{}) }))
	defer sink.Close()
	t.Setenv("HTTP_PROXY", sink.URL)
	t.Setenv("ALL_PROXY", sink.URL)
	t.Setenv("NO_PROXY", "")
	origin := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, sink.URL, 307) }))
	defer origin.Close()
	c := mustClient(t, origin.URL)
	_, err := c.Current(context.Background(), "player-one")
	statusError(t, err, 503)
	if captured.Load() != 0 || c.http.Transport.(*http.Transport).Proxy != nil || c.http.Jar != nil {
		t.Fatal("credential could escape to redirect/proxy/cookie jar")
	}
}

func TestClientMalformedResponsesAndScopeChangesFailClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(CurrentResult) any
		raw    string
		media  string
	}{
		{name: "missing", raw: `{"data":{},"requestId":"x"}`},
		{name: "null envelope", raw: `{"data":null,"requestId":"x"}`},
		{name: "trailing", raw: `{"data":{"current":null}} {}`},
		{name: "unknown", raw: `{"data":{"current":null,"token":"secret"}}`},
		{name: "wrong media", raw: `{"data":{"current":null}}`, media: "text/plain"},
		{name: "too large", raw: strings.Repeat("x", (64<<10)+1)},
		{name: "foreign application", mutate: func(r CurrentResult) any { r.Current.Reservation.ApplicationID = "other"; return r }},
		{name: "foreign placement", mutate: func(r CurrentResult) any { r.Current.Reservation.PlacementID = "other"; return r }},
		{name: "foreign revision", mutate: func(r CurrentResult) any { r.Current.Reservation.RevisionID = "other"; return r }},
		{name: "foreign region", mutate: func(r CurrentResult) any { r.Current.Reservation.Region = "other"; return r }},
		{name: "completed held", mutate: func(r CurrentResult) any { r.Current.Reservation.State = "completed"; return r }},
		{name: "technical abort held", mutate: func(r CurrentResult) any {
			r.Current.Reservation.State = "technical_aborted"
			r.Current.Reservation.FailureCode = "host_process_terminated"
			return r
		}},
		{name: "missing generation", mutate: func(r CurrentResult) any {
			return map[string]any{"current": map[string]any{"reservation": r.Current.Reservation}}
		}},
		{name: "bad generation", mutate: func(r CurrentResult) any { r.Current.ConnectionGeneration = MaxGeneration + 1; return r }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.raw != "" {
					media := test.media
					if media == "" {
						media = "application/json"
					}
					w.Header().Set("Content-Type", media)
					fmt.Fprint(w, test.raw)
					return
				}
				respond(w, test.mutate(CurrentResult{Current: &Current{Reservation: testReservation()}}))
			}))
			defer s.Close()
			_, err := mustClient(t, s.URL).Current(context.Background(), "player-one")
			statusError(t, err, 502)
		})
	}
}

func TestClientScopePreflight(t *testing.T) {
	for _, field := range []string{"application", "placement", "revision", "region", "permission"} {
		t.Run(field, func(t *testing.T) {
			s := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				scope := Scope{CallerID: "caller-one", ApplicationID: "app-one", PlacementID: "placement-one", RevisionID: "revision-one", Region: "us-west", Operations: []string{"read", "reserve", "cancel", "assignment", "resume"}}
				switch field {
				case "application":
					scope.ApplicationID = "other"
				case "placement":
					scope.PlacementID = "other"
				case "revision":
					scope.RevisionID = "other"
				case "region":
					scope.Region = "other"
				case "permission":
					scope.Operations = []string{"read"}
				}
				respond(w, scope)
			}))
			defer s.Close()
			if err := mustClient(t, s.URL).CheckScope(context.Background()); err == nil {
				t.Fatal("wrong scope accepted")
			}
		})
	}
}

func TestClientErrorsAreSanitizedAndRequestsAreBounded(t *testing.T) {
	for _, code := range []int{301, 401, 403, 404, 409, 422, 429, 500, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			s := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				fmt.Fprint(w, "credential-secret ticket-secret arbitrary-error")
			}))
			defer s.Close()
			_, err := mustClient(t, s.URL).Current(context.Background(), "player-one")
			want := code
			if code == 301 || code >= 500 {
				want = 503
			}
			statusError(t, err, want)
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("server body leaked")
			}
		})
	}
	s := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer s.Close()
	c := mustClient(t, s.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.Current(ctx, "player-one")
	statusError(t, err, 503)
	if c.http.Timeout <= 0 || c.http.Timeout > 10*time.Second {
		t.Fatal("missing request timeout")
	}
}

func TestClientRejectsInvalidRequestsBeforeHTTP(t *testing.T) {
	var calls atomic.Int64
	s := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer s.Close()
	c := mustClient(t, s.URL)
	ctx := context.Background()
	_, e := c.Current(ctx, " player-one")
	statusError(t, e, 422)
	members := []SearchMatchMember{{ParticipantID: "player-one", SearchID: "search-player-one", NakamaTicket: "ticket-one"}, {ParticipantID: "player-two", SearchID: "search-player-two", NakamaTicket: "ticket-two"}}
	_, e = c.MatchSearches(ctx, "bad:key", members)
	statusError(t, e, 422)
	_, e = c.MatchSearches(ctx, "request-1234", members[:1])
	statusError(t, e, 422)
	duplicate := append([]SearchMatchMember(nil), members...)
	duplicate[1].ParticipantID = duplicate[0].ParticipantID
	_, e = c.MatchSearches(ctx, "request-1234", duplicate)
	statusError(t, e, 422)
	_, e = c.Cancel(ctx, "../other")
	statusError(t, e, 422)
	_, e = c.Status(ctx, "../other", "player-one")
	statusError(t, e, 422)
	_, e = c.Status(ctx, "allocation-one", " player-one")
	statusError(t, e, 422)
	for _, v := range []struct {
		gen    int64
		resume bool
	}{{1, false}, {0, true}, {-1, true}, {MaxGeneration, true}} {
		_, e = c.Issue(ctx, "allocation-one", "player-one", "attempt-1234", v.gen, v.resume)
		statusError(t, e, 422)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid request reached HTTP")
	}
}

func TestClientStatusValidatesAbortedCodeScopeAllocationAndStrictEnvelope(t *testing.T) {
	validAborted := testReservation()
	validAborted.State = "technical_aborted"
	validAborted.FailureCode = "host_process_terminated"
	s := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respond(w, ReservationStatus{Reservation: validAborted})
	}))
	out, err := mustClient(t, s.URL).Status(context.Background(), "allocation-one", "player-one")
	s.Close()
	if err != nil || out.Reservation.State != "technical_aborted" || out.Reservation.FailureCode != "host_process_terminated" {
		t.Fatalf("valid technical status rejected: %+v %v", out, err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*ReservationStatus) any
	}{
		{name: "wrong allocation", mutate: func(s *ReservationStatus) any { s.Reservation.AllocationID = "another-allocation"; return s }},
		{name: "foreign application", mutate: func(s *ReservationStatus) any { s.Reservation.ApplicationID = "other"; return s }},
		{name: "foreign placement", mutate: func(s *ReservationStatus) any { s.Reservation.PlacementID = "other"; return s }},
		{name: "foreign revision", mutate: func(s *ReservationStatus) any { s.Reservation.RevisionID = "other"; return s }},
		{name: "foreign region", mutate: func(s *ReservationStatus) any { s.Reservation.Region = "other"; return s }},
		{name: "unknown state", mutate: func(s *ReservationStatus) any { s.Reservation.State = "aborted"; return s }},
		{name: "missing aborted code", mutate: func(s *ReservationStatus) any { s.Reservation.State = "technical_aborted"; return s }},
		{name: "wrong aborted code", mutate: func(s *ReservationStatus) any {
			s.Reservation.State = "technical_aborted"
			s.Reservation.FailureCode = "other"
			return s
		}},
		{name: "failure code on normal state", mutate: func(s *ReservationStatus) any { s.Reservation.FailureCode = "host_process_terminated"; return s }},
		{name: "replay member", mutate: func(s *ReservationStatus) any { return map[string]any{"reservation": s.Reservation, "replay": false} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reservation := ReservationStatus{Reservation: testReservation()}
				respond(w, tc.mutate(&reservation))
			}))
			defer server.Close()
			_, err := mustClient(t, server.URL).Status(context.Background(), "allocation-one", "player-one")
			statusError(t, err, 502)
		})
	}
}

func TestClientAssignmentReplayShapes(t *testing.T) {
	for _, state := range []string{"issued", "expired", "superseded"} {
		t.Run(state, func(t *testing.T) {
			a := testAssignment(2)
			a.Ticket.State = state
			if state != "issued" {
				a.Ticket.Token = ""
				a.Endpoint = nil
			}
			s := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				respond(w, AssignmentResult{Assignment: a, Replay: true})
			}))
			defer s.Close()
			out, err := mustClient(t, s.URL).Issue(context.Background(), "allocation-one", "player-one", "attempt-1234", 2, true)
			if err != nil || out.Assignment.Ticket.State != state || !out.Replay {
				t.Fatalf("%+v %v", out, err)
			}
		})
	}
	for _, change := range []func(*Assignment){func(a *Assignment) { a.Ticket.Generation = 9 }, func(a *Assignment) { a.Ticket.Token = "" }, func(a *Assignment) { a.Ticket.Token = "gft1.invalid" }, func(a *Assignment) { a.Endpoint = nil }, func(a *Assignment) { a.Endpoint.Address = "url.invalid" }, func(a *Assignment) { a.Endpoint.Address = "127.0.0.1" }, func(a *Assignment) { a.Endpoint.Address = "169.254.1.1" }, func(a *Assignment) { a.Endpoint.Ports[0].Port = 0 }, func(a *Assignment) { a.Endpoint.Ports[0].Protocol = "HTTP" }, func(a *Assignment) { a.Ticket.State = "superseded" }} {
		a := testAssignment(2)
		change(&a)
		if validAssignment(a, 3) {
			t.Fatal("malformed assignment accepted")
		}
	}
}

func TestConfigPrivateKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "business.key")
	t.Setenv("GAMEFLEET_BUSINESS_KEY_FILE", path)
	t.Setenv("GAMEFLEET_BUSINESS_URL", "http://127.0.0.1:17682")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("missing key accepted")
	}
	if err := os.WriteFile(path, []byte(clientConfig("").Key+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ConfigFromEnv()
	if err != nil || cfg.Key != clientConfig("").Key {
		t.Fatal("private newline key failed", err)
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = ConfigFromEnv(); err == nil {
		t.Fatal("world-readable key accepted")
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := path + ".link"
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GAMEFLEET_BUSINESS_KEY_FILE", link)
	if _, err = ConfigFromEnv(); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestRequiredLifecycleAndReplayFields(t *testing.T) {
	r := testReservation()
	raw, _ := json.Marshal(r)
	var fields map[string]any
	json.Unmarshal(raw, &fields)
	delete(fields, "cancellationRequested")
	raw, _ = json.Marshal(fields)
	var decoded Reservation
	if strictObject(raw, &decoded) == nil {
		t.Fatal("missing cancellation flag accepted")
	}
	for _, body := range []string{`{"reservation":{}}`, `{"reservation":{},"replay":null}`} {
		var out ReservationResult
		if strictObject([]byte(body), &out) == nil {
			t.Fatal("missing replay accepted")
		}
	}
	a := testAssignment(2)
	a.Ticket.State = "expired"
	a.Ticket.Token = ""
	a.Endpoint = nil
	for _, replay := range []any{nil, false} {
		s := newBusinessTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			respond(w, map[string]any{"assignment": a, "replay": replay})
		}))
		c := mustClient(t, s.URL)
		_, err := c.Issue(context.Background(), "allocation-one", "player-one", "attempt-1234", 2, true)
		statusError(t, err, 502)
		s.Close()
	}
}

func TestReservationFailureCodeRejectsNullWireValue(t *testing.T) {
	fields := map[string]any{}
	raw, err := json.Marshal(testReservation())
	if err != nil || json.Unmarshal(raw, &fields) != nil {
		t.Fatal("marshal reservation fixture", err)
	}
	fields["failureCode"] = nil
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal("marshal null failure code", err)
	}
	var reservation Reservation
	if strictObject(raw, &reservation) == nil {
		t.Fatal("explicit null failureCode silently became an absent value")
	}

	fields["failureCode"] = "host_process_terminated"
	raw, err = json.Marshal(fields)
	if err != nil || strictObject(raw, &reservation) != nil || reservation.FailureCode != "host_process_terminated" {
		t.Fatalf("string failureCode did not decode: value=%q err=%v", reservation.FailureCode, err)
	}
	delete(fields, "failureCode")
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal("marshal absent failure code", err)
	}
	reservation.FailureCode = "stale-value"
	if strictObject(raw, &reservation) != nil || reservation.FailureCode != "" {
		t.Fatalf("absent failureCode retained a stale decode value: %+v", reservation)
	}
}
