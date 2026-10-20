package connector

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"mellium.im/xmpp/jid"

	"github.com/keplersj/mautrix-xmpp/pkg/xmppx"
)

// incoming is one XMPP message resolved to bridge terms.
type incoming struct {
	msg       *xmppx.Message
	portal    networkid.PortalKey
	chat      jid.JID
	isMUC     bool
	sender    bridgev2.EventSender
	senderJID string
	ts        time.Time
	id        string // sender-assigned XMPP ID used for the bridged message ID
	archiveID string
	archived  bool // came from MAM rather than live
}

func (in *incoming) messageID() networkid.MessageID {
	return makeMessageID(in.portal.ID, in.id)
}

// resolveIncoming works out which chat a message belongs to and who sent it.
// carbon is "sent"/"received" for XEP-0280 copies. archive is the archive JID
// and result for MAM results. It returns nil for messages that should not be
// bridged.
func (c *XMPPClient) resolveIncoming(log *zerolog.Logger, msg *xmppx.Message, carbon string, archive *xmppx.ArchivedMessage) *incoming {
	in := &incoming{msg: msg, ts: time.Now()}
	if msg.Delay != nil && !msg.Delay.Stamp.IsZero() {
		in.ts = msg.Delay.Stamp
	}
	if archive != nil {
		in.archived = true
		in.archiveID = archive.ArchiveID
		if !archive.Timestamp.IsZero() {
			in.ts = archive.Timestamp
		}
	}
	from, err := jid.Parse(msg.From)
	if msg.From == "" {
		from, err = c.account, nil
	}
	if err != nil {
		log.Debug().Str("from", msg.From).Msg("Dropping message with invalid sender")
		return nil
	}

	if msg.Type == xmppx.MessageGroupchat {
		room := from.Bare()
		if c.getRoom(room) == nil && archive == nil {
			log.Debug().Stringer("room", room).Msg("Dropping groupchat message for a room we're not in")
			return nil
		}
		in.chat, in.isMUC = room, true
		in.portal = makeMUCPortalKey(c.login.ID, room)
		if in.archiveID == "" {
			in.archiveID = archiveIDBy(msg, room.String())
		}
		if from.Resourcepart() == "" {
			// A message from the room itself (subject changes, notices).
			in.sender = bridgev2.EventSender{}
		} else {
			var realHint string
			if msg.MUCUser != nil && len(msg.MUCUser.Items) > 0 {
				realHint = msg.MUCUser.Items[0].JID
			}
			in.sender, _ = c.occupantSender(from, realHint)
			in.senderJID = from.String()
		}
	} else {
		var partner jid.JID
		toMe := true
		if carbon == "sent" || from.Bare().Equal(c.account) {
			to, err := jid.Parse(msg.To)
			if err != nil || msg.To == "" {
				return nil
			}
			partner, toMe = to.Bare(), false
		} else {
			partner = from.Bare()
		}
		if partner.Localpart() == "" {
			// Server announcements and transports aren't chats.
			log.Debug().Stringer("from", from).Msg("Dropping message from a JID without a localpart")
			return nil
		}
		if c.getRoom(partner) != nil {
			log.Debug().Stringer("from", from).Msg("Dropping MUC private message (not supported yet)")
			return nil
		}
		in.chat = partner
		in.portal = makeDMPortalKey(c.login.ID, partner)
		if toMe {
			in.sender = bridgev2.EventSender{Sender: makeUserID(partner)}
			in.senderJID = partner.String()
		} else {
			in.sender = c.selfSender()
			in.senderJID = c.account.String()
		}
		if in.archiveID == "" {
			in.archiveID = archiveIDBy(msg, c.account.String())
		}
	}

	in.id = xmppID(msg)
	if in.id == "" {
		if in.archiveID != "" {
			in.id = "archive-" + in.archiveID
		} else {
			in.id = syntheticMessageID(msg.From, in.ts, msg.Body)
		}
	}
	return in
}

