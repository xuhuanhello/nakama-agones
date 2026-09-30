package gamefleet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func clientPendingSearch() Search {
	created := testReservation().CreatedAt.Add(-time.Second)
	return Search{ID: "search-player-one", State: "pending", Region: "us-west", Compatibility: "dm-v1", CreatedAt: created, ExpiresAt: created.Add(120 * time.Second)}
}

func clientBoundSearch() Search {
	s := clientPendingSearch()
	r := testReservation()
	s.State, s.AllocationID, s.Reservation = "bound", r.AllocationID, &r
	s.ResolvedAt = &r.CreatedAt
	return s
}

func TestClientSearchRoutesPreserveWireIdentityAndBoundCancellation(t *testing.T) {
	pending, bound := clientPendingSearch(), clientBoundSearch()
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+clientConfig("").Key || r.URL.RawQuery != "" {
			t.Error("search request changed transport or authority")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		want := map[string]any{"version": SearchVersion, "participantId": "player-one"}
		switch r.URL.Path {
		case "/business/v1/searches":
			want["requestId"], want["compatibility"] = "request-one", "dm-v1"
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(201)
			respond(w, SearchResult{Search: pending})
		case "/business/v1/searches/search-player-one/status":
			respond(w, SearchStatus{Search: pending})
		case "/business/v1/searches/search-player-one/cancel":
			// Losing the cancellation race must return the original bound room,
			// not send an allocation cancel or claim that the seat was released.
			respond(w, SearchResult{Search: bound, Replay: true})
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(404)
		}
		if !reflect.DeepEqual(body, want) {
			t.Errorf("payload %v, want %v", body, want)
		}
	}))
	defer server.Close()
	c := mustClient(t, server.URL)
	result, err := c.BeginSearch(context.Background(), "player-one", "request-one")
	if err != nil || result.Replay || !reflect.DeepEqual(result.Search, pending) {
		t.Fatalf("begin: %+v %v", result, err)
	}
	status, err := c.SearchStatus(context.Background(), pending.ID, "player-one")
	if err != nil || !reflect.DeepEqual(status.Search, pending) {
		t.Fatalf("status: %+v %v", status, err)
	}
	cancel, err := c.CancelSearch(context.Background(), pending.ID, "player-one")
	if err != nil || !cancel.Replay || !reflect.DeepEqual(cancel.Search, bound) {
		t.Fatalf("bound cancel: %+v %v", cancel, err)
	}
	if len(paths) != 3 {
		t.Fatalf("unexpected extra request: %v", paths)
	}
}

func TestClientSearchLostResponsesRequireExplicitExactRetry(t *testing.T) {
	for _, operation := range []string{"begin", "match"} {
		t.Run(operation, func(t *testing.T) {
			var bodies []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				bodies = append(bodies, string(raw))
				if len(bodies) == 1 {
					// Model a commit followed by an unavailable reply. The bridge
					// must not invent a different search/match identity or retry.
					w.WriteHeader(503)
					fmt.Fprint(w, "private-upstream-detail")
					return
				}
				if operation == "begin" {
					respond(w, SearchResult{Search: clientPendingSearch(), Replay: true})
				} else {
					respond(w, ReservationResult{Reservation: testReservation(), Replay: true})
				}
			}))
			defer server.Close()
			c := mustClient(t, server.URL)
			call := func() (bool, error) {
				if operation == "begin" {
					r, err := c.BeginSearch(context.Background(), "player-one", "request-one")
					return r.Replay, err
				}
				r, err := c.MatchSearches(context.Background(), "match-request-one", []SearchMatchMember{
					{ParticipantID: "player-one", SearchID: "search-player-one", NakamaTicket: "ticket-one"},
					{ParticipantID: "player-two", SearchID: "search-player-two", NakamaTicket: "ticket-two"},
				})
				return r.Replay, err
			}
			_, err := call()
			statusError(t, err, 503)
			if strings.Contains(err.Error(), "private-upstream-detail") || len(bodies) != 1 {
				t.Fatal("error leaked data or silently retried")
			}
			replay, err := call()
			if err != nil || !replay || len(bodies) != 2 || bodies[0] != bodies[1] {
				t.Fatalf("exact retry: bodies=%v replay=%v error=%v", bodies, replay, err)
			}
		})
	}
}

