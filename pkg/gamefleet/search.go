package gamefleet

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Search is durable queue correlation, not a game admission credential. Bound
// searches read lifecycle state from the original room reservation ledger.
type Search struct {
	ID            string       `json:"searchId"`
	State         string       `json:"state"`
	Region        string       `json:"region"`
	Compatibility string       `json:"compatibility"`
	CreatedAt     time.Time    `json:"createdAt"`
	ExpiresAt     time.Time    `json:"expiresAt"`
	ResolvedAt    *time.Time   `json:"resolvedAt,omitempty"`
	AllocationID  string       `json:"allocationId,omitempty"`
	Reservation   *Reservation `json:"reservation,omitempty"`
}

type SearchResult struct {
	Search Search `json:"search"`
	Replay bool   `json:"replay"`
}

type SearchStatus struct {
	Search Search `json:"search"`
}

type SearchMatchMember struct {
	ParticipantID string `json:"participantId"`
	SearchID      string `json:"searchId"`
	NakamaTicket  string `json:"nakamaTicket"`
}

func (s *Search) UnmarshalJSON(raw []byte) error {
	type plain Search
	var wire struct {
		*plain
		ResolvedAt   json.RawMessage `json:"resolvedAt"`
		AllocationID json.RawMessage `json:"allocationId"`
		Reservation  json.RawMessage `json:"reservation"`
	}
	*s = Search{}
	wire.plain = (*plain)(s)
	if err := strictObject(raw, &wire); err != nil {
		return err
	}
	if wire.ResolvedAt != nil {
		if err := json.Unmarshal(wire.ResolvedAt, &s.ResolvedAt); err != nil || s.ResolvedAt == nil {
			return errors.New("invalid resolvedAt")
		}
	}
	if wire.AllocationID != nil {
		var id *string
		if err := json.Unmarshal(wire.AllocationID, &id); err != nil || id == nil || *id == "" {
			return errors.New("invalid allocationId")
		}
		s.AllocationID = *id
	}
	if wire.Reservation != nil {
		if err := json.Unmarshal(wire.Reservation, &s.Reservation); err != nil || s.Reservation == nil {
			return errors.New("invalid reservation")
		}
	}
	return nil
}

func (s *SearchResult) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Search Search `json:"search"`
		Replay *bool  `json:"replay"`
	}
	if err := strictObject(raw, &wire); err != nil {
		return err
	}
	if wire.Replay == nil {
		return errors.New("missing replay")
	}
	s.Search, s.Replay = wire.Search, *wire.Replay
	return nil
}

func (c *Client) validSearch(s Search) bool {
	ttl := s.ExpiresAt.Sub(s.CreatedAt)
	if !attemptID.MatchString(s.ID) || s.Region != c.config.Region || s.Compatibility != c.config.Compatibility || s.CreatedAt.UnixMilli() <= 0 || ttl < 30*time.Second || ttl > 600*time.Second || ttl%time.Second != 0 {
		return false
	}
	noRoom := s.AllocationID == "" && s.Reservation == nil
	if s.State == "pending" {
		return s.ResolvedAt == nil && noRoom
	}
	if s.ResolvedAt == nil || s.ResolvedAt.Before(s.CreatedAt) {
		return false
	}
	switch s.State {
	case "cancelled":
		return s.ResolvedAt.Before(s.ExpiresAt) && noRoom
	case "expired":
		return !s.ResolvedAt.Before(s.ExpiresAt) && noRoom
	case "bound":
		return s.ResolvedAt.Before(s.ExpiresAt) && opaqueID.MatchString(s.AllocationID) && s.Reservation != nil && s.Reservation.AllocationID == s.AllocationID && s.Reservation.CreatedAt.Equal(*s.ResolvedAt) && c.validReservation(*s.Reservation)
	}
	return false
}

func (c *Client) BeginSearch(ctx context.Context, user, requestID string) (SearchResult, error) {
	var out SearchResult
	if !exactText(user, 256) || !attemptID.MatchString(requestID) {
		return out, &Error{Status: 422}
	}
	err := c.call(ctx, "POST", "/business/v1/searches", map[string]string{"version": SearchVersion, "participantId": user, "requestId": requestID, "compatibility": c.config.Compatibility}, &out, 200, 201)
	if err == nil && !c.validSearch(out.Search) {
		err = &Error{Status: 502}
	}
	return out, err
}

func (c *Client) SearchStatus(ctx context.Context, id, user string) (SearchStatus, error) {
	var out SearchStatus
	if !attemptID.MatchString(id) || !exactText(user, 256) {
		return out, &Error{Status: 422}
	}
	err := c.call(ctx, "POST", "/business/v1/searches/"+id+"/status", map[string]string{"version": SearchVersion, "participantId": user}, &out, 200)
	if err == nil && (!c.validSearch(out.Search) || out.Search.ID != id) {
		err = &Error{Status: 502}
	}
	return out, err
}

func (c *Client) CancelSearch(ctx context.Context, id, user string) (SearchResult, error) {
	var out SearchResult
	if !attemptID.MatchString(id) || !exactText(user, 256) {
		return out, &Error{Status: 422}
	}
	err := c.call(ctx, "POST", "/business/v1/searches/"+id+"/cancel", map[string]string{"version": SearchVersion, "participantId": user}, &out, 200)
	if err == nil && (!c.validSearch(out.Search) || out.Search.ID != id || out.Search.State == "pending") {
		err = &Error{Status: 502}
	}
	return out, err
}

func (c *Client) MatchSearches(ctx context.Context, key string, members []SearchMatchMember) (ReservationResult, error) {
	var out ReservationResult
	if !attemptID.MatchString(key) || len(members) != 2 {
		return out, &Error{Status: 422}
	}
	for _, m := range members {
		if !exactText(m.ParticipantID, 256) || !attemptID.MatchString(m.SearchID) || !exactText(m.NakamaTicket, 256) {
			return out, &Error{Status: 422}
		}
	}
	if members[0].ParticipantID == members[1].ParticipantID || members[0].SearchID == members[1].SearchID || members[0].NakamaTicket == members[1].NakamaTicket {
		return out, &Error{Status: 422}
	}
	body := struct {
		Version       string              `json:"version"`
		Key           string              `json:"idempotencyKey"`
		Compatibility string              `json:"compatibility"`
		Members       []SearchMatchMember `json:"members"`
	}{SearchVersion, key, c.config.Compatibility, append([]SearchMatchMember(nil), members...)}
	// A just-committed room can make the host's previous inventory temporarily
	// inconsistent until its next signed report. Nakama consumes the matched
	// queue entries even when the callback fails, so resolve that brief window
	// here with the exact same immutable match request. A committed replay can
	// only return its original allocation; no player/search/ticket is replaced.
	retryCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	delay := 100 * time.Millisecond
	for attempt := 0; attempt < 20; attempt++ {
		out = ReservationResult{}
		err := c.call(retryCtx, "POST", "/business/v1/searches/match", body, &out, 200, 202)
		if err == nil {
			if !c.validReservation(out.Reservation) {
				return ReservationResult{}, &Error{Status: 502}
			}
			return out, nil
		}
		var conflict *Error
		if !errors.As(err, &conflict) || (conflict.Status != 409 && conflict.Status != 429) || attempt == 19 || retryCtx.Err() != nil {
			return ReservationResult{}, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-retryCtx.Done():
			timer.Stop()
			return ReservationResult{}, err
		case <-timer.C:
		}
		delay = min(delay*2, 500*time.Millisecond)
	}
	return ReservationResult{}, &Error{Status: 503}
}