// HandleMessage handles a live message from the server.
func (c *XMPPClient) HandleMessage(s *xmppx.Session, msg *xmppx.Message) {
	log := c.log.With().Str("action", "handle xmpp message").Str("stanza_id", msg.ID).Logger()
	ctx := log.WithContext(context.Background())

	if msg.Type == xmppx.MessageError {
		log.Warn().Str("from", msg.From).Str("error", msg.Error.Error()).Msg("Received message error")
		return
	}
	carbon := ""
	if msg.CarbonSent != nil || msg.CarbonReceived != nil {
		// XEP-0280 §11: carbons must come from our own bare JID.
		if msg.From != "" {
			if from, err := jid.Parse(msg.From); err != nil || !from.Equal(c.account) {
				log.Warn().Str("from", msg.From).Msg("Dropping spoofed carbon")
				return
			}
		}
		fwd := msg.CarbonReceived
		carbon = "received"
		if msg.CarbonSent != nil {
			fwd, carbon = msg.CarbonSent, "sent"
		}
		if fwd.Forwarded.Message == nil {
			return
		}
		msg = fwd.Forwarded.Message
	}
	if msg.MAMResult != nil {
		// Results of queries we didn't make; ours are consumed by the session.
		return
	}
	in := c.resolveIncoming(&log, msg, carbon, nil)
	if in == nil {
		return
	}
	c.dispatch(ctx, in)

	// Answer delivery receipt requests in 1:1 chats (XEP-0184 §5.4): the
	// message has reached the user's Matrix account.
	if !in.isMUC && msg.ReceiptRequest != nil && !in.sender.IsFromMe && carbon == "" && msg.ID != "" && msg.Body != "" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := c.sendStanza(ctx, &xmppx.Message{
				To: msg.From, Type: msg.Type, ID: xmppx.RandomID(),
				ReceiptReceived: &xmppx.Receipt{ID: msg.ID},
			}, false)
			if err != nil {
				log.Debug().Err(err).Msg("Failed to send delivery receipt")
			}
		}()
	}
}

// dispatch turns a resolved message into remote events and queues them.
func (c *XMPPClient) dispatch(ctx context.Context, in *incoming) {
	log := zerolog.Ctx(ctx)
	for _, evt := range c.eventsFor(ctx, in) {
		c.queueRemoteEvent(evt)
	}
	if in.msg.Body != "" || in.msg.Reactions != nil || in.msg.Retract != nil {
		c.noteSeen(in.ts)
	}
	if in.isMUC && in.archiveID != "" && in.msg.Body != "" && in.msg.Replace == nil {
		if err := c.aliases.Put(ctx, c.login.ID, in.portal.ID, in.archiveID, in.messageID()); err != nil {
			log.Warn().Err(err).Msg("Failed to store message alias")
		}
	}
}

func (c *XMPPClient) eventMeta(in *incoming, typ bridgev2.RemoteEventType) simplevent.EventMeta {
	return simplevent.EventMeta{
		Type:      typ,
		PortalKey: in.portal,
		Sender:    in.sender,
		Timestamp: in.ts,
		LogContext: func(lc zerolog.Context) zerolog.Context {
			return lc.Str("xmpp_id", in.id).Stringer("chat", in.chat)
		},
	}
}

// resolveTarget maps an ID referenced by another message (reaction, reply,
// retraction, marker) to a bridged message ID. Group chat references use the
// room's stanza-id; 1:1 references use the sender-assigned ID.
func (c *XMPPClient) resolveTarget(ctx context.Context, portal networkid.PortalKey, isMUC bool, ref string) networkid.MessageID {
	if ref == "" {
		return ""
	}
	if isMUC {
		if id, err := c.aliases.Get(ctx, c.login.ID, portal.ID, ref); err == nil && id != "" {
			return id
		}
	}
	return makeMessageID(portal.ID, ref)
}

