package connector

import (
	"context"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/keplersj/mautrix-xmpp/pkg/xmppx"
)

// archiveQuery returns the MAM query base for a portal and whether the
// archive supports MAM at all.
func (c *XMPPClient) archiveQuery(portal *bridgev2.Portal) (xmppx.MAMQuery, bool, error) {
	j, isMUC, err := parsePortalID(portal.ID)
	if err != nil {
		return xmppx.MAMQuery{}, false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if isMUC {
		room := c.rooms[j.String()]
		return xmppx.MAMQuery{Archive: j.String()}, room != nil && room.mam, nil
	}
	return xmppx.MAMQuery{With: j.String()}, c.accountMAM, nil
}

// FetchMessages implements bridgev2.BackfillingNetworkAPI using MAM.
func (c *XMPPClient) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	if params.ThreadRoot != "" {
		return &bridgev2.FetchMessagesResponse{}, nil
	}
	s, err := c.session()
	if err != nil {
		return nil, err
	}
	q, ok, err := c.archiveQuery(params.Portal)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &bridgev2.FetchMessagesResponse{HasMore: false, Forward: params.Forward}, nil
	}
	count := params.Count
	if count <= 0 {
		count = c.main.Config.MAM.PageSize
	}
	pageSize := min(count, c.main.Config.MAM.PageSize)

	var archived []*xmppx.ArchivedMessage
	resp := &bridgev2.FetchMessagesResponse{Forward: params.Forward}
	switch {
	case params.Forward && params.AnchorMessage != nil:
		// Catch up on everything after the newest bridged message.
		q.Start = params.AnchorMessage.Timestamp
		for len(archived) < count {
			q.Max = pageSize
			page, err := s.QueryMAM(ctx, q)
			if err != nil {
				return nil, err
			}
			archived = append(archived, page.Messages...)
			if page.Complete || page.Last == "" || len(page.Messages) == 0 {
				break
			}
			q.After = page.Last
		}
	default:
		// Newest page (initial backfill) or the page before the cursor or
		// the oldest bridged message (backwards backfill).
		q.BeforeLast = true
		if params.Cursor != "" {
			q.Before, q.BeforeLast = string(params.Cursor), false
		} else if !params.Forward && params.AnchorMessage != nil {
			q.End = params.AnchorMessage.Timestamp
		}
		q.Max = count
		page, err := s.QueryMAM(ctx, q)
		if err != nil {
			return nil, err
		}
		archived = page.Messages
		resp.HasMore = !page.Complete && len(page.Messages) > 0
		resp.Cursor = networkid.PaginationCursor(page.First)
	}
	resp.Messages = c.convertArchive(ctx, params.Portal, archived)
	resp.MarkRead = false
	return resp, nil
}

// convertArchive converts a page of archived messages for backfill. Edits,
// retractions and reactions that target messages in the same page are folded
// into those messages; others are dropped (they can't be inserted into
// history on their own).
func (c *XMPPClient) convertArchive(ctx context.Context, portal *bridgev2.Portal, archived []*xmppx.ArchivedMessage) []*bridgev2.BackfillMessage {
	log := zerolog.Ctx(ctx)
	type entry struct {
		in        *incoming
		reactions map[networkid.UserID]*incoming
	}
	byID := make(map[networkid.MessageID]*entry)
	byArchive := make(map[string]*entry)
	var order []*entry

	lookup := func(in *incoming, ref string) *entry {
		if e := byID[makeMessageID(in.portal.ID, ref)]; e != nil {
			return e
		}
		return byArchive[ref]
	}

	for _, am := range archived {
		in := c.resolveIncoming(log, am.Message, "", am)
		if in == nil || in.portal != portal.PortalKey {
			continue
		}
		msg := in.msg
		switch {
		case msg.Retract != nil || (msg.ApplyTo != nil && msg.ApplyTo.Retract != nil):
			ref := ""
			if msg.Retract != nil {
				ref = msg.Retract.ID
			} else {
				ref = msg.ApplyTo.ID
			}
			if e := lookup(in, ref); e != nil && c.sameSender(e.in, in) {
				e.in = nil
			}
		case msg.Replace != nil && msg.Body != "":
			if e := lookup(in, msg.Replace.ID); e != nil && e.in != nil && c.sameSender(e.in, in) {
				edited := *e.in
				copied := *e.in.msg
				copied.Body, copied.Fallbacks, copied.OOB = msg.Body, msg.Fallbacks, msg.OOB
				edited.msg = &copied
				e.in = &edited
			}
		case msg.Reactions != nil:
			if e := lookup(in, msg.Reactions.ID); e != nil && in.sender.Sender != "" {
				e.reactions[in.sender.Sender] = in
			}
		case msg.Retracted != nil:
			// Tombstone of a retracted message.
		case msg.Body != "" || isEncrypted(msg):
			e := &entry{in: in, reactions: make(map[networkid.UserID]*incoming)}
			byID[in.messageID()] = e
			if in.archiveID != "" {
				byArchive[in.archiveID] = e
			}
			order = append(order, e)
		}
	}

	out := make([]*bridgev2.BackfillMessage, 0, len(order))
	for _, e := range order {
		if e.in == nil {
			continue
		}
		in := e.in
		cm, err := c.convertMessage(ctx, portal, c.main.br.Bot, in)
		if err != nil {
			log.Debug().Err(err).Str("xmpp_id", in.id).Msg("Skipping archived message")
			continue
		}
		bm := &bridgev2.BackfillMessage{
			ConvertedMessage: cm,
			Sender:           in.sender,
			ID:               in.messageID(),
			TxnID:            networkid.TransactionID(in.id),
			Timestamp:        in.ts,
		}
		for _, r := range e.reactions {
			bm.Reactions = append(bm.Reactions, reactionsFor(r.sender, r.msg.Reactions.Reactions, r.ts)...)
		}
		if in.isMUC && in.archiveID != "" {
			if err := c.aliases.Put(ctx, c.login.ID, in.portal.ID, in.archiveID, bm.ID); err != nil {
				log.Warn().Err(err).Msg("Failed to store message alias")
			}
		}
		out = append(out, bm)
	}
	return out
}

