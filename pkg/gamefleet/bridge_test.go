package gamefleet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/heroiclabs/nakama-common/rtapi"
	"github.com/heroiclabs/nakama-common/runtime"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type bridgeBeforeHook = func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *rtapi.Envelope) (*rtapi.Envelope, error)
type bridgeRPCHook = func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, string) (string, error)
type bridgeMatchedHook = func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, []runtime.MatchmakerEntry) (string, error)

type bridgeInitializer struct {
	runtime.Initializer
	beforeName string
	before     bridgeBeforeHook
	matched    bridgeMatchedHook
	rpcs       map[string]bridgeRPCHook
	registered []string
}

func (i *bridgeInitializer) RegisterBeforeRt(name string, fn bridgeBeforeHook) error {
	i.registered = append(i.registered, "before:"+name)
	i.beforeName, i.before = name, fn
	return nil
}
func (i *bridgeInitializer) RegisterMatchmakerMatched(fn bridgeMatchedHook) error {
	i.registered = append(i.registered, "matched")
	i.matched = fn
	return nil
}
func (i *bridgeInitializer) RegisterRpc(name string, fn bridgeRPCHook) error {
	i.registered = append(i.registered, "rpc:"+name)
	if i.rpcs == nil {
		i.rpcs = make(map[string]bridgeRPCHook)
	}
	i.rpcs[name] = fn
	return nil
}

type bridgeBackend struct {
	calls []string

	currentFn      func(context.Context, string) (CurrentResult, error)
	statusFn       func(context.Context, string, string) (ReservationStatus, error)
	beginSearchFn  func(context.Context, string, string) (SearchResult, error)
	searchStatusFn func(context.Context, string, string) (SearchStatus, error)
	cancelSearchFn func(context.Context, string, string) (SearchResult, error)
	matchSearchFn  func(context.Context, string, []SearchMatchMember) (ReservationResult, error)
	issueFn        func(context.Context, string, string, string, int64, bool) (AssignmentResult, error)
	cancelFn       func(context.Context, string) (ReservationResult, error)

	currentUsers     []string
	statusArgs       []bridgeStatusCall
	beginSearchArgs  []bridgeSearchCall
	searchStatusArgs []bridgeSearchCall
	cancelSearchArgs []bridgeSearchCall
	matchKeys        []string
	matchMembers     [][]SearchMatchMember
	issueArgs        []bridgeIssueCall
	cancelIDs        []string
}

type bridgeIssueCall struct {
	allocationID string
	user         string
	key          string
	previous     int64
	resume       bool
}

type bridgeStatusCall struct {
	allocationID string
	user         string
}

type bridgeSearchCall struct {
	searchID string
	user     string
	key      string
}

func (f *bridgeBackend) Current(ctx context.Context, user string) (CurrentResult, error) {
	f.calls = append(f.calls, "current")
	f.currentUsers = append(f.currentUsers, user)
	if f.currentFn != nil {
		return f.currentFn(ctx, user)
	}
	return CurrentResult{}, nil
}
func (f *bridgeBackend) Status(ctx context.Context, allocationID, user string) (ReservationStatus, error) {
	f.calls = append(f.calls, "status")
	f.statusArgs = append(f.statusArgs, bridgeStatusCall{allocationID, user})
	if f.statusFn != nil {
		return f.statusFn(ctx, allocationID, user)
	}
	return ReservationStatus{}, nil
}
func (f *bridgeBackend) BeginSearch(ctx context.Context, user, key string) (SearchResult, error) {
	f.calls = append(f.calls, "begin_search")
	f.beginSearchArgs = append(f.beginSearchArgs, bridgeSearchCall{user: user, key: key})
	if f.beginSearchFn != nil {
		return f.beginSearchFn(ctx, user, key)
	}
	return SearchResult{}, nil
}
func (f *bridgeBackend) SearchStatus(ctx context.Context, id, user string) (SearchStatus, error) {
	f.calls = append(f.calls, "search_status")
	f.searchStatusArgs = append(f.searchStatusArgs, bridgeSearchCall{searchID: id, user: user})
	if f.searchStatusFn != nil {
		return f.searchStatusFn(ctx, id, user)
	}
	cfg := bridgeTestConfig()
	return SearchStatus{Search: Search{ID: id, State: "pending", Region: cfg.Region, Compatibility: cfg.Compatibility}}, nil
}
func (f *bridgeBackend) CancelSearch(ctx context.Context, id, user string) (SearchResult, error) {
	f.calls = append(f.calls, "cancel_search")
	f.cancelSearchArgs = append(f.cancelSearchArgs, bridgeSearchCall{searchID: id, user: user})
	if f.cancelSearchFn != nil {
		return f.cancelSearchFn(ctx, id, user)
	}
	return SearchResult{}, nil
}
func (f *bridgeBackend) MatchSearches(ctx context.Context, key string, members []SearchMatchMember) (ReservationResult, error) {
	f.calls = append(f.calls, "match_searches")
	f.matchKeys = append(f.matchKeys, key)
	f.matchMembers = append(f.matchMembers, append([]SearchMatchMember(nil), members...))
	if f.matchSearchFn != nil {
		return f.matchSearchFn(ctx, key, members)
	}
	return ReservationResult{}, nil
}
func (f *bridgeBackend) Issue(ctx context.Context, allocationID, user, key string, previous int64, resume bool) (AssignmentResult, error) {
	f.calls = append(f.calls, "issue")
	f.issueArgs = append(f.issueArgs, bridgeIssueCall{allocationID, user, key, previous, resume})
	if f.issueFn != nil {
		return f.issueFn(ctx, allocationID, user, key, previous, resume)
	}
	return AssignmentResult{}, nil
}
func (f *bridgeBackend) Cancel(ctx context.Context, allocationID string) (ReservationResult, error) {
	f.calls = append(f.calls, "cancel")
	f.cancelIDs = append(f.cancelIDs, allocationID)
	if f.cancelFn != nil {
		return f.cancelFn(ctx, allocationID)
	}
	return ReservationResult{}, nil
}

