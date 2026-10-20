package connector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"mellium.im/xmpp/jid"

	"github.com/keplersj/mautrix-xmpp/pkg/xmppx"
)

// ackTimeout is how long HandleMatrixMessage waits for the server's XEP-0198
// acknowledgement before reporting success anyway. Unacknowledged messages
// are re-sent after a reconnect.
const ackTimeout = 10 * time.Second

// chatTarget is where outgoing stanzas for a portal go.
type chatTarget struct {
	to    jid.JID
	isMUC bool
	typ   string
}

func (c *XMPPClient) targetFor(portal *bridgev2.Portal) (chatTarget, error) {
	j, isMUC, err := parsePortalID(portal.ID)
	if err != nil {
		return chatTarget{}, err
	}
	t := chatTarget{to: j, isMUC: isMUC, typ: xmppx.MessageChat}
	if isMUC {
		t.typ = xmppx.MessageGroupchat
		if room := c.getRoom(j); room == nil || !room.joined {
			return t, fmt.Errorf("not joined to %s", j)
		}
	}
	return t, nil
}

// refID returns the ID other XMPP clients use to reference a bridged message:
// the room's stanza-id in group chats (falling back to the sender's ID if the
// room doesn't assign one), the sender-assigned ID in 1:1 chats.
func (c *XMPPClient) refID(ctx context.Context, portal *bridgev2.Portal, msg *database.Message, isMUC bool) string {
	meta, _ := msg.Metadata.(*MessageMetadata)
	xid := ""
	if meta != nil {
		xid = meta.XMPPID
		if isMUC && meta.ArchiveID != "" {
			return meta.ArchiveID
		}
	}
	if xid == "" {
		_, xid, _ = parseMessageID(msg.ID)
	}
	if isMUC {
		if alias, err := c.aliases.GetAlias(ctx, c.login.ID, portal.ID, msg.ID); err == nil && alias != "" {
			return alias
		}
	}
	return xid
}

// send sends a message and waits briefly for the server to acknowledge it.
func (c *XMPPClient) send(ctx context.Context, msg *xmppx.Message) error {
	return c.sendStanza(ctx, msg, true)
}

func (c *XMPPClient) sendViaSession(ctx context.Context, msg *xmppx.Message, waitAck bool) error {
	s, err := c.session()
	if err != nil {
		return err
	}
	if err = s.SendMessage(ctx, msg); err != nil || !waitAck {
		return err
	}
	ackCtx, cancel := context.WithTimeout(ctx, ackTimeout)
	defer cancel()
	if !s.WaitAck(ackCtx, msg.ID) && s.StreamManagementEnabled() {
		zerolog.Ctx(ctx).Warn().Str("xmpp_id", msg.ID).Msg("Server did not acknowledge message in time, it will be re-sent after reconnecting if needed")
	}
	return nil
}

func (c *XMPPClient) newOutgoing(t chatTarget) *xmppx.Message {
	msg := &xmppx.Message{To: t.to.String(), Type: t.typ, ID: xmppx.RandomID(), Markable: &xmppx.Empty{}}
	if !t.isMUC {
		msg.ReceiptRequest = &xmppx.Empty{}
		msg.Active = &xmppx.Empty{}
	}
	return msg
}

func (c *XMPPClient) ownSenderJID(t chatTarget) string {
	if t.isMUC {
		if room := c.getRoom(t.to); room != nil {
			return room.occupant.String()
		}
	}
	return c.account.String()
}