func (c *XMPPClient) sameSender(a, b *incoming) bool {
	if a.sender.IsFromMe || b.sender.IsFromMe {
		return a.sender.IsFromMe == b.sender.IsFromMe
	}
	return a.sender.Sender == b.sender.Sender
}

// catchUp fetches messages missed while disconnected from the account archive
// and every joined room's archive, and handles them like live messages.
func (c *XMPPClient) catchUp(ctx context.Context, s *xmppx.Session, lastSeen time.Time) {
	log := zerolog.Ctx(ctx).With().Str("action", "mam catch-up").Logger()
	ctx = log.WithContext(ctx)
	c.mu.Lock()
	accountMAM := c.accountMAM
	var rooms []string
	for key, room := range c.rooms {
		if room.joined && room.mam {
			rooms = append(rooms, key)
		}
	}
	c.mu.Unlock()
	if lastSeen.IsZero() {
		// Fresh login: history comes from per-portal backfill instead.
		c.noteSeen(time.Now())
		return
	}
	// Allow for clock skew between us and the server. New messages are
	// deduplicated by ID; edits, retractions and reactions have no stable ID
	// of their own, so those older than lastSeen are assumed handled.
	since := lastSeen.Add(-1 * time.Minute)
	if accountMAM {
		c.catchUpArchive(ctx, s, xmppx.MAMQuery{Start: since}, lastSeen)
	}
	for _, room := range rooms {
		c.catchUpArchive(ctx, s, xmppx.MAMQuery{Archive: room, Start: since}, lastSeen)
	}
}

func (c *XMPPClient) catchUpArchive(ctx context.Context, s *xmppx.Session, q xmppx.MAMQuery, lastSeen time.Time) {
	log := zerolog.Ctx(ctx).With().Str("archive", q.Archive).Logger()
	total := 0
	limit := c.main.Config.MAM.CatchupMax
	for total < limit {
		q.Max = min(c.main.Config.MAM.PageSize, limit-total)
		page, err := s.QueryMAM(ctx, q)
		if err != nil {
			log.Warn().Err(err).Msg("Failed to query archive for catch-up")
			return
		}
		for _, am := range page.Messages {
			in := c.resolveIncoming(&log, am.Message, "", am)
			if in == nil {
				continue
			}
			m := in.msg
			isUpdate := m.Replace != nil || m.Retract != nil || m.ApplyTo != nil || m.Reactions != nil
			if isUpdate && !in.ts.After(lastSeen) {
				continue
			}
			c.dispatch(ctx, in)
		}
		total += len(page.Messages)
		if page.Complete || page.Last == "" || len(page.Messages) == 0 {
			break
		}
		q.After = page.Last
	}
	if total > 0 {
		log.Info().Int("count", total).Msg("Caught up on archived messages")
	}
}