func bridgeTestConfig() Config {
	return Config{Region: "local-west", Compatibility: "build-2026-09"}
}

func bridgeHooks(t *testing.T, backend Backend) (*bridgeInitializer, Config) {
	t.Helper()
	cfg := bridgeTestConfig()
	i := &bridgeInitializer{}
	if err := Register(i, backend, cfg); err != nil {
		t.Fatal(err)
	}
	if i.beforeName != "MatchmakerAdd" || i.before == nil || i.matched == nil {
		t.Fatal("bridge must register queue admission and matchmaker matched hooks")
	}
	wantRPCs := []string{CurrentRPC, StatusRPC, AssignmentRPC, ResumeRPC, CancelRPC, SearchBeginRPC, SearchStatusRPC, SearchCancelRPC}
	if len(i.rpcs) != len(wantRPCs) {
		t.Fatalf("registered RPCs = %v, want exactly %v", mapKeys(i.rpcs), wantRPCs)
	}
	for _, name := range wantRPCs {
		if i.rpcs[name] == nil {
			t.Fatalf("missing RPC %q", name)
		}
	}
	if len(i.registered) != 10 {
		t.Fatalf("registration touched unexpected runtime surfaces: %v", i.registered)
	}
	return i, cfg
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

func bridgeCtx(user string) context.Context {
	return context.WithValue(context.Background(), runtime.RUNTIME_CTX_USER_ID, user)
}

func bridgeProfilePayload() string {
	cfg := bridgeTestConfig()
	return fmt.Sprintf(`{"version":%q,"compatibility":%q,"region":%q}`, RoomVersion, cfg.Compatibility, cfg.Region)
}

func bridgeStatusPayload(allocationID string) string {
	cfg := bridgeTestConfig()
	return fmt.Sprintf(`{"version":%q,"compatibility":%q,"region":%q,"allocationId":%q}`, RoomVersion, cfg.Compatibility, cfg.Region, allocationID)
}

func bridgeSearchBeginPayload(requestID string) string {
	cfg := bridgeTestConfig()
	return fmt.Sprintf(`{"version":%q,"compatibility":%q,"region":%q,"requestId":%q}`, SearchVersion, cfg.Compatibility, cfg.Region, requestID)
}

func bridgeSearchParticipantPayload(searchID string) string {
	cfg := bridgeTestConfig()
	return fmt.Sprintf(`{"version":%q,"compatibility":%q,"region":%q,"searchId":%q}`, SearchVersion, cfg.Compatibility, cfg.Region, searchID)
}

func bridgeTicketPayload(allocationID, requestID string, previous int64) string {
	cfg := bridgeTestConfig()
	return fmt.Sprintf(`{"version":%q,"compatibility":%q,"region":%q,"allocationId":%q,"requestId":%q,"previousConnectionGeneration":%d}`,
		RoomVersion, cfg.Compatibility, cfg.Region, allocationID, requestID, previous)
}

func assertBridgeError(t *testing.T, err error, code int, message string) {
	t.Helper()
	var got *runtime.Error
	if !errors.As(err, &got) || got.Code != code || got.Message != message {
		t.Fatalf("got %v; want Nakama runtime error %d %q", err, code, message)
	}
}

func assertNoBackendCalls(t *testing.T, backend *bridgeBackend) {
	t.Helper()
	if len(backend.calls) != 0 {
		t.Fatalf("invalid request called backend: %v", backend.calls)
	}
}

func TestRegisterInstallsOnlyPlayerBridgeHooksAndRPCs(t *testing.T) {
	backend := &bridgeBackend{}
	i, _ := bridgeHooks(t, backend)
	if !reflect.DeepEqual(i.registered, []string{"before:MatchmakerAdd", "matched", "rpc:" + CurrentRPC, "rpc:" + StatusRPC, "rpc:" + AssignmentRPC, "rpc:" + ResumeRPC, "rpc:" + CancelRPC, "rpc:" + SearchBeginRPC, "rpc:" + SearchStatusRPC, "rpc:" + SearchCancelRPC}) {
		t.Fatalf("unexpected registrations: %v", i.registered)
	}
	assertNoBackendCalls(t, backend)
}

func matchmakerEnvelope(version, compatibility, region string, countMultiple *wrapperspb.Int32Value) *rtapi.Envelope {
	return &rtapi.Envelope{Cid: "request-17", Message: &rtapi.Envelope_MatchmakerAdd{MatchmakerAdd: &rtapi.MatchmakerAdd{
		MinCount: 2, MaxCount: 2, CountMultiple: countMultiple,
		Query:             "properties.region:local-west",
		StringProperties:  map[string]string{"gamefleet_protocol": version, "build_hash": compatibility, "region": region, "gamefleet_search_id": "search_player-a", "custom": "kept"},
		NumericProperties: map[string]float64{"skill": 1234},
	}}}
}

func TestMatchmakerAddRequiresAuthenticatedExactTwoPlayerProfileAndPreservesEnvelope(t *testing.T) {
	backend := &bridgeBackend{}
	i, cfg := bridgeHooks(t, backend)
	validSearchCalls := 0
	for _, tc := range []struct {
		name            string
		ctx             context.Context
		request         *rtapi.Envelope
		code            int
		message         string
		wantPassThrough bool
	}{
		{name: "missing_auth", ctx: context.Background(), request: matchmakerEnvelope(RoomVersion, cfg.Compatibility, cfg.Region, nil), code: 16, message: "user authentication required"},
		{name: "wrong_version", ctx: bridgeCtx("player-a"), request: matchmakerEnvelope("old", cfg.Compatibility, cfg.Region, nil), code: 9, message: "gamefleet_protocol_mismatch"},
		{name: "wrong_compatibility", ctx: bridgeCtx("player-a"), request: matchmakerEnvelope(RoomVersion, "old-build", cfg.Region, nil), code: 9, message: "fleet_build_mismatch"},
		{name: "wrong_region", ctx: bridgeCtx("player-a"), request: matchmakerEnvelope(RoomVersion, cfg.Compatibility, "other", nil), code: 9, message: "fleet_region_mismatch"},
		{name: "missing_search", ctx: bridgeCtx("player-a"), request: func() *rtapi.Envelope {
			e := matchmakerEnvelope(RoomVersion, cfg.Compatibility, cfg.Region, nil)
			delete(e.GetMatchmakerAdd().StringProperties, "gamefleet_search_id")
			return e
		}(), code: 3, message: "owned search required"},
		{name: "invalid_search", ctx: bridgeCtx("player-a"), request: func() *rtapi.Envelope {
			e := matchmakerEnvelope(RoomVersion, cfg.Compatibility, cfg.Region, nil)
			e.GetMatchmakerAdd().StringProperties["gamefleet_search_id"] = "short"
			return e
		}(), code: 3, message: "owned search required"},
		{name: "count_multiple_one", ctx: bridgeCtx("player-a"), request: matchmakerEnvelope(RoomVersion, cfg.Compatibility, cfg.Region, wrapperspb.Int32(1)), code: 3, message: "two-player match required"},
		{name: "count_multiple_three", ctx: bridgeCtx("player-a"), request: matchmakerEnvelope(RoomVersion, cfg.Compatibility, cfg.Region, wrapperspb.Int32(3)), code: 3, message: "two-player match required"},
		{name: "min_not_two", ctx: bridgeCtx("player-a"), request: func() *rtapi.Envelope {
			e := matchmakerEnvelope(RoomVersion, cfg.Compatibility, cfg.Region, wrapperspb.Int32(2))
			e.GetMatchmakerAdd().MinCount = 1
			return e
		}(), code: 3, message: "two-player match required"},
		{name: "max_not_two", ctx: bridgeCtx("player-a"), request: func() *rtapi.Envelope {
			e := matchmakerEnvelope(RoomVersion, cfg.Compatibility, cfg.Region, wrapperspb.Int32(2))
			e.GetMatchmakerAdd().MaxCount = 3
			return e
		}(), code: 3, message: "two-player match required"},
		{name: "nil_envelope", ctx: bridgeCtx("player-a"), request: nil, code: 3, message: "two-player match required"},
		{name: "valid_optional_multiple", ctx: bridgeCtx("player-a"), request: matchmakerEnvelope(RoomVersion, cfg.Compatibility, cfg.Region, nil), wantPassThrough: true},
		{name: "valid_explicit_multiple", ctx: bridgeCtx("player-a"), request: matchmakerEnvelope(RoomVersion, cfg.Compatibility, cfg.Region, wrapperspb.Int32(2)), wantPassThrough: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var original *rtapi.Envelope
			if tc.request != nil {
				original = proto.Clone(tc.request).(*rtapi.Envelope)
			}
			response, err := i.before(tc.ctx, nil, nil, nil, tc.request)
			if tc.wantPassThrough {
				if err != nil || response != tc.request || !proto.Equal(original, response) {
					t.Fatalf("valid envelope was not passed through unchanged: response=%v err=%v", response, err)
				}
				validSearchCalls++
				if len(backend.searchStatusArgs) != validSearchCalls {
					t.Fatalf("valid queue request did not check exactly one owned search: %+v", backend.searchStatusArgs)
				}
				if got := backend.searchStatusArgs[validSearchCalls-1]; got != (bridgeSearchCall{searchID: "search_player-a", user: "player-a"}) {
					t.Fatalf("admission did not check the authenticated user's exact search: %+v", got)
				}
				return
			}
			assertBridgeError(t, err, tc.code, tc.message)
			if response != nil {
				t.Fatal("rejected matchmaker request continued to admission")
			}
			if len(backend.searchStatusArgs) != validSearchCalls {
				t.Fatalf("invalid request called search status: %+v", backend.searchStatusArgs)
			}
		})
	}
	if len(backend.searchStatusArgs) != 2 || !reflect.DeepEqual(backend.calls, []string{"search_status", "search_status"}) {
		t.Fatalf("only the two valid requests should check owned search state: calls=%v args=%v", backend.calls, backend.searchStatusArgs)
	}
}

