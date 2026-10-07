package gamefleet

import (
	"context"
	"reflect"
	"testing"

	"github.com/heroiclabs/nakama-common/runtime"
)

func archiveProfileBackend(t *testing.T, primary *bridgeBackend, archive *archiveBackendReader) (*bridgeInitializer, Config) {
	t.Helper()
	wrapped := &terminalArchiveBackend{
		Backend: primary,
		archive: archive,
		archiveScope: TerminalArchiveConfig{TLS: fixtureTLS,
			Region:        "archive-west",
			Compatibility: "build-before-rollout",
		},
	}
	return bridgeHooks(t, wrapped)
}

func archiveRoomPayload(t *testing.T, version, compatibility, region, allocationID string) string {
	t.Helper()
	return jsBridge(t, map[string]any{"version": version, "compatibility": compatibility, "region": region, "allocationId": allocationID})
}

func archiveSearchPayload(t *testing.T, version, compatibility, region, searchID string) string {
	t.Helper()
	return jsBridge(t, map[string]any{"version": version, "compatibility": compatibility, "region": region, "searchId": searchID})
}

func TestTerminalArchiveReadsExactOldProfileWithoutQueryingCurrentBackend(t *testing.T) {
	primary := &bridgeBackend{
		statusFn: func(context.Context, string, string) (ReservationStatus, error) {
			t.Fatal("old-profile reservation status queried the active backend")
			return ReservationStatus{}, nil
		},
		searchStatusFn: func(context.Context, string, string) (SearchStatus, error) {
			t.Fatal("old-profile search status queried the active backend")
			return SearchStatus{}, nil
		},
	}
	archive := &archiveBackendReader{
		room:   ReservationStatus{Reservation: Reservation{AllocationID: "allocation-old-001", State: "completed"}},
		search: SearchStatus{Search: Search{ID: "search-old-0001", State: "expired"}},
	}
	i, _ := archiveProfileBackend(t, primary, archive)
	ctx := bridgeCtx("authenticated-nakama-user")

	roomJSON, err := i.rpcs[StatusRPC](ctx, nil, nil, nil,
		archiveRoomPayload(t, RoomVersion, "build-before-rollout", "archive-west", "allocation-old-001"))
	if err != nil || roomJSON == "" {
		t.Fatalf("old-profile terminal room status failed: %q %v", roomJSON, err)
	}
	searchJSON, err := i.rpcs[SearchStatusRPC](ctx, nil, nil, nil,
		archiveSearchPayload(t, SearchVersion, "build-before-rollout", "archive-west", "search-old-0001"))
	if err != nil || searchJSON == "" {
		t.Fatalf("old-profile terminal search status failed: %q %v", searchJSON, err)
	}
	if !reflect.DeepEqual(archive.rooms, []bridgeStatusCall{{"allocation-old-001", "authenticated-nakama-user"}}) ||
		!reflect.DeepEqual(archive.searches, []bridgeStatusCall{{"search-old-0001", "authenticated-nakama-user"}}) {
		t.Fatalf("archive received changed IDs or non-context identity: rooms=%v searches=%v", archive.rooms, archive.searches)
	}
	if len(primary.calls) != 0 || len(primary.statusArgs) != 0 || len(primary.searchStatusArgs) != 0 {
		t.Fatalf("historical profile contacted current backend: calls=%v", primary.calls)
	}
}

