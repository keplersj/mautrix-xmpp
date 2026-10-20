package xmppx

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"mellium.im/xmpp/jid"
)

// MUC status codes (XEP-0045 §15.6).
const (
	MUCStatusNonAnonymous = 100
	MUCStatusSelf         = 110
	MUCStatusNickChanged  = 303
	MUCStatusNickAssigned = 210
)

type joinState struct {
	mu      sync.Mutex
	waiters map[string]chan *Presence
}

// JoinResult describes a successful MUC join.
type JoinResult struct {
	// Occupant is our own occupant JID (room@service/nick), which may differ
	// from the requested nick if the room changed it.
	Occupant jid.JID
	// SelfPresence is the room's presence for us.
	SelfPresence *Presence
}

// MUCJoinError is returned when the room refuses the join.
type MUCJoinError struct {
	Err *StanzaError
}

func (e *MUCJoinError) Error() string { return "failed to join room: " + e.Err.Error() }

// matchJoin is called from the read loop for every presence. It resolves a
// pending join if this is our self-presence or a join error.
func (s *Session) matchJoin(p *Presence) {
	from, err := jid.Parse(p.From)
	if err != nil {
		return
	}
	room := from.Bare().String()
	s.joins.mu.Lock()
	ch, ok := s.joins.waiters[room]
	s.joins.mu.Unlock()
	if !ok {
		return
	}
	if p.Type == "error" || (p.Type != "unavailable" && p.MUCUser.HasStatus(MUCStatusSelf)) {
		select {
		case ch <- p:
		default:
		}
	}
}

// JoinMUC joins a room with the given nick and waits for the room to confirm.
// History is not requested (maxstanzas=0); use MAM for history.
func (s *Session) JoinMUC(ctx context.Context, room jid.JID, nick, password string) (*JoinResult, error) {
	occupant, err := room.Bare().WithResource(nick)
	if err != nil {
		return nil, fmt.Errorf("invalid nick %q: %w", nick, err)
	}
	ch := make(chan *Presence, 1)
	key := room.Bare().String()
	s.joins.mu.Lock()
	if _, busy := s.joins.waiters[key]; busy {
		s.joins.mu.Unlock()
		return nil, errors.New("join already in progress")
	}
	s.joins.waiters[key] = ch
	s.joins.mu.Unlock()
	defer func() {
		s.joins.mu.Lock()
		delete(s.joins.waiters, key)
		s.joins.mu.Unlock()
	}()

	zero := 0
	err = s.SendPresence(ctx, &Presence{
		To:  occupant.String(),
		MUC: &MUCJoin{Password: password, History: &MUCHistory{MaxStanzas: &zero}},
	})
	if err != nil {
		return nil, err
	}
	select {
	case p := <-ch:
		if p.Type == "error" {
			if p.Error == nil {
				p.Error = &StanzaError{}
			}
			return nil, &MUCJoinError{Err: p.Error}
		}
		from, _ := jid.Parse(p.From)
		return &JoinResult{Occupant: from, SelfPresence: p}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, s.err
	}
}

// LeaveMUC sends unavailable presence to the room.
func (s *Session) LeaveMUC(ctx context.Context, occupant jid.JID) error {
	return s.SendPresence(ctx, &Presence{To: occupant.String(), Type: "unavailable"})
}