func TestMatchmakerAdmissionRequiresOwnedPendingCompatibleSearch(t *testing.T) {
	cfg := bridgeTestConfig()
	for _, tc := range []struct {
		name   string
		search Search
	}{
		{name: "foreign_search", search: Search{ID: "search_someone_else", State: "pending", Region: cfg.Region, Compatibility: cfg.Compatibility}},
		{name: "already_bound", search: Search{ID: "search_player-a", State: "bound", Region: cfg.Region, Compatibility: cfg.Compatibility}},
		{name: "wrong_region", search: Search{ID: "search_player-a", State: "pending", Region: "other", Compatibility: cfg.Compatibility}},
		{name: "wrong_compatibility", search: Search{ID: "search_player-a", State: "pending", Region: cfg.Region, Compatibility: "other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &bridgeBackend{searchStatusFn: func(_ context.Context, id, user string) (SearchStatus, error) {
				if id != "search_player-a" || user != "player-a" {
					t.Fatalf("search authorization inputs changed: id=%q user=%q", id, user)
				}
				return SearchStatus{Search: tc.search}, nil
			}}
			i, _ := bridgeHooks(t, backend)
			request := matchmakerEnvelope(RoomVersion, cfg.Compatibility, cfg.Region, nil)
			response, err := i.before(bridgeCtx("player-a"), nil, nil, nil, request)
			assertBridgeError(t, err, 9, "search unavailable")
			if response != nil || !reflect.DeepEqual(backend.calls, []string{"search_status"}) || !reflect.DeepEqual(backend.searchStatusArgs, []bridgeSearchCall{{searchID: "search_player-a", user: "player-a"}}) {
				t.Fatalf("non-owned or non-pending search reached queue: response=%v calls=%v args=%v", response, backend.calls, backend.searchStatusArgs)
			}
		})
	}
}

