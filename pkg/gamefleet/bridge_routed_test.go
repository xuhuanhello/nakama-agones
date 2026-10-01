package gamefleet

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/heroiclabs/nakama-common/runtime"
	"google.golang.org/protobuf/proto"
)

func routedBridgePool(ch string) string { return "gfsp_" + strings.Repeat(ch, 64) }

func routedBridgeStatus(cfg Config, id, pool, state, allocation string) RoutedSearchStatus {
	return RoutedSearchStatus{Version: ServiceSearchRoutedVersion, MatchPoolID: pool, Search: Search{
		ID: id, State: state, Region: cfg.Region, Compatibility: cfg.Compatibility, AllocationID: allocation,
	}}
}

func routedBridgeEntry(user, ticket string, cfg Config, pool string) bridgeEntry {
	entry := bridgeMatchEntry(user, ticket, cfg).(bridgeEntry)
	entry.properties[routedPoolProperty] = pool
	entry.properties[routedQueueProperty] = routedQueueProtocol
	return entry
}

func TestRoutedMatchmakerAddReplacesUntrustedQueryAndNumericShadows(t *testing.T) {
	cfg := bridgeTestConfig()
	pool := routedBridgePool("a")
	for _, query := range []string{"*", "properties.region:any OR *", "-properties.gamefleet_match_pool:" + pool, "invalid (((", ""} {
		t.Run(query, func(t *testing.T) {
			backend := &bridgeBackend{}
			var calls []bridgeSearchCall
			b := bridge{backend: backend, config: cfg, matchingRoutedSearchStatus: func(_ context.Context, id, user string) (RoutedSearchStatus, error) {
				calls = append(calls, bridgeSearchCall{searchID: id, user: user})
				return routedBridgeStatus(cfg, id, pool, "pending", ""), nil
			}}
			request := matchmakerEnvelope(RoomVersion, cfg.Compatibility, cfg.Region, nil)
			r := request.GetMatchmakerAdd()
			r.Query = query
			r.StringProperties[routedPoolProperty] = routedBridgePool("b")
			r.StringProperties[routedQueueProperty] = "spoofed-v2"
			r.StringProperties["participantId"] = "not-the-authenticated-user"
			for _, name := range []string{"gamefleet_protocol", "build_hash", "region", "gamefleet_search_id", routedPoolProperty, routedQueueProperty} {
				r.NumericProperties[name] = 123
			}
			original := proto.Clone(request)
			response, err := b.before(bridgeCtx("player-a"), nil, nil, nil, request)
			if err != nil || response == nil {
				t.Fatal("owned pending routed search rejected", err)
			}
			wantQuery := "+properties.gamefleet_match_pool:" + pool + " +properties.gamefleet_queue_protocol:gamefleet-service-search-v2"
			got := response.GetMatchmakerAdd()
			if got.Query != wantQuery || got.StringProperties[routedPoolProperty] != pool || got.StringProperties[routedQueueProperty] != routedQueueProtocol {
				t.Fatal("client query or spoofed property influenced the authoritative queue")
			}
			if !reflect.DeepEqual(got.NumericProperties, map[string]float64{"skill": 1234}) || got.StringProperties["custom"] != "kept" ||
				!proto.Equal(request, original) || response == request {
				t.Fatal("numeric shadows, unrelated properties, or original envelope were mishandled")
			}
			if !reflect.DeepEqual(calls, []bridgeSearchCall{{searchID: "search_player-a", user: "player-a"}}) {
				t.Fatal("status did not use exact search and authenticated participant")
			}
			assertNoBackendCalls(t, backend)
		})
	}
}

