package gamefleet

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func retryTestMembers() []SearchMatchMember {
	return []SearchMatchMember{
		{ParticipantID: "player-one", SearchID: "search-player-one", NakamaTicket: "ticket-one"},
		{ParticipantID: "player-two", SearchID: "search-player-two", NakamaTicket: "ticket-two"},
	}
}

func writeMatchRetrySuccess(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data":      ReservationResult{Reservation: testReservation()},
		"requestId": "match-retry-test",
	})
}

func TestMatchSearchesRetries409And429WithFrozenMembersAndOneCommit(t *testing.T) {
	members := retryTestMembers()
	expectedMembers := append([]SearchMatchMember(nil), members...)
	firstRequest := make(chan struct{})
	releaseFirst := make(chan struct{})
	var calls atomic.Int32
	var persisted atomic.Int32
	var mu sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/business/v1/searches/match" || r.URL.RawQuery != "" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		call := calls.Add(1)
		mu.Lock()
		bodies = append(bodies, append([]byte(nil), raw...))
		mu.Unlock()
		if call == 1 {
			close(firstRequest)
			select {
			case <-releaseFirst:
			case <-r.Context().Done():
				return
			}
			w.WriteHeader(http.StatusConflict)
			return
		}
		if call == 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if call == 3 {
			if persisted.Add(1) != 1 {
				t.Errorf("more than one fake durable reservation was created")
			}
			writeMatchRetrySuccess(w, http.StatusAccepted)
			return
		}
		t.Errorf("unexpected extra match attempt %d", call)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	c := mustClient(t, server.URL)
	type outcome struct {
		result ReservationResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := c.MatchSearches(context.Background(), "match-request-1234", members)
		done <- outcome{result: result, err: err}
	}()
	select {
	case <-firstRequest:
	case <-time.After(time.Second):
		t.Fatal("initial match request did not arrive")
	}
	// The caller may reuse or mutate its slice after the call starts. Retries
	// must stay bound to the original participant/search/ticket snapshot.
	members[0] = SearchMatchMember{ParticipantID: "other-one", SearchID: "other-search-one", NakamaTicket: "other-ticket-one"}
	members[1] = SearchMatchMember{ParticipantID: "other-two", SearchID: "other-search-two", NakamaTicket: "other-ticket-two"}
	close(releaseFirst)
	var got outcome
	select {
	case got = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("match retry did not finish")
	}
	if got.err != nil || got.result.Reservation.AllocationID != testReservation().AllocationID {
		t.Fatalf("match retry result=%+v err=%v", got.result, got.err)
	}
	if calls.Load() != 3 || persisted.Load() != 1 {
		t.Fatalf("requests=%d durable reservations=%d, want 3 and 1", calls.Load(), persisted.Load())
	}

	mu.Lock()
	gotBodies := make([][]byte, len(bodies))
	for i := range bodies {
		gotBodies[i] = append([]byte(nil), bodies[i]...)
	}
	mu.Unlock()
	if len(gotBodies) != 3 {
		t.Fatalf("captured %d request bodies, want 3", len(gotBodies))
	}
	for i := 1; i < len(gotBodies); i++ {
		if !bytes.Equal(gotBodies[0], gotBodies[i]) {
			t.Fatalf("retry %d changed exact match body\nfirst: %s\nnext:  %s", i+1, gotBodies[0], gotBodies[i])
		}
	}
	var wire struct {
		Version       string              `json:"version"`
		Key           string              `json:"idempotencyKey"`
		Compatibility string              `json:"compatibility"`
		Members       []SearchMatchMember `json:"members"`
	}
	if err := json.Unmarshal(gotBodies[0], &wire); err != nil {
		t.Fatalf("decode captured match body: %v", err)
	}
	if wire.Version != SearchVersion || wire.Key != "match-request-1234" || wire.Compatibility != "dm-v1" ||
		!reflect.DeepEqual(wire.Members, expectedMembers) {
		t.Fatalf("request did not retain original match identity: %+v", wire)
	}
}

func TestMatchSearchesPersistentConflictStopsAtCallerDeadline(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusConflict)
	}))
	defer server.Close()
	c := mustClient(t, server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.MatchSearches(ctx, "match-request-1234", retryTestMembers())
	if err == nil {
		t.Fatal("persistent conflict unexpectedly succeeded")
	}
	if time.Since(start) > time.Second {
		t.Fatal("short caller deadline was not respected")
	}
	if calls.Load() < 1 || calls.Load() > 2 {
		t.Fatalf("requests before caller deadline = %d, want 1 or 2", calls.Load())
	}
	// Let any incorrectly scheduled retry become observable after cancellation.
	time.Sleep(350 * time.Millisecond)
	if calls.Load() > 2 {
		t.Fatalf("additional HTTP requests continued after caller deadline: %d", calls.Load())
	}
}

func TestMatchSearchesDoesNotRetryNonTransientStatuses(t *testing.T) {
	for _, tc := range []struct {
		responseStatus int
		errorStatus    int
	}{
		{http.StatusUnauthorized, http.StatusUnauthorized},
		{http.StatusForbidden, http.StatusForbidden},
		{http.StatusNotFound, http.StatusNotFound},
		{http.StatusUnprocessableEntity, http.StatusUnprocessableEntity},
		// Unknown upstream gateway statuses are sanitized to 503 by client.call.
		{http.StatusBadGateway, http.StatusServiceUnavailable},
		{http.StatusServiceUnavailable, http.StatusServiceUnavailable},
	} {
		t.Run(http.StatusText(tc.responseStatus), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.responseStatus)
				// Even a gateway error with a malformed body is a non-retryable
				// transport result; response bytes must not control retry policy.
				_, _ = io.WriteString(w, `{"not-valid-json"`)
			}))
			defer server.Close()
			c := mustClient(t, server.URL)
			_, err := c.MatchSearches(context.Background(), "match-request-1234", retryTestMembers())
			statusError(t, err, tc.errorStatus)
			if calls.Load() != 1 {
				t.Fatalf("status %d made %d requests, want exactly one", tc.responseStatus, calls.Load())
			}
		})
	}
}