type bridgePresence struct {
	runtime.Presence
	user string
}

func (p bridgePresence) GetUserId() string { return p.user }

type bridgeEntry struct {
	runtime.MatchmakerEntry
	presence   runtime.Presence
	ticket     string
	properties map[string]any
}

func (e bridgeEntry) GetPresence() runtime.Presence { return e.presence }
func (e bridgeEntry) GetTicket() string             { return e.ticket }
func (e bridgeEntry) GetProperties() map[string]any { return e.properties }

func bridgeMatchEntry(user, ticket string, cfg Config) runtime.MatchmakerEntry {
	return bridgeEntry{
		presence: bridgePresence{user: user}, ticket: ticket,
		properties: map[string]any{"gamefleet_protocol": RoomVersion, "build_hash": cfg.Compatibility, "region": cfg.Region, "gamefleet_search_id": "search_" + strings.ReplaceAll(user, "-", "_")},
	}
}

func TestMatchedUsesCanonicalOwnedSearchAndTicketSet(t *testing.T) {
	backend := &bridgeBackend{}
	i, cfg := bridgeHooks(t, backend)
	valid := []runtime.MatchmakerEntry{bridgeMatchEntry("player-b", "ticket-b", cfg), bridgeMatchEntry("player-a", "ticket-a", cfg)}
	if _, err := i.matched(context.Background(), nil, nil, nil, valid); err != nil {
		t.Fatalf("valid pair failed: %v", err)
	}
	wantMembers := []SearchMatchMember{{ParticipantID: "player-a", SearchID: "search_player_a", NakamaTicket: "ticket-a"}, {ParticipantID: "player-b", SearchID: "search_player_b", NakamaTicket: "ticket-b"}}
	if len(backend.matchKeys) != 1 || !reflect.DeepEqual(backend.matchMembers[0], wantMembers) {
		t.Fatalf("match must use trusted users, search IDs and tickets in canonical order: members=%v keys=%v", backend.matchMembers, backend.matchKeys)
	}
	key := backend.matchKeys[0]
	if !strings.HasPrefix(key, "matchmaker_search_") || len(key) != len("matchmaker_search_")+64 {
		t.Fatalf("unexpected match idempotency key format %q", key)
	}
	_, err := i.matched(context.Background(), nil, nil, nil, []runtime.MatchmakerEntry{valid[1], valid[0]})
	if err != nil {
		t.Fatalf("reversed pair failed: %v", err)
	}
	if len(backend.matchKeys) != 2 || backend.matchKeys[1] != key || !reflect.DeepEqual(backend.matchMembers[1], wantMembers) {
		t.Fatalf("reversing Nakama entries must preserve the same match request: keys=%v members=%v", backend.matchKeys, backend.matchMembers)
	}
}

