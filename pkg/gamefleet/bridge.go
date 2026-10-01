package gamefleet

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"

	"github.com/heroiclabs/nakama-common/rtapi"
	"github.com/heroiclabs/nakama-common/runtime"
)

const (
	CurrentRPC      = "gamefleet_current_v1"
	StatusRPC       = "gamefleet_status_v1"
	AssignmentRPC   = "gamefleet_assignment_v1"
	ResumeRPC       = "gamefleet_resume_v1"
	CancelRPC       = "gamefleet_cancel_v1"
	SearchBeginRPC  = "gamefleet_search_begin_v1"
	SearchStatusRPC = "gamefleet_search_status_v1"
	SearchCancelRPC = "gamefleet_search_cancel_v1"
)

// Register owns the single matched hook, but never registers FleetManager,
// database migrations, HTTP/admin endpoints or a background reconciler.
func Register(initializer runtime.Initializer, backend Backend, cfg Config) error {
	return registerBridge(initializer, bridge{backend: backend, config: cfg})
}

func registerBridge(initializer runtime.Initializer, b bridge) error {
	if initializer == nil || b.backend == nil || !exactText(b.config.Region, 128) || !exactText(b.config.Compatibility, 128) {
		return errors.New("invalid GameFleet bridge configuration")
	}
	if b.matchingRoutedSearchStatus != nil && (b.matchingSearchStatus != nil || len(b.config.Region) > 64 || len(b.config.Compatibility) > 64) {
		return errors.New("invalid routed GameFleet bridge configuration")
	}
	if err := initializer.RegisterBeforeRt("MatchmakerAdd", b.before); err != nil {
		return err
	}
	if err := initializer.RegisterMatchmakerMatched(b.matched); err != nil {
		return err
	}
	for _, route := range []string{CurrentRPC, StatusRPC, AssignmentRPC, ResumeRPC, CancelRPC, SearchBeginRPC, SearchStatusRPC, SearchCancelRPC} {
		name := route
		if err := initializer.RegisterRpc(name, func(ctx context.Context, _ runtime.Logger, _ *sql.DB, _ runtime.NakamaModule, payload string) (string, error) {
			return b.rpc(ctx, name, payload)
		}); err != nil {
			return err
		}
	}
	return nil
}

type bridge struct {
	backend Backend
	config  Config
	// Existing records may be readable through History without being mapped to
	// this matching service. Admission to Matchmaker uses this separate check.
	matchingSearchStatus func(context.Context, string, string) (SearchStatus, error)
	// Explicit v2 mode separates pool metadata from player recovery DTOs;
	// recovery continues using its independent History authority.
	matchingRoutedSearchStatus func(context.Context, string, string) (RoutedSearchStatus, error)
}

func authenticatedUser(ctx context.Context) (string, error) {
	user, _ := ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string)
	if !exactText(user, 256) {
		return "", runtime.NewError("user authentication required", 16)
	}
	return user, nil
}

func (b bridge) profile(version, compatibility, region string) error {
	if version != RoomVersion {
		return runtime.NewError("gamefleet_protocol_mismatch", 9)
	}
	if compatibility != b.config.Compatibility {
		return runtime.NewError("fleet_build_mismatch", 9)
	}
	if region != b.config.Region {
		return runtime.NewError("fleet_region_mismatch", 9)
	}
	return nil
}

func (b bridge) before(ctx context.Context, _ runtime.Logger, _ *sql.DB, _ runtime.NakamaModule, in *rtapi.Envelope) (*rtapi.Envelope, error) {
	user, err := authenticatedUser(ctx)
	if err != nil {
		return nil, err
	}
	r := in.GetMatchmakerAdd()
	if r == nil || r.MinCount != 2 || r.MaxCount != 2 || (r.CountMultiple != nil && r.CountMultiple.Value != 2) {
		return nil, runtime.NewError("two-player match required", 3)
	}
	p := r.StringProperties
	if err := b.profile(p["gamefleet_protocol"], p["build_hash"], p["region"]); err != nil {
		return nil, err
	}
	id := p["gamefleet_search_id"]
	if !attemptID.MatchString(id) {
		return nil, runtime.NewError("owned search required", 3)
	}
	if b.matchingRoutedSearchStatus != nil {
		return b.beforeRouted(ctx, in, id, user)
	}
	status := b.backend.SearchStatus
	if b.matchingSearchStatus != nil {
		status = b.matchingSearchStatus
	}
	search, err := status(ctx, id, user)
	if err != nil {
		return nil, playerSearchError(err)
	}
	if search.Search.ID != id || search.Search.State != "pending" || search.Search.Region != b.config.Region || search.Search.Compatibility != b.config.Compatibility {
		return nil, runtime.NewError("search unavailable", 9)
	}
	return in, nil
}