func TestRoutedMatchmakerAddDeniesInvalidStatusWithoutFallbackOrMutation(t *testing.T) {
	cfg := bridgeTestConfig()
	pool := routedBridgePool("a")
	for name, change := range map[string]func(*RoutedSearchStatus){
		"wrong-version":       func(s *RoutedSearchStatus) { s.Version = ServiceSearchVersion },
		"missing-pool":        func(s *RoutedSearchStatus) { s.MatchPoolID = "" },
		"query-injection":     func(s *RoutedSearchStatus) { s.MatchPoolID = pool + " OR *" },
		"upper-pool":          func(s *RoutedSearchStatus) { s.MatchPoolID = strings.ToUpper(pool) },
		"wrong-search":        func(s *RoutedSearchStatus) { s.Search.ID = "search_other" },
		"wrong-region":        func(s *RoutedSearchStatus) { s.Search.Region = "other-west" },
		"wrong-compatibility": func(s *RoutedSearchStatus) { s.Search.Compatibility = "other-build" },
		"bound":               func(s *RoutedSearchStatus) { s.Search.State = "bound" },
		"cancelled":           func(s *RoutedSearchStatus) { s.Search.State = "cancelled" },
		"expired":             func(s *RoutedSearchStatus) { s.Search.State = "expired" },
	} {
		t.Run(name, func(t *testing.T) {
			backend := &bridgeBackend{}
			b := bridge{backend: backend, config: cfg, matchingRoutedSearchStatus: func(_ context.Context, id, _ string) (RoutedSearchStatus, error) {
				out := routedBridgeStatus(cfg, id, pool, "pending", "")
				change(&out)
				return out, nil
			}}
			request := matchmakerEnvelope(RoomVersion, cfg.Compatibility, cfg.Region, nil)
			original := proto.Clone(request)
			out, err := b.before(bridgeCtx("player-a"), nil, nil, nil, request)
			if err == nil || out != nil || !proto.Equal(request, original) {
				t.Fatal("invalid routed status entered the queue or mutated the input")
			}
			assertNoBackendCalls(t, backend)
		})
	}
	backend := &bridgeBackend{}
	b := bridge{backend: backend, config: cfg, matchingRoutedSearchStatus: func(context.Context, string, string) (RoutedSearchStatus, error) {
		return RoutedSearchStatus{}, &Error{Status: 403}
	}}
	_, err := b.before(bridgeCtx("player-a"), nil, nil, nil, matchmakerEnvelope(RoomVersion, cfg.Compatibility, cfg.Region, nil))
	assertBridgeError(t, err, 7, "gamefleet access denied")
	assertNoBackendCalls(t, backend)
}

func TestRoutedMatchedRechecksOriginalPairAndKeepsCanonicalReplayKey(t *testing.T) {
	cfg := bridgeTestConfig()
	pool := routedBridgePool("a")
	backend := &bridgeBackend{}
	var calls []bridgeSearchCall
	state, allocation := "pending", ""
	b := bridge{backend: backend, config: cfg, matchingRoutedSearchStatus: func(_ context.Context, id, user string) (RoutedSearchStatus, error) {
		calls = append(calls, bridgeSearchCall{searchID: id, user: user})
		return routedBridgeStatus(cfg, id, pool, state, allocation), nil
	}}
	first, second := routedBridgeEntry("player-b", "ticket-b", cfg, pool), routedBridgeEntry("player-a", "ticket-a", cfg, pool)
	first.properties["participantId"] = "spoofed-player"
	entries := []runtime.MatchmakerEntry{first, second}
	if _, err := b.matched(context.Background(), nil, nil, nil, entries); err != nil {
		t.Fatal("valid routed pair rejected", err)
	}
	want := []SearchMatchMember{{ParticipantID: "player-a", SearchID: "search_player_a", NakamaTicket: "ticket-a"}, {ParticipantID: "player-b", SearchID: "search_player_b", NakamaTicket: "ticket-b"}}
	key := stableKey("matchmaker_search_", []any{SearchVersion, cfg.Region, cfg.Compatibility, want})
	if len(backend.matchKeys) != 1 || backend.matchKeys[0] != key || !reflect.DeepEqual(backend.matchMembers[0], want) ||
		!reflect.DeepEqual(calls, []bridgeSearchCall{{searchID: "search_player_b", user: "player-b"}, {searchID: "search_player_a", user: "player-a"}}) {
		t.Fatal("matched pair did not recheck presence identities or retain the established immutable request")
	}
	// Simulate a post-commit retry: the route may now point elsewhere, but the
	// two exact mappings are still bound to their original source/allocation.
	state, allocation = "bound", "allocation_original_001"
	if _, err := b.matched(context.Background(), nil, nil, nil, []runtime.MatchmakerEntry{second, first}); err != nil {
		t.Fatal("original bound pair replay rejected", err)
	}
	if len(backend.matchKeys) != 2 || backend.matchKeys[1] != key || !reflect.DeepEqual(backend.matchMembers[1], want) {
		t.Fatal("routed bound replay or entry reversal changed match identity")
	}
}

func TestRoutedMatchedRejectsOldSpoofedOrMixedPoolEntriesBeforeLookup(t *testing.T) {
	cfg := bridgeTestConfig()
	pool := routedBridgePool("a")
	for name, change := range map[string]func(*bridgeEntry){
		"missing-marker": func(e *bridgeEntry) { delete(e.properties, routedQueueProperty) },
		"wrong-marker":   func(e *bridgeEntry) { e.properties[routedQueueProperty] = "v1" },
		"numeric-marker": func(e *bridgeEntry) { e.properties[routedQueueProperty] = 2.0 },
		"missing-pool":   func(e *bridgeEntry) { delete(e.properties, routedPoolProperty) },
		"numeric-pool":   func(e *bridgeEntry) { e.properties[routedPoolProperty] = 1.0 },
		"invalid-pool":   func(e *bridgeEntry) { e.properties[routedPoolProperty] = pool + " OR *" },
		"other-pool":     func(e *bridgeEntry) { e.properties[routedPoolProperty] = routedBridgePool("b") },
	} {
		t.Run(name, func(t *testing.T) {
			backend := &bridgeBackend{}
			lookups := 0
			b := bridge{backend: backend, config: cfg, matchingRoutedSearchStatus: func(context.Context, string, string) (RoutedSearchStatus, error) {
				lookups++
				return RoutedSearchStatus{}, nil
			}}
			first, second := routedBridgeEntry("player-a", "ticket-a", cfg, pool), routedBridgeEntry("player-b", "ticket-b", cfg, pool)
			change(&second)
			_, err := b.matched(context.Background(), nil, nil, nil, []runtime.MatchmakerEntry{first, second})
			assertBridgeError(t, err, 9, "search pool unavailable")
			if lookups != 0 {
				t.Fatal("invalid queue entry reached mapped status lookup")
			}
			assertNoBackendCalls(t, backend)
		})
	}
}