func TestMatchedRejectsMalformedPairBeforeMatchSearches(t *testing.T) {
	backend := &bridgeBackend{}
	i, cfg := bridgeHooks(t, backend)
	wrongProfile := bridgeEntry{presence: bridgePresence{user: "player-b"}, ticket: "ticket-b", properties: map[string]any{"gamefleet_protocol": RoomVersion, "build_hash": "old-build", "region": cfg.Region}}
	missingPresence := bridgeEntry{ticket: "ticket-b", properties: map[string]any{"gamefleet_protocol": RoomVersion, "build_hash": cfg.Compatibility, "region": cfg.Region}}
	badPropertyType := bridgeEntry{presence: bridgePresence{user: "player-b"}, ticket: "ticket-b", properties: map[string]any{"gamefleet_protocol": 1, "build_hash": cfg.Compatibility, "region": cfg.Region}}
	for _, tc := range []struct {
		name    string
		entries []runtime.MatchmakerEntry
		code    int
		message string
	}{
		{"no_entries", nil, 3, "two-player match required"},
		{"one_entry", []runtime.MatchmakerEntry{bridgeMatchEntry("player-a", "ticket-a", cfg)}, 3, "two-player match required"},
		{"three_entries", []runtime.MatchmakerEntry{bridgeMatchEntry("player-a", "ticket-a", cfg), bridgeMatchEntry("player-b", "ticket-b", cfg), bridgeMatchEntry("player-c", "ticket-c", cfg)}, 3, "two-player match required"},
		{"nil_entry", []runtime.MatchmakerEntry{nil, bridgeMatchEntry("player-b", "ticket-b", cfg)}, 3, "invalid match entry"},
		{"missing_presence", []runtime.MatchmakerEntry{missingPresence, bridgeMatchEntry("player-a", "ticket-a", cfg)}, 3, "invalid match entry"},
		{"wrong_profile", []runtime.MatchmakerEntry{wrongProfile, bridgeMatchEntry("player-a", "ticket-a", cfg)}, 9, "fleet_build_mismatch"},
		{"wrong_property_type", []runtime.MatchmakerEntry{badPropertyType, bridgeMatchEntry("player-a", "ticket-a", cfg)}, 9, "gamefleet_protocol_mismatch"},
		{"duplicate_presence_user", []runtime.MatchmakerEntry{bridgeMatchEntry("player-a", "ticket-a", cfg), bridgeMatchEntry("player-a", "ticket-b", cfg)}, 3, "distinct players required"},
		{"duplicate_search", []runtime.MatchmakerEntry{bridgeMatchEntry("player-a", "ticket-a", cfg), bridgeEntry{presence: bridgePresence{user: "player-b"}, ticket: "ticket-b", properties: map[string]any{"gamefleet_protocol": RoomVersion, "build_hash": cfg.Compatibility, "region": cfg.Region, "gamefleet_search_id": "search_player_a"}}}, 3, "distinct players required"},
		{"invalid_ticket", []runtime.MatchmakerEntry{bridgeMatchEntry("player-a", "", cfg), bridgeMatchEntry("player-b", "ticket-b", cfg)}, 3, "invalid match entry"},
		{"missing_search", []runtime.MatchmakerEntry{bridgeEntry{presence: bridgePresence{user: "player-a"}, ticket: "ticket-a", properties: map[string]any{"gamefleet_protocol": RoomVersion, "build_hash": cfg.Compatibility, "region": cfg.Region}}, bridgeMatchEntry("player-b", "ticket-b", cfg)}, 3, "invalid match entry"},
		{"wrong_search_type", []runtime.MatchmakerEntry{bridgeEntry{presence: bridgePresence{user: "player-a"}, ticket: "ticket-a", properties: map[string]any{"gamefleet_protocol": RoomVersion, "build_hash": cfg.Compatibility, "region": cfg.Region, "gamefleet_search_id": 1}}, bridgeMatchEntry("player-b", "ticket-b", cfg)}, 3, "invalid match entry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := i.matched(context.Background(), nil, nil, nil, tc.entries)
			assertBridgeError(t, err, tc.code, tc.message)
		})
	}
	if len(backend.matchKeys) != 0 {
		t.Fatalf("invalid matched callback reached search match: %v", backend.matchKeys)
	}
}

