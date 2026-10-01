package gamefleet

import (
	"context"

	"github.com/heroiclabs/nakama-common/rtapi"
	"github.com/heroiclabs/nakama-common/runtime"
	"google.golang.org/protobuf/proto"
)

const (
	routedPoolProperty  = "gamefleet_match_pool"
	routedQueueProperty = "gamefleet_queue_protocol"
	routedQueueProtocol = "gamefleet-service-search-v2"
)

func (b bridge) routedMatchingStatus(ctx context.Context, id, user string) (RoutedSearchStatus, error) {
	out, err := b.matchingRoutedSearchStatus(ctx, id, user)
	if err != nil {
		return RoutedSearchStatus{}, playerSearchError(err)
	}
	if out.Version != ServiceSearchRoutedVersion || !routedMatchPoolID.MatchString(out.MatchPoolID) ||
		out.Search.ID != id || out.Search.Region != b.config.Region || out.Search.Compatibility != b.config.Compatibility {
		return RoutedSearchStatus{}, runtime.NewError("search pool unavailable", 9)
	}
	return out, nil
}

func (b bridge) beforeRouted(ctx context.Context, in *rtapi.Envelope, id, user string) (*rtapi.Envelope, error) {
	out, err := b.routedMatchingStatus(ctx, id, user)
	if err != nil {
		return nil, err
	}
	if out.Search.State != "pending" {
		return nil, runtime.NewError("search unavailable", 9)
	}
	// Clone only after authority has been checked. Numeric properties are
	// merged by Nakama too, so remove shadows of every routing field before
	// writing the server-authoritative string values.
	response := proto.Clone(in).(*rtapi.Envelope)
	r := response.GetMatchmakerAdd()
	for _, name := range []string{"gamefleet_protocol", "build_hash", "region", "gamefleet_search_id", routedPoolProperty, routedQueueProperty} {
		delete(r.NumericProperties, name)
	}
	r.StringProperties["gamefleet_protocol"] = RoomVersion
	r.StringProperties["build_hash"] = b.config.Compatibility
	r.StringProperties["region"] = b.config.Region
	r.StringProperties["gamefleet_search_id"] = id
	r.StringProperties[routedPoolProperty] = out.MatchPoolID
	r.StringProperties[routedQueueProperty] = routedQueueProtocol
	// Both clauses are MUST terms. Pool is restricted to a fixed ASCII
	// alphabet and the queue version is a constant; no client query is used.
	r.Query = "+properties." + routedPoolProperty + ":" + out.MatchPoolID +
		" +properties." + routedQueueProperty + ":" + routedQueueProtocol
	return response, nil
}

func (b bridge) checkRoutedMatch(ctx context.Context, entries []runtime.MatchmakerEntry, members []SearchMatchMember) error {
	// Validate both entry shapes first. An old-format or spoofed callback
	// cannot perform even a status lookup under the new queue protocol.
	pools := make([]string, len(entries))
	for i, entry := range entries {
		pool, _ := entry.GetProperties()[routedPoolProperty].(string)
		queue, _ := entry.GetProperties()[routedQueueProperty].(string)
		if !routedMatchPoolID.MatchString(pool) || queue != routedQueueProtocol {
			return runtime.NewError("search pool unavailable", 9)
		}
		pools[i] = pool
	}
	if pools[0] != pools[1] {
		return runtime.NewError("search pool unavailable", 9)
	}
	searches := make([]Search, len(members))
	for i, member := range members {
		out, err := b.routedMatchingStatus(ctx, member.SearchID, member.ParticipantID)
		if err != nil {
			return err
		}
		if out.MatchPoolID != pools[i] {
			return runtime.NewError("search pool unavailable", 9)
		}
		searches[i] = out.Search
	}
	if searches[0].State == "pending" && searches[1].State == "pending" {
		return nil
	}
	// A lost post-commit callback response may be retried with its exact
	// pair. Only the platform's original Match receipt can authorize that
	// replay; this check alone grants no new allocation or match authority.
	if searches[0].State == "bound" && searches[1].State == "bound" &&
		opaqueID.MatchString(searches[0].AllocationID) && searches[0].AllocationID == searches[1].AllocationID {
		return nil
	}
	return runtime.NewError("search unavailable", 9)
}
