package gamefleet

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/heroiclabs/nakama-common/runtime"
)

func TestSearchBeginRPCRecoversOriginalAttemptAfterLostReply(t *testing.T) {
	backend := &bridgeBackend{}
	i, cfg := bridgeHooks(t, backend)
	s := Search{ID: "search_player_a", State: "pending", Region: cfg.Region, Compatibility: cfg.Compatibility}
	backend.beginSearchFn = func(_ context.Context, user, key string) (SearchResult, error) {
		if len(backend.beginSearchArgs) == 1 {
			return SearchResult{}, &Error{Status: 503}
		}
		return SearchResult{Search: s, Replay: true}, nil
	}
	payload := bridgeSearchBeginPayload("attempt_123")
	_, err := i.rpcs[SearchBeginRPC](bridgeCtx("player-a"), nil, nil, nil, payload)
	assertBridgeError(t, err, 14, "gamefleet temporarily unavailable; retry the same request")
	if len(backend.beginSearchArgs) != 1 {
		t.Fatal("lost response was silently retried")
	}
	raw, err := i.rpcs[SearchBeginRPC](bridgeCtx("player-a"), nil, nil, nil, payload)
	var result SearchResult
	if err != nil || json.Unmarshal([]byte(raw), &result) != nil || !result.Replay || result.Search.ID != s.ID {
		t.Fatalf("exact begin replay: %s %v", raw, err)
	}
	first, second := backend.beginSearchArgs[0], backend.beginSearchArgs[1]
	if first != second || first.user != "player-a" || first.key != stableKey("search_begin_", []string{"player-a", "attempt_123"}) {
		t.Fatalf("retry changed identity: %+v %+v", first, second)
	}
	if _, err = i.rpcs[SearchBeginRPC](bridgeCtx("player-b"), nil, nil, nil, payload); err != nil {
		t.Fatal(err)
	}
	third := backend.beginSearchArgs[2]
	if third.user != "player-b" || third.key == first.key || !reflect.DeepEqual(backend.calls, []string{"begin_search", "begin_search", "begin_search"}) {
		t.Fatal("begin borrowed player identity or relied on Current to determine queue state")
	}
}

func TestSearchStatusAndCancelRPCReadOwnedSearchWithoutReleasingRoom(t *testing.T) {
	for _, state := range []string{"pending", "cancelled", "expired", "bound"} {
		t.Run(state, func(t *testing.T) {
			backend := &bridgeBackend{}
			i, cfg := bridgeHooks(t, backend)
			s := Search{ID: "search_player_a", State: state, Region: cfg.Region, Compatibility: cfg.Compatibility}
			if state == "bound" {
				r := Reservation{AllocationID: "alloc_original", State: "prepared"}
				s.AllocationID, s.Reservation = r.AllocationID, &r
			}
			backend.searchStatusFn = func(_ context.Context, id, user string) (SearchStatus, error) {
				if id != s.ID || user != "player-a" {
					t.Fatal("status used untrusted identity")
				}
				return SearchStatus{Search: s}, nil
			}
			backend.cancelSearchFn = func(_ context.Context, id, user string) (SearchResult, error) {
				if id != s.ID || user != "player-a" {
					t.Fatal("cancel used untrusted identity")
				}
				resolved := s
				if state == "pending" {
					resolved.State = "cancelled"
				}
				return SearchResult{Search: resolved, Replay: state != "pending"}, nil
			}
			payload := bridgeSearchParticipantPayload(s.ID)
			raw, err := i.rpcs[SearchStatusRPC](bridgeCtx("player-a"), nil, nil, nil, payload)
			var status SearchStatus
			if err != nil || json.Unmarshal([]byte(raw), &status) != nil || status.Search.State != state {
				t.Fatalf("status: %s %v", raw, err)
			}
			for range 2 {
				raw, err = i.rpcs[SearchCancelRPC](bridgeCtx("player-a"), nil, nil, nil, payload)
				var cancelled SearchResult
				if err != nil || json.Unmarshal([]byte(raw), &cancelled) != nil || cancelled.Search.State == "pending" {
					t.Fatalf("cancel: %s %v", raw, err)
				}
				if state == "bound" && (cancelled.Search.AllocationID != "alloc_original" || cancelled.Search.Reservation == nil) {
					t.Fatal("bound cancellation hid the already committed room")
				}
			}
			if !reflect.DeepEqual(backend.calls, []string{"search_status", "cancel_search", "cancel_search"}) || len(backend.cancelIDs) != 0 || len(backend.currentUsers) != 0 {
				t.Fatalf("search cancellation called room allocation APIs: %v", backend.calls)
			}
		})
	}
}