func TestCurrentRPCUsesAuthenticatedContextUserAndRejectsCallerParticipantField(t *testing.T) {
	backend := &bridgeBackend{currentFn: func(_ context.Context, user string) (CurrentResult, error) {
		return CurrentResult{Current: &Current{Reservation: Reservation{AllocationID: "alloc-self"}, ConnectionGeneration: 12}}, nil
	}}
	i, _ := bridgeHooks(t, backend)
	ctx := bridgeCtx("authenticated-player")
	result, err := i.rpcs[CurrentRPC](ctx, nil, nil, nil, bridgeProfilePayload())
	if err != nil || !strings.Contains(result, `"alloc-self"`) {
		t.Fatalf("current lookup failed: result=%q err=%v", result, err)
	}
	if !reflect.DeepEqual(backend.currentUsers, []string{"authenticated-player"}) {
		t.Fatalf("current lookup must use authenticated Nakama user, got %v", backend.currentUsers)
	}
	malicious := strings.TrimSuffix(bridgeProfilePayload(), "}") + `,"participantId":"victim-player"}`
	_, err = i.rpcs[CurrentRPC](ctx, nil, nil, nil, malicious)
	assertBridgeError(t, err, 3, "invalid payload")
	if len(backend.currentUsers) != 1 {
		t.Fatal("caller-supplied participantId must be rejected before a lookup")
	}
}

func TestStatusRPCUsesAuthenticatedUserWithoutCurrentReservationCheck(t *testing.T) {
	backend := &bridgeBackend{statusFn: func(_ context.Context, allocationID, user string) (ReservationStatus, error) {
		if allocationID != "alloc_123" || user != "authenticated-player" {
			t.Fatalf("status request used caller data: allocation=%q user=%q", allocationID, user)
		}
		r := Reservation{AllocationID: allocationID, State: "technical_aborted", FailureCode: "host_process_terminated"}
		return ReservationStatus{Reservation: r}, nil
	}}
	i, _ := bridgeHooks(t, backend)
	result, err := i.rpcs[StatusRPC](bridgeCtx("authenticated-player"), nil, nil, nil, bridgeStatusPayload("alloc_123"))
	if err != nil || !strings.Contains(result, `"technical_aborted"`) || !strings.Contains(result, `"host_process_terminated"`) {
		t.Fatalf("technical status lookup failed: result=%q err=%v", result, err)
	}
	if !reflect.DeepEqual(backend.calls, []string{"status"}) || !reflect.DeepEqual(backend.statusArgs, []bridgeStatusCall{{"alloc_123", "authenticated-player"}}) || len(backend.currentUsers) != 0 {
		t.Fatalf("status must call the history endpoint directly with the authenticated user: calls=%v args=%v current=%v", backend.calls, backend.statusArgs, backend.currentUsers)
	}

	forged := strings.TrimSuffix(bridgeStatusPayload("alloc_123"), "}") + `,"participantId":"victim-player"}`
	_, err = i.rpcs[StatusRPC](bridgeCtx("authenticated-player"), nil, nil, nil, forged)
	assertBridgeError(t, err, 3, "invalid payload")
	if len(backend.statusArgs) != 1 {
		t.Fatal("caller-supplied participantId reached status endpoint")
	}
}