func TestClientSearchStatusValidatesDurableLifecycleFacts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Search)
		valid  bool
	}{
		{"pending", func(*Search) {}, true},
		{"cancelled", func(s *Search) { now := s.CreatedAt.Add(time.Second); s.State, s.ResolvedAt = "cancelled", &now }, true},
		{"expired", func(s *Search) { now := s.ExpiresAt; s.State, s.ResolvedAt = "expired", &now }, true},
		{"bound", func(s *Search) { *s = clientBoundSearch() }, true},
		{"completed bound", func(s *Search) {
			*s = clientBoundSearch()
			s.Reservation.State = "completed"
			s.Reservation.UpdatedAt = s.Reservation.CreatedAt.Add(time.Hour)
		}, true},
		{"aborted bound", func(s *Search) {
			*s = clientBoundSearch()
			s.Reservation.State, s.Reservation.FailureCode = "technical_aborted", "host_process_terminated"
		}, true},
		{"missing id", func(s *Search) { s.ID = "" }, false},
		{"different id", func(s *Search) { s.ID = "search-player-other" }, false},
		{"wrong region", func(s *Search) { s.Region = "other" }, false},
		{"wrong compatibility", func(s *Search) { s.Compatibility = "other" }, false},
		{"created missing", func(s *Search) { s.CreatedAt = time.Time{} }, false},
		{"created before epoch", func(s *Search) { s.CreatedAt = time.Unix(-1, 0); s.ExpiresAt = s.CreatedAt.Add(time.Minute) }, false},
		{"short ttl", func(s *Search) { s.ExpiresAt = s.CreatedAt.Add(29 * time.Second) }, false},
		{"long ttl", func(s *Search) { s.ExpiresAt = s.CreatedAt.Add(601 * time.Second) }, false},
		{"fractional ttl", func(s *Search) { s.ExpiresAt = s.ExpiresAt.Add(time.Millisecond) }, false},
		{"unknown state", func(s *Search) { s.State = "released" }, false},
		{"pending resolved", func(s *Search) { now := s.CreatedAt; s.ResolvedAt = &now }, false},
		{"pending allocation", func(s *Search) { s.AllocationID = "allocation-one" }, false},
		{"pending reservation", func(s *Search) { r := testReservation(); s.Reservation = &r }, false},
		{"cancel without resolution", func(s *Search) { s.State = "cancelled" }, false},
		{"cancel at expiry", func(s *Search) { now := s.ExpiresAt; s.State, s.ResolvedAt = "cancelled", &now }, false},
		{"expired too early", func(s *Search) { now := s.ExpiresAt.Add(-time.Millisecond); s.State, s.ResolvedAt = "expired", &now }, false},
		{"resolution before begin", func(s *Search) { now := s.CreatedAt.Add(-time.Millisecond); s.State, s.ResolvedAt = "cancelled", &now }, false},
		{"bound without reservation", func(s *Search) { *s = clientBoundSearch(); s.Reservation = nil }, false},
		{"bound mismatched allocation", func(s *Search) { *s = clientBoundSearch(); s.AllocationID = "allocation-other" }, false},
		{"bound mismatched commit time", func(s *Search) {
			*s = clientBoundSearch()
			now := s.ResolvedAt.Add(time.Millisecond)
			s.ResolvedAt = &now
		}, false},
		{"bound foreign scope", func(s *Search) { *s = clientBoundSearch(); s.Reservation.ApplicationID = "other" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := clientPendingSearch()
			tc.change(&s)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { respond(w, SearchStatus{Search: s}) }))
			defer server.Close()
			_, err := mustClient(t, server.URL).SearchStatus(context.Background(), clientPendingSearch().ID, "player-one")
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				statusError(t, err, 502)
			}
		})
	}
}