func TestTerminalArchiveProfileIsReadOnlyAndRejectsUnknownScopeOrProtocol(t *testing.T) {
	primary, archive := &bridgeBackend{}, &archiveBackendReader{}
	i, cfg := archiveProfileBackend(t, primary, archive)
	ctx := bridgeCtx("player-a")

	// Other status profiles cannot select the archive reader by approximation.
	for _, tc := range []struct {
		name, payload, message string
	}{
		{"unknown room compatibility", archiveRoomPayload(t, RoomVersion, "another-build", "archive-west", "allocation-old-001"), "fleet_build_mismatch"},
		{"unknown room region", archiveRoomPayload(t, RoomVersion, cfg.Compatibility, "another-region", "allocation-old-001"), "fleet_region_mismatch"},
		{"wrong room protocol", archiveRoomPayload(t, "future-room-protocol", "build-before-rollout", "archive-west", "allocation-old-001"), "gamefleet_protocol_mismatch"},
		{"unknown search compatibility", archiveSearchPayload(t, SearchVersion, "another-build", "archive-west", "search-old-0001"), "fleet_build_mismatch"},
		{"unknown search region", archiveSearchPayload(t, SearchVersion, cfg.Compatibility, "another-region", "search-old-0001"), "fleet_region_mismatch"},
		{"wrong search protocol", archiveSearchPayload(t, "future-search-protocol", "build-before-rollout", "archive-west", "search-old-0001"), "gamefleet_protocol_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := StatusRPC
			if tc.name == "unknown search compatibility" || tc.name == "unknown search region" || tc.name == "wrong search protocol" {
				route = SearchStatusRPC
			}
			_, err := i.rpcs[route](ctx, nil, nil, nil, tc.payload)
			assertBridgeError(t, err, 9, tc.message)
		})
	}

	// Current, cancellation, admission and ticket APIs still require the active
	// profile; no archive status read is used to grant mutation authority.
	oldRoomBase := map[string]any{"version": RoomVersion, "compatibility": "build-before-rollout", "region": "archive-west"}
	roomWith := func(extra map[string]any) string {
		body := make(map[string]any, len(oldRoomBase)+len(extra))
		for k, v := range oldRoomBase {
			body[k] = v
		}
		for k, v := range extra {
			body[k] = v
		}
		return jsBridge(t, body)
	}
	oldSearchBase := map[string]any{"version": SearchVersion, "compatibility": "build-before-rollout", "region": "archive-west"}
	searchWith := func(extra map[string]any) string {
		body := make(map[string]any, len(oldSearchBase)+len(extra))
		for k, v := range oldSearchBase {
			body[k] = v
		}
		for k, v := range extra {
			body[k] = v
		}
		return jsBridge(t, body)
	}
	mutations := []struct{ route, payload string }{
		{CurrentRPC, roomWith(nil)},
		{CancelRPC, roomWith(map[string]any{"allocationId": "allocation-old-001"})},
		{AssignmentRPC, roomWith(map[string]any{"allocationId": "allocation-old-001", "requestId": "request-old-001", "previousConnectionGeneration": int64(0)})},
		{ResumeRPC, roomWith(map[string]any{"allocationId": "allocation-old-001", "requestId": "request-old-001", "previousConnectionGeneration": int64(1)})},
		{SearchBeginRPC, searchWith(map[string]any{"requestId": "request-old-001"})},
		{SearchCancelRPC, searchWith(map[string]any{"searchId": "search-old-0001"})},
	}
	for _, tc := range mutations {
		t.Run(tc.route, func(t *testing.T) {
			_, err := i.rpcs[tc.route](ctx, nil, nil, nil, tc.payload)
			assertBridgeError(t, err, 9, "fleet_build_mismatch")
		})
	}
	if len(primary.calls) != 0 || len(archive.rooms) != 0 || len(archive.searches) != 0 {
		t.Fatalf("non-status operation used a backend: primary=%v archive rooms=%v searches=%v", primary.calls, archive.rooms, archive.searches)
	}
	if cfg.Compatibility == "build-before-rollout" || cfg.Region == "archive-west" {
		t.Fatal("test archive scope must remain distinct from active profile")
	}
}

func TestTerminalArchiveOldProfileReadFailureRemainsFailure(t *testing.T) {
	primary, archive := &bridgeBackend{}, &archiveBackendReader{err: &Error{Status: 503}}
	i, _ := archiveProfileBackend(t, primary, archive)
	ctx := bridgeCtx("player-a")
	for _, tc := range []struct{ route, payload string }{
		{StatusRPC, archiveRoomPayload(t, RoomVersion, "build-before-rollout", "archive-west", "allocation-old-001")},
		{SearchStatusRPC, archiveSearchPayload(t, SearchVersion, "build-before-rollout", "archive-west", "search-old-0001")},
	} {
		raw, err := i.rpcs[tc.route](ctx, nil, nil, nil, tc.payload)
		if raw != "" {
			t.Fatalf("failed archive read returned apparent state for %s: %q", tc.route, raw)
		}
		assertBridgeError(t, err, 14, "gamefleet temporarily unavailable; retry the same request")
	}
	if len(primary.calls) != 0 || !reflect.DeepEqual(archive.rooms, []bridgeStatusCall{{"allocation-old-001", "player-a"}}) ||
		!reflect.DeepEqual(archive.searches, []bridgeStatusCall{{"search-old-0001", "player-a"}}) {
		t.Fatalf("archive failure path changed IDs/identity or contacted primary: primary=%v archive rooms=%v searches=%v", primary.calls, archive.rooms, archive.searches)
	}
}

func TestTerminalArchiveDoesNotRelaxMatchmakingProfiles(t *testing.T) {
	primary, archive := &bridgeBackend{}, &archiveBackendReader{}
	i, _ := archiveProfileBackend(t, primary, archive)
	ctx := bridgeCtx("player-a")
	queued, err := i.before(ctx, nil, nil, nil, matchmakerEnvelope(RoomVersion, "build-before-rollout", "archive-west", nil))
	if queued != nil {
		t.Fatal("old archive profile entered the matchmaker queue")
	}
	assertBridgeError(t, err, 9, "fleet_build_mismatch")

	old := bridgeEntry{
		presence: bridgePresence{user: "player-a"}, ticket: "ticket-a",
		properties: map[string]any{"gamefleet_protocol": RoomVersion, "build_hash": "build-before-rollout", "region": "archive-west", "gamefleet_search_id": "search_player_a"},
	}
	_, err = i.matched(context.Background(), nil, nil, nil, []runtime.MatchmakerEntry{old, bridgeMatchEntry("player-b", "ticket-b", bridgeTestConfig())})
	assertBridgeError(t, err, 9, "fleet_build_mismatch")
	if len(primary.calls) != 0 || len(archive.rooms) != 0 || len(archive.searches) != 0 {
		t.Fatalf("archive configuration affected matchmaking: primary=%v archive rooms=%v searches=%v", primary.calls, archive.rooms, archive.searches)
	}
}
