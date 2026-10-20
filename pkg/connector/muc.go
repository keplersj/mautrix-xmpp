package connector

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
	"mellium.im/xmpp/jid"

	"github.com/keplersj/mautrix-xmpp/pkg/xmppx"
)

// mucRoom is the in-memory state of a joined group chat.
type mucRoom struct {
	jid      jid.JID
	occupant jid.JID // our own occupant JID
	name     string
	password string
	subject  *string
	mam      bool
	joined   bool

	occupants map[string]*mucOccupant // by nick
}

type mucOccupant struct {
	nick        string
	realJID     jid.JID // zero if the room hides it
	affiliation string
	role        string
}

func (r *mucRoom) nick() string { return r.occupant.Resourcepart() }

func (c *XMPPClient) getRoom(room jid.JID) *mucRoom {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rooms[room.Bare().String()]
}

// occupantSender returns the event sender for a message from an occupant JID.
// realHint is a real JID carried in the message itself (MAM results from
// non-anonymous rooms include it).
func (c *XMPPClient) occupantSender(occupant jid.JID, realHint string) (bridgev2.EventSender, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.rooms[occupant.Bare().String()]
	if room == nil {
		room = &mucRoom{}
	}
	return c.occupantSenderLocked(room, occupant, realHint)
}

func (c *XMPPClient) selfSender() bridgev2.EventSender {
	return bridgev2.EventSender{IsFromMe: true, SenderLogin: c.login.ID, Sender: makeUserID(c.account)}
}