func (c *XMPPClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	t, err := c.targetFor(msg.Portal)
	if err != nil {
		return nil, err
	}
	content := msg.Content
	out := c.newOutgoing(t)
	var caption string
	switch content.MsgType {
	case event.MsgText, event.MsgNotice, event.MsgEmote:
		out.Body = matrixBody(content)
	case event.MsgImage, event.MsgVideo, event.MsgAudio, event.MsgFile:
		url, cap, err := c.uploadMatrixMedia(ctx, content)
		if err != nil {
			return nil, err
		}
		out.Body = url
		out.OOB = []xmppx.OOB{{URL: url}}
		caption = cap
	default:
		return nil, fmt.Errorf("%w: %s", bridgev2.ErrUnsupportedMessageType, content.MsgType)
	}
	if msg.ReplyTo != nil {
		out.Reply = &xmppx.Reply{ID: c.refID(ctx, msg.Portal, msg.ReplyTo, t.isMUC)}
		if meta, ok := msg.ReplyTo.Metadata.(*MessageMetadata); ok && meta.SenderJID != "" {
			out.Reply.To = meta.SenderJID
		}
	}
	if err = c.send(ctx, out); err != nil {
		return nil, err
	}
	if caption != "" {
		capMsg := c.newOutgoing(t)
		capMsg.Body = caption
		if err = c.send(ctx, capMsg); err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to send caption")
		}
	}
	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:        makeMessageID(msg.Portal.ID, out.ID),
			SenderID:  makeUserID(c.account),
			Timestamp: time.Now(),
			Metadata:  &MessageMetadata{XMPPID: out.ID, SenderJID: c.ownSenderJID(t)},
		},
	}, nil
}

// uploadMatrixMedia downloads Matrix media and uploads it to the XMPP server.
// It returns the URL and the caption, if any.
func (c *XMPPClient) uploadMatrixMedia(ctx context.Context, content *event.MessageEventContent) (string, string, error) {
	fileName := content.FileName
	caption := ""
	if fileName == "" {
		fileName = content.Body
	} else if content.Body != "" && content.Body != fileName {
		caption = content.Body
	}
	if fileName == "" {
		fileName = "file"
	}
	mimeType := "application/octet-stream"
	if content.Info != nil && content.Info.MimeType != "" {
		mimeType = content.Info.MimeType
	}
	if content.Info != nil && int64(content.Info.Size) > c.main.Config.Media.MaxUploadSize {
		return "", "", fmt.Errorf("file is larger than the bridge's upload limit")
	}
	uri := content.URL
	if content.File != nil {
		uri = content.File.URL
	}
	data, err := c.main.br.Bot.DownloadMedia(ctx, uri, content.File)
	if err != nil {
		return "", "", fmt.Errorf("failed to download media from Matrix: %w", err)
	}
	url, err := c.uploadToXMPP(ctx, data, fileName, mimeType)
	if err != nil {
		return "", "", err
	}
	return url, caption, nil
}

func (c *XMPPClient) HandleMatrixEdit(ctx context.Context, msg *bridgev2.MatrixEdit) error {
	t, err := c.targetFor(msg.Portal)
	if err != nil {
		return err
	}
	if msg.Content.MsgType.IsMedia() {
		return fmt.Errorf("%w: editing files is not supported", bridgev2.ErrUnsupportedMessageType)
	}
	meta, _ := msg.EditTarget.Metadata.(*MessageMetadata)
	orig := ""
	if meta != nil {
		orig = meta.XMPPID
	}
	if orig == "" {
		_, orig, _ = parseMessageID(msg.EditTarget.ID)
	}
	out := c.newOutgoing(t)
	out.Body = matrixBody(msg.Content)
	// XEP-0308 §3: always reference the original message, not earlier edits.
	out.Replace = &xmppx.Replace{ID: orig}
	return c.send(ctx, out)
}

func (c *XMPPClient) HandleMatrixMessageRemove(ctx context.Context, msg *bridgev2.MatrixMessageRemove) error {
	t, err := c.targetFor(msg.Portal)
	if err != nil {
		return err
	}
	if msg.TargetMessage.SenderID != makeUserID(c.account) {
		return errors.New("XMPP only allows retracting your own messages")
	}
	ref := c.refID(ctx, msg.Portal, msg.TargetMessage, t.isMUC)
	out := c.newOutgoing(t)
	out.Markable, out.ReceiptRequest = nil, nil
	out.Body = retractFallback
	out.Retract = &xmppx.Retract{ID: ref}
	out.ApplyTo = &xmppx.ApplyTo{ID: ref, Retract: &xmppx.Empty{}}
	out.Fallbacks = []xmppx.Fallback{{For: xmppx.NSRetract}}
	out.Store = &xmppx.Empty{}
	return c.send(ctx, out)
}