func TestPlayerRPCAuthenticationPayloadAndProfilesPreflightWithoutBackendCalls(t *testing.T) {
	backend := &bridgeBackend{}
	i, cfg := bridgeHooks(t, backend)
	for route, payload := range map[string]string{
		CurrentRPC:      bridgeProfilePayload(),
		StatusRPC:       bridgeStatusPayload("alloc_123"),
		CancelRPC:       fmt.Sprintf(`{"version":%q,"compatibility":%q,"region":%q,"allocationId":"alloc_123"}`, RoomVersion, cfg.Compatibility, cfg.Region),
		AssignmentRPC:   bridgeTicketPayload("alloc_123", "attempt_123", 0),
		ResumeRPC:       bridgeTicketPayload("alloc_123", "attempt_123", 1),
		SearchBeginRPC:  bridgeSearchBeginPayload("attempt_123"),
		SearchStatusRPC: bridgeSearchParticipantPayload("search_123"),
		SearchCancelRPC: bridgeSearchParticipantPayload("search_123"),
	} {
		if _, err := i.rpcs[route](context.Background(), nil, nil, nil, payload); err == nil {
			t.Fatalf("%s accepted an unauthenticated request", route)
		} else {
			assertBridgeError(t, err, 16, "user authentication required")
		}
		for _, malformed := range []string{"", "[]", `{"version":`, payload + ` {}`, strings.Repeat("x", 8193)} {
			_, err := i.rpcs[route](bridgeCtx("player-a"), nil, nil, nil, malformed)
			assertBridgeError(t, err, 3, "invalid payload")
		}
	}
	for _, badProfile := range []struct{ version, compatibility, region, want string }{
		{"wrong", cfg.Compatibility, cfg.Region, "gamefleet_protocol_mismatch"},
		{RoomVersion, "wrong", cfg.Region, "fleet_build_mismatch"},
		{RoomVersion, cfg.Compatibility, "wrong", "fleet_region_mismatch"},
	} {
		bad := fmt.Sprintf(`{"version":%q,"compatibility":%q,"region":%q}`, badProfile.version, badProfile.compatibility, badProfile.region)
		_, err := i.rpcs[CurrentRPC](bridgeCtx("player-a"), nil, nil, nil, bad)
		assertBridgeError(t, err, 9, badProfile.want)
	}
	for _, tc := range []struct{ route, payload string }{
		{AssignmentRPC, bridgeTicketPayload("alloc_123", "short", 0)},
		{AssignmentRPC, bridgeTicketPayload("alloc_123", "", 0)},
		{AssignmentRPC, `{"version":"gamefleet.player-room.v1","compatibility":"build-2026-09","region":"local-west","allocationId":"alloc_123","previousConnectionGeneration":0}`},
		{AssignmentRPC, bridgeTicketPayload("alloc_123", "attempt_123", 1)},
		{ResumeRPC, bridgeTicketPayload("alloc_123", "attempt_123", 0)},
		{ResumeRPC, `{"version":"gamefleet.player-room.v1","compatibility":"build-2026-09","region":"local-west","allocationId":"alloc_123","requestId":"attempt_123","previousConnectionGeneration":null}`},
		{CancelRPC, fmt.Sprintf(`{"version":%q,"compatibility":%q,"region":%q,"allocationId":"bad/id"}`, RoomVersion, cfg.Compatibility, cfg.Region)},
		{StatusRPC, bridgeStatusPayload("bad/id")},
		{SearchBeginRPC, bridgeSearchBeginPayload("short")},
		{SearchStatusRPC, bridgeSearchParticipantPayload("short")},
		{SearchCancelRPC, bridgeSearchParticipantPayload("short")},
	} {
		_, err := i.rpcs[tc.route](bridgeCtx("player-a"), nil, nil, nil, tc.payload)
		assertBridgeError(t, err, 3, "invalid payload")
	}
	for _, route := range []string{SearchBeginRPC, SearchStatusRPC, SearchCancelRPC} {
		payload := bridgeSearchBeginPayload("attempt_123")
		if route != SearchBeginRPC {
			payload = bridgeSearchParticipantPayload("search_123")
		}
		wrongVersion := strings.Replace(payload, SearchVersion, RoomVersion, 1)
		_, err := i.rpcs[route](bridgeCtx("player-a"), nil, nil, nil, wrongVersion)
		assertBridgeError(t, err, 9, "gamefleet_protocol_mismatch")
	}
	assertNoBackendCalls(t, backend)
}

func TestAssignmentAndResumeCheckOwnershipBeforeIssueAndPreserveRetryIdentity(t *testing.T) {
	backend := &bridgeBackend{currentFn: func(_ context.Context, _ string) (CurrentResult, error) {
		return CurrentResult{Current: &Current{Reservation: Reservation{AllocationID: "alloc_123"}, ConnectionGeneration: 91}}, nil
	}}
	i, _ := bridgeHooks(t, backend)
	ctx := bridgeCtx("player-a")
	request := bridgeTicketPayload("alloc_123", "attempt_123", 7)
	for n := 0; n < 2; n++ {
		result, err := i.rpcs[ResumeRPC](ctx, nil, nil, nil, request)
		if err != nil || result == "" {
			t.Fatalf("resume attempt %d failed: %q %v", n, result, err)
		}
	}
	if !reflect.DeepEqual(backend.calls, []string{"current", "issue", "current", "issue"}) {
		t.Fatalf("resume must verify ownership immediately before each issue: %v", backend.calls)
	}
	first, second := backend.issueArgs[0], backend.issueArgs[1]
	if first != second || first.user != "player-a" || first.previous != 7 || !first.resume || first.allocationID != "alloc_123" {
		t.Fatalf("retries must preserve user, allocation, key, and caller previous generation despite lookup generation 91: %#v %#v", first, second)
	}
	if !strings.HasPrefix(first.key, "player_") || len(first.key) != len("player_")+64 {
		t.Fatalf("unexpected player idempotency key %q", first.key)
	}
	_, err := i.rpcs[ResumeRPC](bridgeCtx("player-b"), nil, nil, nil, request)
	if err != nil {
		t.Fatalf("second authorized player resume failed: %v", err)
	}
	third := backend.issueArgs[2]
	if third.user != "player-b" || third.key == first.key {
		t.Fatalf("different authenticated players must be isolated by idempotency key: first=%#v third=%#v", first, third)
	}
	_, err = i.rpcs[AssignmentRPC](ctx, nil, nil, nil, bridgeTicketPayload("alloc_123", "attempt_123", 0))
	if err != nil {
		t.Fatalf("assignment failed: %v", err)
	}
	assignment := backend.issueArgs[3]
	if assignment.resume || assignment.previous != 0 || assignment.key == first.key {
		t.Fatalf("assignment key must be scoped to route and preserve assignment semantics: %#v", assignment)
	}
}

