package gamefleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"regexp"
)

const ServiceSearchRoutedVersion = "gamefleet.service-player-search.v2"

var routedMatchPoolID = regexp.MustCompile(`^gfsp_[0-9a-f]{64}$`)

// RoutedSearchResult is the private service API's routed search response. It
// is deliberately separate from the player-facing SearchResult DTO.
type RoutedSearchResult struct {
	Version     string `json:"version"`
	Search      Search `json:"search"`
	MatchPoolID string `json:"matchPoolId"`
	Replay      bool   `json:"replay"`
}

// RoutedSearchStatus is the private service API's routed status response.
type RoutedSearchStatus struct {
	Version     string `json:"version"`
	Search      Search `json:"search"`
	MatchPoolID string `json:"matchPoolId"`
}

// RoutedServiceSearchClient is an explicit v2-only companion to
// ServiceSearchClient. It shares the validated loopback transport and service
// identity without changing ServiceSearchConfig or the existing v1 client.
type RoutedServiceSearchClient struct {
	transport *ServiceSearchClient
}

func NewRoutedServiceSearchClient(cfg ServiceSearchConfig) (*RoutedServiceSearchClient, error) {
	transport, err := NewServiceSearchClient(cfg)
	if err != nil {
		return nil, err
	}
	if len(cfg.Region) > 64 || len(cfg.Compatibility) > 64 {
		transport.Close()
		return nil, errors.New("routed service region and compatibility are limited to 64 UTF-8 bytes")
	}
	return &RoutedServiceSearchClient{transport: transport}, nil
}

func (c *RoutedServiceSearchClient) Close() {
	if c != nil && c.transport != nil {
		c.transport.Close()
	}
}

func (c *RoutedServiceSearchClient) CheckScope(ctx context.Context) error {
	if c == nil || c.transport == nil {
		return serviceSearchError(422, "")
	}
	return c.transport.CheckScope(ctx)
}

func (c *RoutedServiceSearchClient) BeginSearch(ctx context.Context, participant, requestID string) (RoutedSearchResult, error) {
	var out RoutedSearchResult
	if c == nil || c.transport == nil || ctx == nil || !exactText(participant, 256) || !attemptID.MatchString(requestID) {
		return out, serviceSearchError(422, "")
	}
	body := struct {
		Version       string `json:"version"`
		ParticipantID string `json:"participantId"`
		RequestID     string `json:"requestId"`
		Region        string `json:"region"`
		Compatibility string `json:"compatibility"`
	}{ServiceSearchRoutedVersion, participant, requestID, c.transport.config.Region, c.transport.config.Compatibility}
	status, raw, err := c.exchange(ctx, "/business/v2/service-searches", body, http.StatusCreated, http.StatusOK)
	if err != nil {
		return RoutedSearchResult{}, err
	}
	if err = decodeRoutedEnvelope(raw, &out); err != nil || out.Version != ServiceSearchRoutedVersion ||
		!c.transport.validSearch(out.Search, out.Search.ID) ||
		(status == http.StatusCreated && out.Replay) || (status == http.StatusOK && !out.Replay) {
		return RoutedSearchResult{}, serviceSearchError(502, "")
	}
	return out, nil
}

func (c *RoutedServiceSearchClient) SearchStatus(ctx context.Context, searchID, participant string) (RoutedSearchStatus, error) {
	var out RoutedSearchStatus
	if c == nil || c.transport == nil || ctx == nil || !attemptID.MatchString(searchID) || !exactText(participant, 256) {
		return out, serviceSearchError(422, "")
	}
	body := struct {
		Version       string `json:"version"`
		ParticipantID string `json:"participantId"`
	}{ServiceSearchRoutedVersion, participant}
	_, raw, err := c.exchange(ctx, "/business/v2/service-searches/"+searchID+"/status", body, http.StatusOK)
	if err != nil {
		return RoutedSearchStatus{}, err
	}
	if err = decodeRoutedEnvelope(raw, &out); err != nil || out.Version != ServiceSearchRoutedVersion ||
		!c.transport.validSearch(out.Search, searchID) {
		return RoutedSearchStatus{}, serviceSearchError(502, "")
	}
	return out, nil
}

