package gamefleet

import (
	"context"
	"errors"

	"github.com/heroiclabs/nakama-common/runtime"
)

// serviceBackend has one machine service and no ordinary caller credential.
// New matching uses its mapped searches. Lifecycle operations follow an
// explicitly authorized original source through the History client.
type serviceBackend struct {
	search  *ServiceSearchClient
	history *ServiceHistoryClient
	routed  *RoutedServiceSearchClient
}

var _ Backend = (*serviceBackend)(nil)

func newServiceBackend(search *ServiceSearchClient, history *ServiceHistoryClient, routed ...*RoutedServiceSearchClient) (*serviceBackend, error) {
	if search == nil || history == nil || history.transport == nil || search.config != history.transport.config {
		return nil, errors.New("GameFleet service clients must use one exact scope and credential")
	}
	b := &serviceBackend{search: search, history: history}
	if len(routed) > 1 || (len(routed) == 1 && (routed[0] == nil || routed[0].transport == nil || routed[0].transport.config != search.config)) {
		return nil, errors.New("GameFleet routed client must use the same exact service scope and credential")
	}
	if len(routed) == 1 {
		b.routed = routed[0]
	}
	return b, nil
}

func servicePlayerError(err error) error {
	if err == nil {
		return nil
	}
	var history *ServiceHistoryError
	if errors.As(err, &history) {
		return &Error{Status: history.Status}
	}
	var search *ServiceSearchError
	if errors.As(err, &search) {
		return &Error{Status: search.Status}
	}
	return &Error{Status: 503}
}

func (b *serviceBackend) Current(ctx context.Context, user string) (CurrentResult, error) {
	out, err := b.history.Current(ctx, user)
	return out, servicePlayerError(err)
}

func (b *serviceBackend) Status(ctx context.Context, id, user string) (ReservationStatus, error) {
	out, err := b.history.Status(ctx, id, user)
	return out, servicePlayerError(err)
}

func (b *serviceBackend) BeginSearch(ctx context.Context, user, key string) (SearchResult, error) {
	if b.routed != nil {
		out, err := b.routed.BeginSearch(ctx, user, key)
		return SearchResult{Search: out.Search, Replay: out.Replay}, servicePlayerError(err)
	}
	out, err := b.search.BeginSearch(ctx, user, key)
	return out, servicePlayerError(err)
}

func (b *serviceBackend) SearchStatus(ctx context.Context, id, user string) (SearchStatus, error) {
	out, err := b.history.SearchStatus(ctx, id, user)
	return out, servicePlayerError(err)
}

func (b *serviceBackend) CancelSearch(ctx context.Context, id, user string) (SearchResult, error) {
	out, err := b.history.CancelSearch(ctx, id, user)
	return out, servicePlayerError(err)
}

func (b *serviceBackend) MatchSearches(ctx context.Context, key string, members []SearchMatchMember) (ReservationResult, error) {
	out, err := b.search.MatchSearches(ctx, key, members)
	return out, servicePlayerError(err)
}

func (b *serviceBackend) Issue(ctx context.Context, id, user, key string, previous int64, resume bool) (AssignmentResult, error) {
	out, err := b.history.Issue(ctx, id, user, key, previous, resume)
	return out, servicePlayerError(err)
}

func (b *serviceBackend) Cancel(ctx context.Context, id string) (ReservationResult, error) {
	// Backend's legacy signature lacks a participant argument. This adapter is
	// used only by Nakama; obtain the same authenticated runtime identity used
	// by the bridge, never an identity supplied in the player payload.
	user, err := authenticatedUser(ctx)
	if err != nil {
		return ReservationResult{}, &Error{Status: 403}
	}
	out, err := b.history.Cancel(ctx, id, user)
	return out, servicePlayerError(err)
}

func registerServiceBridge(initializer runtime.Initializer, backend Backend, search *ServiceSearchClient, routed ...*RoutedServiceSearchClient) error {
	if search == nil {
		return errors.New("GameFleet mapped search client is required")
	}
	b := bridge{
		backend: backend,
		config: Config{ApplicationID: search.config.ApplicationID, Region: search.config.Region,
			Compatibility: search.config.Compatibility},
		matchingSearchStatus: func(ctx context.Context, id, user string) (SearchStatus, error) {
			out, err := search.SearchStatus(ctx, id, user)
			return out, servicePlayerError(err)
		},
	}
	if len(routed) > 1 || (len(routed) == 1 && (routed[0] == nil || routed[0].transport == nil || routed[0].transport.config != search.config)) {
		return errors.New("GameFleet routed bridge requires the exact service scope and credential")
	}
	if len(routed) == 1 {
		b.matchingSearchStatus = nil
		b.matchingRoutedSearchStatus = func(ctx context.Context, id, user string) (RoutedSearchStatus, error) {
			out, err := routed[0].SearchStatus(ctx, id, user)
			return out, servicePlayerError(err)
		}
	}
	return registerBridge(initializer, b)
}