func TestRoutedMatchedRejectsUnmatchableOriginalStatus(t *testing.T) {
	cfg := bridgeTestConfig()
	pool := routedBridgePool("a")
	for name, change := range map[string]func(*RoutedSearchStatus, *RoutedSearchStatus){
		"mixed-pool":      func(_, b *RoutedSearchStatus) { b.MatchPoolID = routedBridgePool("b") },
		"changed-search":  func(_, b *RoutedSearchStatus) { b.Search.ID = "search_other" },
		"changed-profile": func(_, b *RoutedSearchStatus) { b.Search.Compatibility = "other-build" },
		"old-version":     func(_, b *RoutedSearchStatus) { b.Version = ServiceSearchVersion },
		"pending-bound":   func(_, b *RoutedSearchStatus) { b.Search.State, b.Search.AllocationID = "bound", "allocation_bound" },
		"cancelled":       func(_, b *RoutedSearchStatus) { b.Search.State = "cancelled" },
		"expired":         func(a, _ *RoutedSearchStatus) { a.Search.State = "expired" },
		"both-terminal":   func(a, b *RoutedSearchStatus) { a.Search.State, b.Search.State = "expired", "cancelled" },
		"different-allocation": func(a, b *RoutedSearchStatus) {
			a.Search.State, b.Search.State = "bound", "bound"
			a.Search.AllocationID, b.Search.AllocationID = "allocation_a", "allocation_b"
		},
		"empty-bound-allocation": func(a, b *RoutedSearchStatus) { a.Search.State, b.Search.State = "bound", "bound" },
	} {
		t.Run(name, func(t *testing.T) {
			backend := &bridgeBackend{}
			first := routedBridgeStatus(cfg, "search_player_a", pool, "pending", "")
			second := routedBridgeStatus(cfg, "search_player_b", pool, "pending", "")
			change(&first, &second)
			b := bridge{backend: backend, config: cfg, matchingRoutedSearchStatus: func(_ context.Context, _, user string) (RoutedSearchStatus, error) {
				if user == "player-a" {
					return first, nil
				}
				return second, nil
			}}
			_, err := b.matched(context.Background(), nil, nil, nil, []runtime.MatchmakerEntry{routedBridgeEntry("player-a", "ticket-a", cfg, pool), routedBridgeEntry("player-b", "ticket-b", cfg, pool)})
			if err == nil {
				t.Fatal("unmatchable original status reached Match")
			}
			assertNoBackendCalls(t, backend)
		})
	}
}

func TestRoutedMatchedRetainsIndependentPlatformMatchAuthorization(t *testing.T) {
	cfg := bridgeTestConfig()
	pool := routedBridgePool("a")
	backend := &bridgeBackend{matchSearchFn: func(context.Context, string, []SearchMatchMember) (ReservationResult, error) {
		return ReservationResult{}, &Error{Status: 403}
	}}
	b := bridge{backend: backend, config: cfg, matchingRoutedSearchStatus: func(_ context.Context, id, _ string) (RoutedSearchStatus, error) {
		return routedBridgeStatus(cfg, id, pool, "pending", ""), nil
	}}
	_, err := b.matched(context.Background(), nil, nil, nil, []runtime.MatchmakerEntry{routedBridgeEntry("player-a", "ticket-a", cfg, pool), routedBridgeEntry("player-b", "ticket-b", cfg, pool)})
	assertBridgeError(t, err, 7, "gamefleet access denied")
	if !reflect.DeepEqual(backend.calls, []string{"match_searches"}) {
		t.Fatal("read authority bypassed the independent Match operation")
	}
	backend.calls = nil
	b.matchingRoutedSearchStatus = func(context.Context, string, string) (RoutedSearchStatus, error) {
		return RoutedSearchStatus{}, &Error{Status: 403}
	}
	_, err = b.matched(context.Background(), nil, nil, nil, []runtime.MatchmakerEntry{routedBridgeEntry("player-a", "ticket-a", cfg, pool), routedBridgeEntry("player-b", "ticket-b", cfg, pool)})
	assertBridgeError(t, err, 7, "gamefleet access denied")
	assertNoBackendCalls(t, backend)
}