// exchange preserves the existing service transport's limits, authorization,
// redirect, timeout, and sanitized error behavior, while leaving successful
// JSON decoding to the case-sensitive v2 DTO decoder below.
func (c *RoutedServiceSearchClient) exchange(ctx context.Context, path string, body any, accepted ...int) (int, []byte, error) {
	if c == nil || c.transport == nil || c.transport.http == nil || ctx == nil {
		return 0, nil, serviceSearchError(422, "")
	}
	encoded, err := json.Marshal(body)
	if err != nil || len(encoded) > 16<<10 {
		return 0, nil, serviceSearchError(422, "")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.transport.base+path, bytes.NewReader(encoded))
	if err != nil {
		return 0, nil, serviceSearchError(503, "")
	}
	req.Header.Set("Authorization", "Bearer "+c.transport.config.Key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.transport.http.Do(req)
	if err != nil {
		return 0, nil, serviceSearchError(503, "")
	}
	defer resp.Body.Close()
	if !serviceSearchStatusExpected(resp.StatusCode, accepted) {
		code := serviceSearchErrorCode(resp)
		return resp.StatusCode, nil, serviceSearchError(resp.StatusCode, code)
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return resp.StatusCode, nil, serviceSearchError(502, "")
	}
	raw, ok := serviceSearchReadBounded(resp.Body)
	if !ok {
		return resp.StatusCode, nil, serviceSearchError(502, "")
	}
	return resp.StatusCode, raw, nil
}

func decodeRoutedEnvelope(raw []byte, out any) error {
	fields, err := routedExactObject(raw, []string{"data", "requestId"}, nil)
	if err != nil {
		return err
	}
	var requestID *string
	if err = json.Unmarshal(fields["requestId"], &requestID); err != nil || requestID == nil || !exactText(*requestID, 128) {
		return errors.New("invalid routed response request id")
	}
	if err = json.Unmarshal(fields["data"], out); err != nil {
		return errors.New("invalid routed response data")
	}
	return nil
}

func routedExactObject(raw []byte, required, optional []string) (map[string]json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' || !uniqueObjectFields(raw) {
		return nil, errors.New("expected unique JSON object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errors.New("invalid JSON object")
	}
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, name := range required {
		allowed[name] = true
		if _, ok := fields[name]; !ok {
			return nil, errors.New("missing required JSON field")
		}
	}
	for _, name := range optional {
		allowed[name] = true
	}
	for name := range fields {
		if !allowed[name] {
			return nil, errors.New("unknown or incorrectly cased JSON field")
		}
	}
	return fields, nil
}

func validateRoutedSearchCase(raw []byte) error {
	fields, err := routedExactObject(raw,
		[]string{"searchId", "state", "region", "compatibility", "createdAt", "expiresAt"},
		[]string{"resolvedAt", "allocationId", "reservation"})
	if err != nil {
		return err
	}
	if reservation, ok := fields["reservation"]; ok {
		reservation = bytes.TrimSpace(reservation)
		if len(reservation) == 0 || reservation[0] != '{' {
			return errors.New("invalid routed search reservation")
		}
		if _, err = routedExactObject(reservation, []string{
			"reservationId", "allocationId", "roomId", "applicationId", "placementId", "revisionId", "region", "state",
			"cancellationRequested", "createdAt", "updatedAt",
		}, []string{"failureCode"}); err != nil {
			return err
		}
	}
	return nil
}

func decodeRoutedSearch(raw []byte, out *Search) error {
	if err := validateRoutedSearchCase(raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return err
	}
	return nil
}

func (r *RoutedSearchResult) UnmarshalJSON(raw []byte) error {
	fields, err := routedExactObject(raw, []string{"version", "search", "matchPoolId", "replay"}, nil)
	if err != nil {
		return err
	}
	var version, pool *string
	if json.Unmarshal(fields["version"], &version) != nil || version == nil || *version != ServiceSearchRoutedVersion ||
		json.Unmarshal(fields["matchPoolId"], &pool) != nil || pool == nil || !routedMatchPoolID.MatchString(*pool) {
		return errors.New("invalid routed search response identity")
	}
	replayRaw := bytes.TrimSpace(fields["replay"])
	var replay bool
	switch {
	case bytes.Equal(replayRaw, []byte("true")):
		replay = true
	case bytes.Equal(replayRaw, []byte("false")):
		replay = false
	default:
		return errors.New("invalid routed search replay flag")
	}
	var search Search
	if err = decodeRoutedSearch(fields["search"], &search); err != nil {
		return err
	}
	*r = RoutedSearchResult{Version: *version, Search: search, MatchPoolID: *pool, Replay: replay}
	return nil
}

func (r *RoutedSearchStatus) UnmarshalJSON(raw []byte) error {
	fields, err := routedExactObject(raw, []string{"version", "search", "matchPoolId"}, nil)
	if err != nil {
		return err
	}
	var version, pool *string
	if json.Unmarshal(fields["version"], &version) != nil || version == nil || *version != ServiceSearchRoutedVersion ||
		json.Unmarshal(fields["matchPoolId"], &pool) != nil || pool == nil || !routedMatchPoolID.MatchString(*pool) {
		return errors.New("invalid routed search status identity")
	}
	var search Search
	if err = decodeRoutedSearch(fields["search"], &search); err != nil {
		return err
	}
	*r = RoutedSearchStatus{Version: *version, Search: search, MatchPoolID: *pool}
	return nil
}