func (b bridge) matched(ctx context.Context, _ runtime.Logger, _ *sql.DB, _ runtime.NakamaModule, entries []runtime.MatchmakerEntry) (string, error) {
	if len(entries) != 2 {
		return "", runtime.NewError("two-player match required", 3)
	}
	members := make([]SearchMatchMember, 0, 2)
	for _, entry := range entries {
		if entry == nil || entry.GetPresence() == nil {
			return "", runtime.NewError("invalid match entry", 3)
		}
		p := entry.GetProperties()
		version, _ := p["gamefleet_protocol"].(string)
		compatibility, _ := p["build_hash"].(string)
		region, _ := p["region"].(string)
		if err := b.profile(version, compatibility, region); err != nil {
			return "", err
		}
		user, ticket := entry.GetPresence().GetUserId(), entry.GetTicket()
		search, _ := p["gamefleet_search_id"].(string)
		if !exactText(user, 256) || !exactText(ticket, 256) || !attemptID.MatchString(search) {
			return "", runtime.NewError("invalid match entry", 3)
		}
		members = append(members, SearchMatchMember{ParticipantID: user, SearchID: search, NakamaTicket: ticket})
	}
	if members[0].ParticipantID == members[1].ParticipantID || members[0].SearchID == members[1].SearchID || members[0].NakamaTicket == members[1].NakamaTicket {
		return "", runtime.NewError("distinct players required", 3)
	}
	if b.matchingRoutedSearchStatus != nil {
		if err := b.checkRoutedMatch(ctx, entries, members); err != nil {
			return "", err
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].ParticipantID < members[j].ParticipantID })
	key := stableKey("matchmaker_search_", []any{SearchVersion, b.config.Region, b.config.Compatibility, members})
	_, err := b.backend.MatchSearches(ctx, key, members)
	// Nakama emits its ordinary matched signal. Clients recover the exact bound
	// search if that signal or this HTTP response is lost after commit.
	return "", playerSearchError(err)
}

type profileRequest struct {
	Version       string `json:"version"`
	Compatibility string `json:"compatibility"`
	Region        string `json:"region"`
}
type ticketRequest struct {
	profileRequest
	AllocationID string `json:"allocationId"`
	RequestID    string `json:"requestId"`
	Previous     *int64 `json:"previousConnectionGeneration"`
}
type cancelRequest struct {
	profileRequest
	AllocationID string `json:"allocationId"`
}
type statusRequest struct {
	profileRequest
	AllocationID string `json:"allocationId"`
}

type searchBeginRequest struct {
	profileRequest
	RequestID string `json:"requestId"`
}

type searchParticipantRequest struct {
	profileRequest
	SearchID string `json:"searchId"`
}

func (b bridge) searchProfile(version, compatibility, region string) error {
	if version != SearchVersion {
		return runtime.NewError("gamefleet_protocol_mismatch", 9)
	}
	return b.profile(RoomVersion, compatibility, region)
}

func decodePlayer(payload string, out any) error {
	if len(payload) > 8192 || strictObject([]byte(payload), out) != nil {
		return runtime.NewError("invalid payload", 3)
	}
	return nil
}

func (b bridge) ownReservation(ctx context.Context, user, id string) error {
	current, err := b.backend.Current(ctx, user)
	if err != nil {
		return playerError(err)
	}
	if current.Current == nil || current.Current.Reservation.AllocationID != id {
		return runtime.NewError("reservation unavailable", 5)
	}
	return nil
}