// getMessage loads a bridged message by ID, or nil.
func (c *XMPPClient) getMessage(ctx context.Context, id networkid.MessageID) *database.Message {
	msg, err := c.lookupMessage(ctx, id)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to look up message")
		return nil
	}
	return msg
}

// senderMatches reports whether the event's sender wrote the target message.
// XEP-0308 and XEP-0424 only allow changing your own messages.
func (c *XMPPClient) senderMatches(in *incoming, target *database.Message) bool {
	if in.sender.IsFromMe {
		return target.SenderID == makeUserID(c.account) || target.SenderID == ""
	}
	return target.SenderID == in.sender.Sender
}

func (c *XMPPClient) eventsFor(ctx context.Context, in *incoming) []bridgev2.RemoteEvent {
	log := zerolog.Ctx(ctx)
	msg := in.msg
	var events []bridgev2.RemoteEvent

	// Room subject (XEP-0045 §8.1): a message with a subject and no body.
	if in.isMUC && msg.Subject != nil && msg.Body == "" {
		topic := *msg.Subject
		c.mu.Lock()
		if room := c.rooms[in.chat.String()]; room != nil {
			room.subject = &topic
		}
		c.mu.Unlock()
		events = append(events, &simplevent.ChatInfoChange{
			EventMeta:      c.eventMeta(in, bridgev2.RemoteEventChatInfoChange),
			ChatInfoChange: &bridgev2.ChatInfoChange{ChatInfo: &bridgev2.ChatInfo{Topic: &topic}},
		})
		return events
	}

	switch {
	case msg.Retract != nil || (msg.ApplyTo != nil && msg.ApplyTo.Retract != nil):
		ref := ""
		if msg.Retract != nil {
			ref = msg.Retract.ID
		} else {
			ref = msg.ApplyTo.ID
		}
		target := c.resolveTarget(ctx, in.portal, in.isMUC, ref)
		if existing := c.getMessage(ctx, target); existing != nil && !c.senderMatches(in, existing) {
			log.Warn().Str("target", string(target)).Msg("Ignoring retraction of someone else's message")
			return nil
		}
		events = append(events, &simplevent.MessageRemove{
			EventMeta:     c.eventMeta(in, bridgev2.RemoteEventMessageRemove),
			TargetMessage: target,
		})
		return events
	case msg.Replace != nil && msg.Body != "":
		target := makeMessageID(in.portal.ID, msg.Replace.ID)
		events = append(events, &simplevent.Message[*incoming]{
			EventMeta:       c.eventMeta(in, bridgev2.RemoteEventEdit),
			Data:            in,
			ID:              in.messageID(),
			TargetMessage:   target,
			ConvertEditFunc: c.convertEdit,
		})
		return events
	}

	if msg.Reactions != nil {
		target := c.resolveTarget(ctx, in.portal, in.isMUC, msg.Reactions.ID)
		events = append(events, &simplevent.ReactionSync{
			EventMeta:     c.eventMeta(in, bridgev2.RemoteEventReactionSync),
			TargetMessage: target,
			Reactions: &bridgev2.ReactionSyncData{Users: map[networkid.UserID]*bridgev2.ReactionSyncUser{
				in.sender.Sender: {Reactions: reactionsFor(in.sender, msg.Reactions.Reactions, in.ts), HasAllReactions: true},
			}},
		})
	}

	if msg.Body != "" || isEncrypted(msg) {
		meta := c.eventMeta(in, bridgev2.RemoteEventMessage)
		meta.CreatePortal = true
		events = append(events, &simplevent.Message[*incoming]{
			EventMeta:          meta,
			Data:               in,
			ID:                 in.messageID(),
			TransactionID:      networkid.TransactionID(in.id),
			ConvertMessageFunc: c.convertMessage,
		})
	}

	if msg.Displayed != nil && !in.archived {
		target := c.resolveTarget(ctx, in.portal, in.isMUC, msg.Displayed.ID)
		events = append(events, &simplevent.Receipt{
			EventMeta:  c.eventMeta(in, bridgev2.RemoteEventReadReceipt),
			LastTarget: target,
		})
	}
	if msg.ReceiptReceived != nil && !in.isMUC && !in.archived && !in.sender.IsFromMe {
		events = append(events, &simplevent.Receipt{
			EventMeta:  c.eventMeta(in, bridgev2.RemoteEventDeliveryReceipt),
			LastTarget: makeMessageID(in.portal.ID, msg.ReceiptReceived.ID),
			Targets:    []networkid.MessageID{makeMessageID(in.portal.ID, msg.ReceiptReceived.ID)},
		})
	}
	if !in.archived && !in.sender.IsFromMe && in.sender.Sender != "" {
		var timeout time.Duration
		hasState := true
		switch {
		case msg.Composing != nil:
			timeout = 30 * time.Second
		case msg.Paused != nil, msg.Active != nil, msg.Inactive != nil, msg.Gone != nil:
			timeout = 0
		default:
			hasState = false
		}
		// A message body implies the user stopped typing; bridgev2 clears
		// typing on messages itself.
		if hasState && msg.Body == "" {
			events = append(events, &simplevent.Typing{
				EventMeta: c.eventMeta(in, bridgev2.RemoteEventTyping),
				Timeout:   timeout,
				Type:      bridgev2.TypingTypeText,
			})
		}
	}
	return events
}