// joinBookmarkedRooms joins every autojoin bookmark and makes sure each has a
// portal.
func (c *XMPPClient) joinBookmarkedRooms(ctx context.Context, s *xmppx.Session) {
	log := zerolog.Ctx(ctx)
	bookmarks, err := s.Bookmarks(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch bookmarks")
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, bm := range bookmarks {
		if !bm.Autojoin {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(bm xmppx.Bookmark) {
			defer wg.Done()
			defer func() { <-sem }()
			c.joinRoom(ctx, s, bm)
		}(bm)
	}
	wg.Wait()
}

func (c *XMPPClient) defaultNick() string {
	if n := c.account.Localpart(); n != "" {
		return n
	}
	return "mautrix-xmpp"
}

func (c *XMPPClient) joinRoom(ctx context.Context, s *xmppx.Session, bm xmppx.Bookmark) {
	log := zerolog.Ctx(ctx).With().Stringer("room", bm.JID).Logger()
	nick := bm.Nick
	if nick == "" {
		nick = c.defaultNick()
	}
	room := &mucRoom{jid: bm.JID.Bare(), name: bm.Name, password: bm.Password, occupants: make(map[string]*mucOccupant)}
	if occ, err := bm.JID.WithResource(nick); err == nil {
		room.occupant = occ
	}
	// Register the room before joining so the occupant presences that arrive
	// during the join are tracked.
	c.mu.Lock()
	if old, ok := c.rooms[room.jid.String()]; ok && old.subject != nil {
		room.subject = old.subject
	}
	c.rooms[room.jid.String()] = room
	c.mu.Unlock()

	info, err := s.DiscoInfo(ctx, room.jid.String())
	if err != nil {
		log.Warn().Err(err).Msg("Failed to query room features")
	}
	if room.name == "" {
		room.name = info.IdentityName("conference")
	}
	joinCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var res *xmppx.JoinResult
	for attempt := 0; attempt < 3; attempt++ {
		res, err = s.JoinMUC(joinCtx, room.jid, nick, bm.Password)
		var jerr *xmppx.MUCJoinError
		if errors.As(err, &jerr) && jerr.Err.Condition.Local == "conflict" {
			nick += "_"
			continue
		}
		break
	}
	if err != nil {
		log.Warn().Err(err).Msg("Failed to join room")
		c.mu.Lock()
		delete(c.rooms, room.jid.String())
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	room.occupant = res.Occupant
	room.joined = true
	room.mam = info.HasFeature(xmppx.NSMAM)
	c.mu.Unlock()
	log.Debug().Str("nick", res.Occupant.Resourcepart()).Bool("mam", room.mam).Msg("Joined room")

	// Create the portal (or refresh its info) even if nobody talks.
	key := makeMUCPortalKey(c.login.ID, room.jid)
	c.queueRemoteEvent(&simplevent.ChatResync{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventChatResync,
			PortalKey:    key,
			CreatePortal: true,
			LogContext: func(lc zerolog.Context) zerolog.Context {
				return lc.Stringer("room", room.jid)
			},
		},
		GetChatInfoFunc: c.GetChatInfo,
	})
	c.updatePortalNick(ctx, key, res.Occupant.Resourcepart(), bm.Password)
}

func (c *XMPPClient) updatePortalNick(ctx context.Context, key networkid.PortalKey, nick, password string) {
	portal, err := c.main.br.GetExistingPortalByKey(ctx, key)
	if err != nil || portal == nil {
		return
	}
	meta := portal.Metadata.(*PortalMetadata)
	if meta.Nick == nick && meta.Password == password {
		return
	}
	meta.Nick, meta.Password = nick, password
	if err = portal.Save(ctx); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to save portal metadata")
	}
}

// HandlePresence tracks MUC occupants. Contact presence is not bridged in
// v0.1.0.
func (c *XMPPClient) HandlePresence(s *xmppx.Session, p *xmppx.Presence) {
	from, err := jid.Parse(p.From)
	if err != nil || from.Resourcepart() == "" || p.MUCUser == nil {
		return
	}
	c.mu.Lock()
	room := c.rooms[from.Bare().String()]
	if room == nil {
		c.mu.Unlock()
		return
	}
	nick := from.Resourcepart()
	var item xmppx.MUCItem
	if len(p.MUCUser.Items) > 0 {
		item = p.MUCUser.Items[0]
	}
	var change *bridgev2.ChatMember
	isSelf := p.MUCUser.HasStatus(xmppx.MUCStatusSelf)
	switch p.Type {
	case "unavailable":
		delete(room.occupants, nick)
		if p.MUCUser.HasStatus(xmppx.MUCStatusNickChanged) && item.Nick != "" {
			// The new nick's presence follows; nothing to bridge yet.
			if isSelf {
				if occ, err := room.jid.WithResource(item.Nick); err == nil {
					room.occupant = occ
				}
			}
			break
		}
		if isSelf {
			room.joined = false
			break
		}
		if room.joined {
			sender, _ := c.occupantSenderLocked(room, from, item.JID)
			change = &bridgev2.ChatMember{EventSender: sender, Membership: event.MembershipLeave}
		}
	case "", "available":
		occ := &mucOccupant{nick: nick, affiliation: item.Affiliation, role: item.Role}
		if item.JID != "" {
			if real, err := parseBareJID(item.JID); err == nil {
				occ.realJID = real
			}
		}
		_, existed := room.occupants[nick]
		room.occupants[nick] = occ
		if isSelf {
			room.occupant = from
		} else if !existed && room.joined {
			sender, _ := c.occupantSenderLocked(room, from, item.JID)
			name := nick
			change = &bridgev2.ChatMember{EventSender: sender, Membership: event.MembershipJoin, Nickname: &name}
		}
	}
	c.mu.Unlock()
	if change == nil || change.IsFromMe {
		return
	}
	members := &bridgev2.ChatMemberList{MemberMap: bridgev2.ChatMemberMap{}}
	members.MemberMap.Set(*change)
	c.queueRemoteEvent(&simplevent.ChatInfoChange{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventChatInfoChange,
			PortalKey: makeMUCPortalKey(c.login.ID, room.jid),
			Sender:    change.EventSender,
			LogContext: func(lc zerolog.Context) zerolog.Context {
				return lc.Stringer("room", room.jid).Str("membership", string(change.Membership))
			},
		},
		ChatInfoChange: &bridgev2.ChatInfoChange{MemberChanges: members},
	})
}

// occupantSenderLocked is occupantSender for callers holding c.mu.
func (c *XMPPClient) occupantSenderLocked(room *mucRoom, occupant jid.JID, realHint string) (bridgev2.EventSender, bool) {
	var real jid.JID
	if occ := room.occupants[occupant.Resourcepart()]; occ != nil {
		real = occ.realJID
	}
	if realHint != "" {
		if j, err := parseBareJID(realHint); err == nil {
			real = j
		}
	}
	if !real.Equal(jid.JID{}) {
		if real.Equal(c.account) {
			return c.selfSender(), true
		}
		return bridgev2.EventSender{Sender: makeUserID(real)}, false
	}
	if occupant.Resourcepart() == room.nick() {
		return c.selfSender(), true
	}
	return bridgev2.EventSender{Sender: makeOccupantUserID(occupant)}, false
}
