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
	CurrentRPC    = "gamefleet_current_v1"
	AssignmentRPC = "gamefleet_assignment_v1"
	ResumeRPC     = "gamefleet_resume_v1"
	CancelRPC     = "gamefleet_cancel_v1"
)

// Register owns the single matched hook, but never registers FleetManager,
// database migrations, HTTP/admin endpoints or a background reconciler.
func Register(initializer runtime.Initializer, backend Backend, cfg Config) error {
	if backend == nil || !exactText(cfg.Region, 128) || !exactText(cfg.Compatibility, 128) {
		return errors.New("invalid GameFleet bridge configuration")
	}
	b := bridge{backend: backend, config: cfg}
	if err := initializer.RegisterBeforeRt("MatchmakerAdd", b.before); err != nil {
		return err
	}
	if err := initializer.RegisterMatchmakerMatched(b.matched); err != nil {
		return err
	}
	for _, route := range []string{CurrentRPC, AssignmentRPC, ResumeRPC, CancelRPC} {
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
	if _, err := authenticatedUser(ctx); err != nil {
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
	return in, nil
}

func (b bridge) matched(ctx context.Context, _ runtime.Logger, _ *sql.DB, _ runtime.NakamaModule, entries []runtime.MatchmakerEntry) (string, error) {
	if len(entries) != 2 {
		return "", runtime.NewError("two-player match required", 3)
	}
	type member struct {
		User   string `json:"user"`
		Ticket string `json:"ticket"`
	}
	members := make([]member, 0, 2)
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
		if !exactText(user, 256) || !exactText(ticket, 256) {
			return "", runtime.NewError("invalid match entry", 3)
		}
		members = append(members, member{user, ticket})
	}
	if members[0].User == members[1].User {
		return "", runtime.NewError("distinct players required", 3)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].User < members[j].User })
	key := stableKey("matchmaker_", []any{RoomVersion, b.config.Region, b.config.Compatibility, members})
	_, err := b.backend.Reserve(ctx, key, []string{members[0].User, members[1].User})
	// Nakama emits its ordinary matched signal. Clients recover via CurrentRPC,
	// including if that signal or this HTTP response is lost after commit.
	return "", playerError(err)
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
	case CurrentRPC:
		var req profileRequest
		if err = decodePlayer(payload, &req); err != nil {
			return "", err
		}
		if err = b.profile(req.Version, req.Compatibility, req.Region); err != nil {
			return "", err
		}
		result, err = b.backend.Current(ctx, user)
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
