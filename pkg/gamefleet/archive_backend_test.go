package gamefleet

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"testing"
)

type archiveBackendReader struct {
	rooms, searches []bridgeStatusCall
	room            ReservationStatus
	search          SearchStatus
	err             error
}

func (a *archiveBackendReader) Status(_ context.Context, id, user string) (ReservationStatus, error) {
	a.rooms = append(a.rooms, bridgeStatusCall{id, user})
	return a.room, a.err
}
func (a *archiveBackendReader) SearchStatus(_ context.Context, id, user string) (SearchStatus, error) {
	a.searches = append(a.searches, bridgeStatusCall{id, user})
	return a.search, a.err
}

func TestTerminalArchiveBackendUsesExactReadOnlyFallback(t *testing.T) {
	for _, status := range []int{401, 403, 404} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			primary := &bridgeBackend{statusFn: func(context.Context, string, string) (ReservationStatus, error) {
				return ReservationStatus{}, &Error{Status: status}
			}, searchStatusFn: func(context.Context, string, string) (SearchStatus, error) {
				return SearchStatus{}, &Error{Status: status}
			}}
			archive := &archiveBackendReader{room: ReservationStatus{Reservation: Reservation{AllocationID: "original-allocation", State: "completed"}},
				search: SearchStatus{Search: Search{ID: "original-search", State: "cancelled"}}}
			backend := &terminalArchiveBackend{Backend: primary, archive: archive}
			room, err := backend.Status(context.Background(), "original-allocation", "authenticated-user")
			if err != nil || room.Reservation.AllocationID != "original-allocation" || !reflect.DeepEqual(archive.rooms, []bridgeStatusCall{{"original-allocation", "authenticated-user"}}) {
				t.Fatal("room fallback changed original object or identity", room, err, archive.rooms)
			}
			search, err := backend.SearchStatus(context.Background(), "original-search", "authenticated-user")
			if err != nil || search.Search.ID != "original-search" || !reflect.DeepEqual(archive.searches, []bridgeStatusCall{{"original-search", "authenticated-user"}}) {
				t.Fatal("search fallback changed original object or identity", search, err, archive.searches)
			}
			if !reflect.DeepEqual(primary.calls, []string{"status", "search_status"}) {
				t.Fatal("read recovery used another primary operation", primary.calls)
			}
		})
	}
}

func TestTerminalArchiveBackendDoesNotMaskActiveStatusOrUncertainFailure(t *testing.T) {
	for _, status := range []int{0, 409, 422, 429, 502, 503} {
		primary := &bridgeBackend{statusFn: func(context.Context, string, string) (ReservationStatus, error) {
			if status == 0 {
				return ReservationStatus{Reservation: Reservation{AllocationID: "original", State: "prepared"}}, nil
			}
			return ReservationStatus{}, &Error{Status: status}
		}, searchStatusFn: func(context.Context, string, string) (SearchStatus, error) {
			if status == 0 {
				return SearchStatus{Search: Search{ID: "search-original", State: "pending"}}, nil
			}
			return SearchStatus{}, &Error{Status: status}
		}}
		archive := &archiveBackendReader{}
		backend := &terminalArchiveBackend{Backend: primary, archive: archive}
		room, err := backend.Status(context.Background(), "original", "player-a")
		if status == 0 && (err != nil || room.Reservation.State != "prepared") {
			t.Fatal("primary active room substituted", room, err)
		}
		search, searchErr := backend.SearchStatus(context.Background(), "search-original", "player-a")
		if status == 0 && (searchErr != nil || search.Search.State != "pending") {
			t.Fatal("primary pending search substituted", search, searchErr)
		}
		if status != 0 {
			for _, got := range []error{err, searchErr} {
				var upstream *Error
				if !errors.As(got, &upstream) || upstream.Status != status {
					t.Fatal("uncertain primary failure hidden", status, got)
				}
			}
		}
		if len(archive.rooms)+len(archive.searches) != 0 {
			t.Fatal("archive called after active status or uncertain failure", status)
		}
	}
}

func TestTerminalArchiveBackendNeverUsesHistoryForOtherOperations(t *testing.T) {
	primary, archive := &bridgeBackend{}, &archiveBackendReader{}
	backend := &terminalArchiveBackend{Backend: primary, archive: archive}
	ctx := context.Background()
	_, _ = backend.Current(ctx, "player-a")
	_, _ = backend.BeginSearch(ctx, "player-a", "original-request")
	_, _ = backend.CancelSearch(ctx, "original-search", "player-a")
	_, _ = backend.MatchSearches(ctx, "original-match", nil)
	_, _ = backend.Issue(ctx, "original", "player-a", "original-request", 0, false)
	_, _ = backend.Issue(ctx, "original", "player-a", "original-resume", 1, true)
	_, _ = backend.Cancel(ctx, "original")
	if len(archive.rooms)+len(archive.searches) != 0 || !reflect.DeepEqual(primary.calls,
		[]string{"current", "begin_search", "cancel_search", "match_searches", "issue", "issue", "cancel"}) {
		t.Fatal("another operation borrowed archive authority", primary.calls)
	}
}

func TestTerminalArchiveRPCUsesAuthenticatedIdentityAndRetainsReadFailure(t *testing.T) {
	primary := &bridgeBackend{statusFn: func(context.Context, string, string) (ReservationStatus, error) {
		return ReservationStatus{}, &Error{Status: 404}
	}, searchStatusFn: func(context.Context, string, string) (SearchStatus, error) {
		return SearchStatus{}, &Error{Status: 404}
	}}
	archive := &archiveBackendReader{room: ReservationStatus{Reservation: Reservation{AllocationID: "original", State: "completed"}},
		search: SearchStatus{Search: Search{ID: "search-original", State: "cancelled"}}}
	i, cfg := bridgeHooks(t, &terminalArchiveBackend{Backend: primary, archive: archive})
	payload := jsBridge(t, map[string]any{"version": RoomVersion, "compatibility": cfg.Compatibility, "region": cfg.Region, "allocationId": "original"})
	raw, err := i.rpcs[StatusRPC](bridgeCtx("player-a"), nil, nil, nil, payload)
	if err != nil || len(archive.rooms) != 1 || archive.rooms[0].user != "player-a" {
		t.Fatal("RPC archive identity did not come from authenticated context", raw, err, archive.rooms)
	}
	_, err = i.rpcs[StatusRPC](bridgeCtx("player-a"), nil, nil, nil,
		jsBridge(t, map[string]any{"version": RoomVersion, "compatibility": cfg.Compatibility, "region": cfg.Region, "allocationId": "original", "participantId": "victim"}))
	assertBridgeError(t, err, 3, "invalid payload")
	if len(archive.rooms) != 1 {
		t.Fatal("payload identity reached the archive reader")
	}
	archive.err = &Error{Status: 503}
	_, err = i.rpcs[SearchStatusRPC](bridgeCtx("player-a"), nil, nil, nil, bridgeSearchParticipantPayload("search-original"))
	assertBridgeError(t, err, 14, "gamefleet temporarily unavailable; retry the same request")
	if !reflect.DeepEqual(primary.calls, []string{"status", "search_status"}) || len(archive.searches) != 1 {
		t.Fatal("unconfirmed history failure queried Current or started new work", primary.calls)
	}
}

func jsBridge(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