func TestSearchRPCRejectsForgedIdentitiesAndAmbiguousFields(t *testing.T) {
	backend := &bridgeBackend{}
	i, cfg := bridgeHooks(t, backend)
	for _, route := range []string{SearchBeginRPC, SearchStatusRPC, SearchCancelRPC} {
		payload := bridgeSearchParticipantPayload("search_player_a")
		if route == SearchBeginRPC {
			payload = bridgeSearchBeginPayload("attempt_123")
		}
		for _, malformed := range []string{
			strings.TrimSuffix(payload, "}") + `,"participantId":"victim"}`,
			strings.TrimSuffix(payload, "}") + `,"callerId":"other"}`,
			strings.TrimSuffix(payload, "}") + `,"Version":"` + SearchVersion + `"}`,
			strings.TrimSuffix(payload, "}") + `,"\u0076ersion":"` + SearchVersion + `"}`,
			strings.TrimSuffix(payload, "}") + `,"ver\u017fion":"` + SearchVersion + `"}`,
		} {
			_, err := i.rpcs[route](bridgeCtx("player-a"), nil, nil, nil, malformed)
			assertBridgeError(t, err, 3, "invalid payload")
		}
		for _, change := range [][2]string{{cfg.Region, "other-region"}, {cfg.Compatibility, "other-build"}} {
			_, err := i.rpcs[route](bridgeCtx("player-a"), nil, nil, nil, strings.Replace(payload, change[0], change[1], 1))
			var got *runtime.Error
			if !errors.As(err, &got) || got.Code != 9 {
				t.Fatalf("profile mismatch: %v", err)
			}
		}
	}
	assertNoBackendCalls(t, backend)
}

func TestSearchMatchedLostReplyRetainsCanonicalParticipantSearchTicket(t *testing.T) {
	backend := &bridgeBackend{}
	i, cfg := bridgeHooks(t, backend)
	backend.matchSearchFn = func(_ context.Context, key string, members []SearchMatchMember) (ReservationResult, error) {
		if len(backend.matchKeys) == 1 {
			return ReservationResult{}, &Error{Status: 503}
		}
		return ReservationResult{Reservation: Reservation{AllocationID: "alloc_original"}, Replay: true}, nil
	}
	a, b := bridgeMatchEntry("player-a", "ticket-a", cfg), bridgeMatchEntry("player-b", "ticket-b", cfg)
	_, err := i.matched(context.Background(), nil, nil, nil, []runtime.MatchmakerEntry{b, a})
	assertBridgeError(t, err, 14, "gamefleet temporarily unavailable; retry the same request")
	_, err = i.matched(context.Background(), nil, nil, nil, []runtime.MatchmakerEntry{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(backend.matchKeys) != 2 || backend.matchKeys[0] != backend.matchKeys[1] || !reflect.DeepEqual(backend.matchMembers[0], backend.matchMembers[1]) {
		t.Fatal("repeat delivery changed match identity or lost exact search/ticket binding")
	}
	if !reflect.DeepEqual(backend.calls, []string{"match_searches", "match_searches"}) {
		t.Fatalf("matched hook made extra state queries or used old room reservation: %v", backend.calls)
	}
}

func TestSearchConflictPointsToExactSearchInsteadOfCurrent(t *testing.T) {
	backend := &bridgeBackend{
		beginSearchFn: func(context.Context, string, string) (SearchResult, error) {
			return SearchResult{}, &Error{Status: 409}
		},
		searchStatusFn: func(context.Context, string, string) (SearchStatus, error) {
			return SearchStatus{}, &Error{Status: 409}
		},
		cancelSearchFn: func(context.Context, string, string) (SearchResult, error) {
			return SearchResult{}, &Error{Status: 409}
		},
		matchSearchFn: func(context.Context, string, []SearchMatchMember) (ReservationResult, error) {
			return ReservationResult{}, &Error{Status: 409}
		},
	}
	i, cfg := bridgeHooks(t, backend)
	for route, payload := range map[string]string{
		SearchBeginRPC:  bridgeSearchBeginPayload("attempt_123"),
		SearchStatusRPC: bridgeSearchParticipantPayload("search_player_a"),
		SearchCancelRPC: bridgeSearchParticipantPayload("search_player_a"),
	} {
		_, err := i.rpcs[route](bridgeCtx("player-a"), nil, nil, nil, payload)
		assertBridgeError(t, err, 9, "gamefleet search conflict; query the same search")
	}
	_, err := i.before(bridgeCtx("player-a"), nil, nil, nil, matchmakerEnvelope(RoomVersion, cfg.Compatibility, cfg.Region, nil))
	assertBridgeError(t, err, 9, "gamefleet search conflict; query the same search")
	_, err = i.matched(context.Background(), nil, nil, nil, []runtime.MatchmakerEntry{bridgeMatchEntry("player-a", "ticket-a", cfg), bridgeMatchEntry("player-b", "ticket-b", cfg)})
	assertBridgeError(t, err, 9, "gamefleet search conflict; query the same search")
	assertBridgeError(t, playerError(&Error{Status: 409}), 9, "gamefleet state conflict; refresh current reservation")
	if len(backend.currentUsers) != 0 {
		t.Fatal("search conflict queried Current")
	}
}