func reactionsFor(sender bridgev2.EventSender, emojis []string, ts time.Time) []*bridgev2.BackfillReaction {
	out := make([]*bridgev2.BackfillReaction, 0, len(emojis))
	seen := make(map[string]bool, len(emojis))
	for _, e := range emojis {
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, &bridgev2.BackfillReaction{
			Sender:    sender,
			EmojiID:   networkid.EmojiID(e),
			Emoji:     e,
			Timestamp: ts,
		})
	}
	return out
}

func (c *XMPPClient) messageMeta(in *incoming) *MessageMetadata {
	return &MessageMetadata{XMPPID: in.id, ArchiveID: in.archiveID, SenderJID: in.senderJID}
}

// convertMessage converts an incoming message for a portal.
func (c *XMPPClient) convertMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, in *incoming) (*bridgev2.ConvertedMessage, error) {
	cm := &bridgev2.ConvertedMessage{}
	if in.msg.Reply != nil && in.msg.Reply.ID != "" {
		target := c.resolveTarget(ctx, in.portal, in.isMUC, in.msg.Reply.ID)
		cm.ReplyTo = &networkid.MessageOptionalPartID{MessageID: target}
	}
	meta := c.messageMeta(in)
	if att, ok := findAttachment(in.msg); ok && !isEncrypted(in.msg) {
		part := c.convertAttachment(ctx, portal, intent, att)
		part.DBMetadata = meta
		cm.Parts = append(cm.Parts, part)
		return cm, nil
	}
	content := textContent(in.msg)
	if content == nil {
		return nil, fmt.Errorf("message has no content")
	}
	cm.Parts = append(cm.Parts, &bridgev2.ConvertedMessagePart{
		Type:       evtTypeMessage,
		Content:    content,
		DBMetadata: meta,
	})
	return cm, nil
}

// convertEdit converts a XEP-0308 correction.
func (c *XMPPClient) convertEdit(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, existing []*database.Message, in *incoming) (*bridgev2.ConvertedEdit, error) {
	if len(existing) == 0 {
		return nil, fmt.Errorf("edit target not found")
	}
	if !c.senderMatches(in, existing[0]) {
		return nil, fmt.Errorf("refusing correction of a message sent by someone else")
	}
	content := textContent(in.msg)
	if content == nil {
		return nil, fmt.Errorf("correction has no content")
	}
	return &bridgev2.ConvertedEdit{
		ModifiedParts: []*bridgev2.ConvertedEditPart{{
			Part:    existing[0],
			Type:    evtTypeMessage,
			Content: content,
		}},
	}, nil
}