func (b bridge) rpc(ctx context.Context, route, payload string) (string, error) {
	user, err := authenticatedUser(ctx)
	if err != nil {
		return "", err
	}
	var result any
	switch route {
	case SearchBeginRPC:
		var req searchBeginRequest
		if err = decodePlayer(payload, &req); err != nil {
			return "", err
		}
		if err = b.searchProfile(req.Version, req.Compatibility, req.Region); err != nil {
			return "", err
		}
		if !attemptID.MatchString(req.RequestID) {
			return "", runtime.NewError("invalid payload", 3)
		}
		key := stableKey("search_begin_", []string{user, req.RequestID})
		result, err = b.backend.BeginSearch(ctx, user, key)
	case SearchStatusRPC, SearchCancelRPC:
		var req searchParticipantRequest
		if err = decodePlayer(payload, &req); err != nil {
			return "", err
		}
		if !attemptID.MatchString(req.SearchID) {
			return "", runtime.NewError("invalid payload", 3)
		}
		if route == SearchStatusRPC {
			if err = b.searchProfile(req.Version, req.Compatibility, req.Region); err != nil {
				archive, ok := b.backend.(archiveProfileReader)
				if !ok || !archive.archiveSearchProfile(req.Version, req.Compatibility, req.Region) {
					return "", err
				}
				result, err = archive.archiveSearchStatus(ctx, req.SearchID, user)
			} else {
				result, err = b.backend.SearchStatus(ctx, req.SearchID, user)
			}
		} else {
			if err = b.searchProfile(req.Version, req.Compatibility, req.Region); err != nil {
				return "", err
			}
			result, err = b.backend.CancelSearch(ctx, req.SearchID, user)
		}
	case CurrentRPC:
		var req profileRequest
		if err = decodePlayer(payload, &req); err != nil {
			return "", err
		}
		if err = b.profile(req.Version, req.Compatibility, req.Region); err != nil {
			return "", err
		}
		result, err = b.backend.Current(ctx, user)
	case StatusRPC:
		var req statusRequest
		if err = decodePlayer(payload, &req); err != nil {
			return "", err
		}
		if !opaqueID.MatchString(req.AllocationID) {
			return "", runtime.NewError("invalid payload", 3)
		}
		// Status is authorized from historical seat ownership by the business
		// API, so it remains readable after the current reservation is released.
		if err = b.profile(req.Version, req.Compatibility, req.Region); err != nil {
			archive, ok := b.backend.(archiveProfileReader)
			if !ok || !archive.archiveRoomProfile(req.Version, req.Compatibility, req.Region) {
				return "", err
			}
			result, err = archive.archiveStatus(ctx, req.AllocationID, user)
		} else {
			result, err = b.backend.Status(ctx, req.AllocationID, user)
		}
	case CancelRPC:
		var req cancelRequest
		if err = decodePlayer(payload, &req); err != nil {
			return "", err
		}
		if err = b.profile(req.Version, req.Compatibility, req.Region); err != nil {
			return "", err
		}
		if !opaqueID.MatchString(req.AllocationID) {
			return "", runtime.NewError("invalid payload", 3)
		}
		if err = b.ownReservation(ctx, user, req.AllocationID); err != nil {
			return "", err
		}
		result, err = b.backend.Cancel(ctx, req.AllocationID)
	case AssignmentRPC, ResumeRPC:
		var req ticketRequest
		if err = decodePlayer(payload, &req); err != nil {
			return "", err
		}
		if err = b.profile(req.Version, req.Compatibility, req.Region); err != nil {
			return "", err
		}
		resume := route == ResumeRPC
		if !opaqueID.MatchString(req.AllocationID) || !attemptID.MatchString(req.RequestID) || req.Previous == nil || *req.Previous < 0 || *req.Previous >= MaxGeneration || (!resume && *req.Previous != 0) || (resume && *req.Previous == 0) {
			return "", runtime.NewError("invalid payload", 3)
		}
		if err = b.ownReservation(ctx, user, req.AllocationID); err != nil {
			return "", err
		}
		// Do not replace the caller's old generation with the current snapshot:
		// exact retries must preserve their original request and ticket identity.
		key := stableKey("player_", []string{user, req.AllocationID, route, req.RequestID})
		result, err = b.backend.Issue(ctx, req.AllocationID, user, key, *req.Previous, resume)
	default:
		return "", runtime.NewError("unknown operation", 3)
	}
	if err != nil {
		if route == SearchBeginRPC || route == SearchStatusRPC || route == SearchCancelRPC {
			return "", playerSearchError(err)
		}
		return "", playerError(err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return "", runtime.NewError("gamefleet response unavailable", 13)
	}
	return string(raw), nil
}

func stableKey(prefix string, value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return prefix + hex.EncodeToString(sum[:])
}

func playerSearchError(err error) error {
	var upstream *Error
	if errors.As(err, &upstream) && upstream.Status == 409 {
		// Current cannot determine whether an exact search is still pending,
		// cancelled or bound. Preserve correlation in the recovery instruction.
		return runtime.NewError("gamefleet search conflict; query the same search", 9)
	}
	return playerError(err)
}

func playerError(err error) error {
	if err == nil {
		return nil
	}
	var upstream *Error
	if errors.As(err, &upstream) {
		switch upstream.Status {
		case 403:
			return runtime.NewError("gamefleet access denied", 7)
		case 404:
			return runtime.NewError("reservation unavailable", 5)
		case 409:
			return runtime.NewError("gamefleet state conflict; refresh current reservation", 9)
		case 422:
			return runtime.NewError("invalid gamefleet request", 3)
		case 429:
			return runtime.NewError("gamefleet rate limited", 8)
		}
	}
	return runtime.NewError("gamefleet temporarily unavailable; retry the same request", 14)
}