func (c *XMPPClient) PreHandleMatrixReaction(_ context.Context, msg *bridgev2.MatrixReaction) (bridgev2.MatrixReactionPreResponse, error) {
	emoji := msg.Content.RelatesTo.Key
	return bridgev2.MatrixReactionPreResponse{
		SenderID: makeUserID(c.account),
		EmojiID:  networkid.EmojiID(emoji),
		Emoji:    emoji,
	}, nil
}

// sendReactions sends the complete set of our reactions to a message
// (XEP-0444 has replace-all semantics).
func (c *XMPPClient) sendReactions(ctx context.Context, portal *bridgev2.Portal, target *database.Message, add, remove networkid.EmojiID) error {
	t, err := c.targetFor(portal)
	if err != nil {
		return err
	}
	existing, err := c.existingReactions(ctx, target.ID)
	if err != nil {
		return fmt.Errorf("failed to get existing reactions: %w", err)
	}
	var emojis []string
	seen := map[networkid.EmojiID]bool{}
	for _, r := range existing {
		if r.EmojiID == remove || seen[r.EmojiID] {
			continue
		}
		seen[r.EmojiID] = true
		emojis = append(emojis, string(r.EmojiID))
	}
	if add != "" && !seen[add] {
		emojis = append(emojis, string(add))
	}
	out := &xmppx.Message{
		To: t.to.String(), Type: t.typ, ID: xmppx.RandomID(),
		Reactions: &xmppx.Reactions{ID: c.refID(ctx, portal, target, t.isMUC), Reactions: emojis},
		Store:     &xmppx.Empty{},
	}
	return c.send(ctx, out)
}

func (c *XMPPClient) HandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (*database.Reaction, error) {
	if err := c.sendReactions(ctx, msg.Portal, msg.TargetMessage, msg.PreHandleResp.EmojiID, ""); err != nil {
		return nil, err
	}
	return &database.Reaction{}, nil
}

func (c *XMPPClient) HandleMatrixReactionRemove(ctx context.Context, msg *bridgev2.MatrixReactionRemove) error {
	target := c.getMessage(ctx, msg.TargetReaction.MessageID)
	if target == nil {
		return errors.New("reaction target not found")
	}
	return c.sendReactions(ctx, msg.Portal, target, "", msg.TargetReaction.EmojiID)
}

func (c *XMPPClient) HandleMatrixReadReceipt(ctx context.Context, msg *bridgev2.MatrixReadReceipt) error {
	if !c.main.Config.SendReadMarkers || msg.ExactMessage == nil || msg.ExactMessage.SenderID == makeUserID(c.account) {
		return nil
	}
	t, err := c.targetFor(msg.Portal)
	if err != nil {
		return err
	}
	return c.sendStanza(ctx, &xmppx.Message{
		To: t.to.String(), Type: t.typ, ID: xmppx.RandomID(),
		Displayed: &xmppx.Marker{ID: c.refID(ctx, msg.Portal, msg.ExactMessage, t.isMUC)},
		Store:     &xmppx.Empty{},
	}, false)
}

func (c *XMPPClient) HandleMatrixTyping(ctx context.Context, msg *bridgev2.MatrixTyping) error {
	if !c.main.Config.SendTyping {
		return nil
	}
	t, err := c.targetFor(msg.Portal)
	if err != nil {
		// Typing in a chat we can't send to is not worth an error.
		return nil //nolint:nilerr
	}
	if t.isMUC {
		return nil
	}
	out := &xmppx.Message{To: t.to.String(), Type: t.typ, ID: xmppx.RandomID(), NoStore: &xmppx.Empty{}}
	if msg.IsTyping {
		out.Composing = &xmppx.Empty{}
	} else {
		out.Active = &xmppx.Empty{}
	}
	if err = c.sendStanza(ctx, out, false); errors.Is(err, errNotConnected) {
		return nil
	}
	return err
}