func TestClientSearchStrictResponseFields(t *testing.T) {
	searchJSON, _ := json.Marshal(clientPendingSearch())
	good := string(searchJSON)
	for _, tc := range []struct{ name, raw string }{
		{"missing search", `{"data":{"replay":false}}`},
		{"null search", `{"data":{"search":null,"replay":false}}`},
		{"missing replay", `{"data":{"search":` + good + `}}`},
		{"null replay", `{"data":{"search":` + good + `,"replay":null}}`},
		{"unknown field", `{"data":{"search":` + good + `,"replay":false,"credential":"private"}}`},
		{"null resolved", `{"data":{"search":` + strings.TrimSuffix(good, "}") + `,"resolvedAt":null},"replay":false}}`},
		{"null allocation", `{"data":{"search":` + strings.TrimSuffix(good, "}") + `,"allocationId":null},"replay":false}}`},
		{"null reservation", `{"data":{"search":` + strings.TrimSuffix(good, "}") + `,"reservation":null},"replay":false}}`},
		{"nested duplicate", `{"data":{"search":` + strings.TrimSuffix(good, "}") + `,"searchId":"search-player-other"},"replay":false}}`},
		{"nested case alias", `{"data":{"search":` + strings.TrimSuffix(good, "}") + `,"SearchID":"search-player-other"},"replay":false}}`},
		{"nested escaped alias", `{"data":{"search":` + strings.TrimSuffix(good, "}") + `,"\u0073earchId":"search-player-other"},"replay":false}}`},
		{"nested unicode fold alias", `{"data":{"search":` + strings.TrimSuffix(good, "}") + `,"\u017fearchId":"search-player-other"},"replay":false}}`},
		{"duplicate envelope", `{"data":{"search":` + good + `,"replay":false},"Data":{"search":` + good + `,"replay":true}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.raw)
			}))
			defer server.Close()
			_, err := mustClient(t, server.URL).BeginSearch(context.Background(), "player-one", "request-one")
			statusError(t, err, 502)
		})
	}
	// The field restriction does not prohibit valid UTF-8 profile values.
	cfg := clientConfig("http://127.0.0.1:1234")
	cfg.Compatibility = "版本一"
	search := clientPendingSearch()
	search.Compatibility = cfg.Compatibility
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { respond(w, SearchResult{Search: search}) }))
	defer server.Close()
	cfg.URL = server.URL
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.BeginSearch(context.Background(), "玩家一", "request-one"); err != nil {
		t.Fatal(err)
	}
}

func TestClientSearchInvalidInputsNeverReachTransport(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	c := mustClient(t, server.URL)
	ctx := context.Background()
	for _, user := range []string{"", " leading", "user\n", string([]byte{0xff})} {
		_, err := c.BeginSearch(ctx, user, "request-one")
		statusError(t, err, 422)
		_, err = c.SearchStatus(ctx, "search-player-one", user)
		statusError(t, err, 422)
		_, err = c.CancelSearch(ctx, "search-player-one", user)
		statusError(t, err, 422)
	}
	for _, id := range []string{"", "short", "../request-one", "request-one?x=1"} {
		_, err := c.BeginSearch(ctx, "player-one", id)
		statusError(t, err, 422)
		_, err = c.SearchStatus(ctx, id, "player-one")
		statusError(t, err, 422)
		_, err = c.CancelSearch(ctx, id, "player-one")
		statusError(t, err, 422)
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid search input sent %d requests", requests.Load())
	}
}

func TestClientSearchCancellationRequiresAuthoritativeTerminalState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { respond(w, SearchResult{Search: clientPendingSearch()}) }))
	defer server.Close()
	_, err := mustClient(t, server.URL).CancelSearch(context.Background(), "search-player-one", "player-one")
	statusError(t, err, 502)
}
