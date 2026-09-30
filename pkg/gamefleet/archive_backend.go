package gamefleet

import (
	"context"
	"errors"
)

// TerminalArchiveReader has no current, ticket, cancel or new-work methods.
// It is configured independently from the active deployment's business key.
type TerminalArchiveReader interface {
	Status(context.Context, string, string) (ReservationStatus, error)
	SearchStatus(context.Context, string, string) (SearchStatus, error)
}

type terminalArchiveBackend struct {
	Backend
	archive      TerminalArchiveReader
	archiveScope TerminalArchiveConfig
}

// archiveProfileReader is deliberately private and exposes only direct reads
// for a profile that does not match the active deployment. Normal-profile
// reads continue through Backend first and retain its narrow error fallback.
type archiveProfileReader interface {
	archiveRoomProfile(version, compatibility, region string) bool
	archiveSearchProfile(version, compatibility, region string) bool
	archiveStatus(context.Context, string, string) (ReservationStatus, error)
	archiveSearchStatus(context.Context, string, string) (SearchStatus, error)
}

func (b *terminalArchiveBackend) archiveRoomProfile(version, compatibility, region string) bool {
	return b != nil && b.archive != nil && version == RoomVersion &&
		exactText(b.archiveScope.Compatibility, 128) && exactText(b.archiveScope.Region, 128) &&
		compatibility == b.archiveScope.Compatibility && region == b.archiveScope.Region
}

func (b *terminalArchiveBackend) archiveSearchProfile(version, compatibility, region string) bool {
	return b != nil && b.archive != nil && version == SearchVersion &&
		exactText(b.archiveScope.Compatibility, 128) && exactText(b.archiveScope.Region, 128) &&
		compatibility == b.archiveScope.Compatibility && region == b.archiveScope.Region
}

func (b *terminalArchiveBackend) archiveStatus(ctx context.Context, id, user string) (ReservationStatus, error) {
	if b == nil || b.archive == nil || ctx == nil || !opaqueID.MatchString(id) || !exactText(user, 256) {
		return ReservationStatus{}, &Error{Status: 422}
	}
	return b.archive.Status(ctx, id, user)
}

func (b *terminalArchiveBackend) archiveSearchStatus(ctx context.Context, id, user string) (SearchStatus, error) {
	if b == nil || b.archive == nil || ctx == nil || !attemptID.MatchString(id) || !exactText(user, 256) {
		return SearchStatus{}, &Error{Status: 422}
	}
	return b.archive.SearchStatus(ctx, id, user)
}

func archiveFallbackAllowed(err error) bool {
	var upstream *Error
	return errors.As(err, &upstream) && (upstream.Status == 401 || upstream.Status == 403 || upstream.Status == 404)
}

func (b *terminalArchiveBackend) Status(ctx context.Context, id, user string) (ReservationStatus, error) {
	if !opaqueID.MatchString(id) || !exactText(user, 256) {
		return ReservationStatus{}, &Error{Status: 422}
	}
	out, err := b.Backend.Status(ctx, id, user)
	if err == nil || !archiveFallbackAllowed(err) {
		return out, err
	}
	// The same exact pointer and authenticated identity are passed through.
	// A failed archive read stays a failure, never an empty current result.
	return b.archive.Status(ctx, id, user)
}

func (b *terminalArchiveBackend) SearchStatus(ctx context.Context, id, user string) (SearchStatus, error) {
	if !attemptID.MatchString(id) || !exactText(user, 256) {
		return SearchStatus{}, &Error{Status: 422}
	}
	out, err := b.Backend.SearchStatus(ctx, id, user)
	if err == nil || !archiveFallbackAllowed(err) {
		return out, err
	}
	return b.archive.SearchStatus(ctx, id, user)
}