func TestAssignmentResumeAndCancelRequireCurrentOwnership(t *testing.T) {
	backend := &bridgeBackend{currentFn: func(_ context.Context, user string) (CurrentResult, error) {
		if user == "player-a" {
			return CurrentResult{Current: &Current{Reservation: Reservation{AllocationID: "alloc_other"}}}, nil
		}
		return CurrentResult{}, nil
	}}
	i, _ := bridgeHooks(t, backend)
	for _, tc := range []struct{ route, payload string }{
		{AssignmentRPC, bridgeTicketPayload("alloc_123", "attempt_123", 0)},
		{ResumeRPC, bridgeTicketPayload("alloc_123", "attempt_123", 1)},
		{CancelRPC, fmt.Sprintf(`{"version":%q,"compatibility":%q,"region":%q,"allocationId":"alloc_123"}`, RoomVersion, bridgeTestConfig().Compatibility, bridgeTestConfig().Region)},
	} {
		result, err := i.rpcs[tc.route](bridgeCtx("player-a"), nil, nil, nil, tc.payload)
		assertBridgeError(t, err, 5, "reservation unavailable")
		if result != "" || strings.Contains(err.Error(), "alloc_other") {
			t.Fatalf("foreign allocation identity was returned: result=%q err=%v", result, err)
		}
	}
	if !reflect.DeepEqual(backend.calls, []string{"current", "current", "current"}) || len(backend.issueArgs) != 0 || len(backend.cancelIDs) != 0 {
		t.Fatalf("ownership must be checked before assignment/resume/cancel: calls=%v issue=%v cancel=%v", backend.calls, backend.issueArgs, backend.cancelIDs)
	}
}

func TestCancelCurrentReservationAndBackendErrorsAreSanitized(t *testing.T) {
	backend := &bridgeBackend{
		currentFn: func(_ context.Context, _ string) (CurrentResult, error) {
			return CurrentResult{Current: &Current{Reservation: Reservation{AllocationID: "alloc_123"}}}, nil
		},
		cancelFn: func(_ context.Context, id string) (ReservationResult, error) {
			if id != "alloc_123" {
				return ReservationResult{}, fmt.Errorf("wrong id %s", id)
			}
			return ReservationResult{Reservation: Reservation{AllocationID: id}, Replay: true}, nil
		},
	}
	i, cfg := bridgeHooks(t, backend)
	cancelPayload := fmt.Sprintf(`{"version":%q,"compatibility":%q,"region":%q,"allocationId":"alloc_123"}`, RoomVersion, cfg.Compatibility, cfg.Region)
	result, err := i.rpcs[CancelRPC](bridgeCtx("player-a"), nil, nil, nil, cancelPayload)
	if err != nil || !strings.Contains(result, `"replay":true`) || !reflect.DeepEqual(backend.cancelIDs, []string{"alloc_123"}) {
		t.Fatalf("owned reservation cancellation failed: result=%q err=%v calls=%v", result, err, backend.calls)
	}

	const secretBody = "upstream body contains bearer-secret and ticket-secret"
	backend.currentFn = func(_ context.Context, _ string) (CurrentResult, error) {
		return CurrentResult{}, errors.New(secretBody)
	}
	result, err = i.rpcs[CurrentRPC](bridgeCtx("player-a"), nil, nil, nil, bridgeProfilePayload())
	assertBridgeError(t, err, 14, "gamefleet temporarily unavailable; retry the same request")
	if result != "" || strings.Contains(err.Error(), "bearer-secret") || strings.Contains(err.Error(), "ticket-secret") || strings.Contains(err.Error(), secretBody) {
		t.Fatalf("upstream details leaked in player error: result=%q err=%v", result, err)
	}

	backend.currentFn = func(_ context.Context, _ string) (CurrentResult, error) {
		return CurrentResult{Current: &Current{Reservation: Reservation{AllocationID: "alloc_123"}}}, nil
	}
	backend.issueFn = func(_ context.Context, _, _, _ string, _ int64, _ bool) (AssignmentResult, error) {
		return AssignmentResult{}, fmt.Errorf("HTTP 502: %s", secretBody)
	}
	result, err = i.rpcs[AssignmentRPC](bridgeCtx("player-a"), nil, nil, nil, bridgeTicketPayload("alloc_123", "attempt_123", 0))
	assertBridgeError(t, err, 14, "gamefleet temporarily unavailable; retry the same request")
	if result != "" || strings.Contains(err.Error(), "bearer-secret") || strings.Contains(err.Error(), "ticket-secret") || strings.Contains(err.Error(), secretBody) {
		t.Fatalf("assignment upstream body/token leaked: result=%q err=%v", result, err)
	}
}
