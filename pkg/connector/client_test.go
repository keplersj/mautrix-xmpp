package connector

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
	"mellium.im/xmpp/jid"

	"github.com/keplersj/mautrix-xmpp/pkg/xmppx"
)

type memAliases map[string]networkid.MessageID

func (m memAliases) key(login networkid.UserLoginID, portal networkid.PortalID, alias string) string {
	return string(login) + "\x00" + string(portal) + "\x00" + alias
}

func (m memAliases) Put(_ context.Context, login networkid.UserLoginID, portal networkid.PortalID, alias string, msg networkid.MessageID) error {
	m[m.key(login, portal, alias)] = msg
	return nil
}

func (m memAliases) Get(_ context.Context, login networkid.UserLoginID, portal networkid.PortalID, alias string) (networkid.MessageID, error) {
	return m[m.key(login, portal, alias)], nil
}

func (m memAliases) GetAlias(_ context.Context, login networkid.UserLoginID, portal networkid.PortalID, msg networkid.MessageID) (string, error) {
	prefix := string(login) + "\x00" + string(portal) + "\x00"
	for k, v := range m {
		if v == msg && len(k) > len(prefix) && k[:len(prefix)] == prefix {
			return k[len(prefix):], nil
		}
	}
	return "", nil
}

func (m memAliases) DeleteLogin(context.Context, networkid.UserLoginID) error { return nil }

type testClient struct {
	*XMPPClient
	events    []bridgev2.RemoteEvent
	sent      []*xmppx.Message
	messages  map[networkid.MessageID]*database.Message
	reactions map[networkid.MessageID][]*database.Reaction
	aliases   memAliases
}

func testConfig(t *testing.T) Config {
	t.Helper()
	var cfg Config
	if err := yaml.Unmarshal([]byte(ExampleConfig), &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

const (
	aliceJID = "alice@example.org"
	bobJID   = "bob@example.net"
	roomJID  = "dev@conference.example.net"
)

func newTestClient(t *testing.T) *testClient {
	t.Helper()
	tc := &testClient{
		messages:  map[networkid.MessageID]*database.Message{},
		reactions: map[networkid.MessageID][]*database.Reaction{},
		aliases:   memAliases{},
	}
	c := &XMPPClient{
		main:    &XMPPConnector{Config: testConfig(t)},
		login:   &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: aliceJID}},
		account: jid.MustParse(aliceJID),
		meta:    &UserLoginMetadata{},
		log:     zerolog.Nop(),
		roster:  map[string]xmppx.RosterItem{},
		rooms:   map[string]*mucRoom{},
	}
	c.queueRemoteEvent = func(evt bridgev2.RemoteEvent) { tc.events = append(tc.events, evt) }
	c.aliases = tc.aliases
	c.lookupMessage = func(_ context.Context, id networkid.MessageID) (*database.Message, error) {
		return tc.messages[id], nil
	}
	c.existingReactions = func(_ context.Context, id networkid.MessageID) ([]*database.Reaction, error) {
		return tc.reactions[id], nil
	}
	c.sendStanza = func(_ context.Context, msg *xmppx.Message, _ bool) error {
		tc.sent = append(tc.sent, msg)
		return nil
	}
	tc.XMPPClient = c
	return tc
}

// joinRoom fakes a joined room with an anonymous occupant "Anon", a
// non-anonymous occupant "Bob" and ourselves as "Alice".
func (tc *testClient) joinRoom() {
	room := jid.MustParse(roomJID)
	tc.rooms[roomJID] = &mucRoom{
		jid:      room,
		occupant: jid.MustParse(roomJID + "/Alice"),
		joined:   true,
		mam:      true,
		occupants: map[string]*mucOccupant{
			"Alice": {nick: "Alice", realJID: jid.MustParse(aliceJID)},
			"Bob":   {nick: "Bob", realJID: jid.MustParse(bobJID)},
			"Anon":  {nick: "Anon"},
		},
	}
}

func (tc *testClient) handle(msg *xmppx.Message) []bridgev2.RemoteEvent {
	tc.events = nil
	tc.HandleMessage(nil, msg)
	return tc.events
}

func single[T any](t *testing.T, events []bridgev2.RemoteEvent) T {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %#v", len(events), events)
	}
	evt, ok := events[0].(T)
	if !ok {
		t.Fatalf("unexpected event type %T", events[0])
	}
	return evt
}

func TestIncomingDirectMessage(t *testing.T) {
	tc := newTestClient(t)
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	evt := single[*simplevent.Message[*incoming]](t, tc.handle(&xmppx.Message{
		From: bobJID + "/phone", To: aliceJID + "/bridge", Type: xmppx.MessageChat, ID: "m1",
		OriginID: &xmppx.OriginID{ID: "o1"}, Body: "hello", Delay: &xmppx.Delay{Stamp: ts},
		StanzaIDs: []xmppx.StanzaID{{ID: "arch1", By: aliceJID}},
	}))
	if evt.PortalKey != makeDMPortalKey(aliceJID, jid.MustParse(bobJID)) {
		t.Errorf("portal = %+v", evt.PortalKey)
	}
	if evt.Sender.Sender != bobJID || evt.Sender.IsFromMe {
		t.Errorf("sender = %+v", evt.Sender)
	}
	if evt.ID != "dm:"+bobJID+"|o1" || !evt.CreatePortal || !evt.Timestamp.Equal(ts) {
		t.Errorf("event = %+v", evt)
	}
	if evt.Data.archiveID != "arch1" {
		t.Errorf("archive ID = %q", evt.Data.archiveID)
	}
	cm, err := tc.convertMessage(context.Background(), nil, nil, evt.Data)
	if err != nil || cm.Parts[0].Content.Body != "hello" {
		t.Fatalf("convert: %+v %v", cm, err)
	}
	meta := cm.Parts[0].DBMetadata.(*MessageMetadata)
	if meta.XMPPID != "o1" || meta.SenderJID != bobJID {
		t.Errorf("metadata = %+v", meta)
	}
}

func TestIncomingCarbons(t *testing.T) {
	tc := newTestClient(t)
	sent := &xmppx.Message{
		From: aliceJID,
		CarbonSent: &xmppx.Carbon{Forwarded: xmppx.Forwarded{Message: &xmppx.Message{
			From: aliceJID + "/laptop", To: bobJID, Type: xmppx.MessageChat, ID: "c1", Body: "from my laptop",
		}}},
	}
	evt := single[*simplevent.Message[*incoming]](t, tc.handle(sent))
	if !evt.Sender.IsFromMe || evt.Sender.SenderLogin != aliceJID || evt.PortalKey.ID != "dm:"+bobJID {
		t.Errorf("sent carbon: %+v %+v", evt.Sender, evt.PortalKey)
	}

	received := &xmppx.Message{
		From: aliceJID,
		CarbonReceived: &xmppx.Carbon{Forwarded: xmppx.Forwarded{Message: &xmppx.Message{
			From: bobJID + "/x", To: aliceJID + "/laptop", Type: xmppx.MessageChat, ID: "c2", Body: "to my laptop",
		}}},
	}
	evt = single[*simplevent.Message[*incoming]](t, tc.handle(received))
	if evt.Sender.IsFromMe || evt.Sender.Sender != bobJID {
		t.Errorf("received carbon: %+v", evt.Sender)
	}

	spoofed := *sent
	spoofed.From = "mallory@example.com"
	if events := tc.handle(&spoofed); len(events) != 0 {
		t.Errorf("spoofed carbon was bridged: %#v", events)
	}
}

func TestIncomingDropped(t *testing.T) {
	tc := newTestClient(t)
	tc.joinRoom()
	for name, msg := range map[string]*xmppx.Message{
		"server announcement": {From: "example.org", Type: xmppx.MessageNormal, Body: "maintenance"},
		"muc private message": {From: roomJID + "/Bob", Type: xmppx.MessageChat, Body: "psst"},
		"unknown room":        {From: "other@conference.example.net/Bob", Type: xmppx.MessageGroupchat, Body: "hi"},
		"error":               {From: bobJID, Type: xmppx.MessageError},
		"foreign mam result":  {From: bobJID, MAMResult: &xmppx.MAMResult{}},
	} {
		if events := tc.handle(msg); len(events) != 0 {
			t.Errorf("%s: expected no events, got %#v", name, events)
		}
	}
}

func TestIncomingGroupchat(t *testing.T) {
	tc := newTestClient(t)
	tc.joinRoom()
	portal := makeMUCPortalKey(aliceJID, jid.MustParse(roomJID))

	evt := single[*simplevent.Message[*incoming]](t, tc.handle(&xmppx.Message{
		From: roomJID + "/Bob", Type: xmppx.MessageGroupchat, ID: "g1", Body: "hi all",
		StanzaIDs: []xmppx.StanzaID{{ID: "room-sid-1", By: roomJID}},
	}))
	if evt.PortalKey != portal || evt.Sender.Sender != bobJID {
		t.Errorf("non-anonymous occupant: %+v %+v", evt.PortalKey, evt.Sender)
	}
	if got, _ := tc.aliases.Get(context.Background(), aliceJID, portal.ID, "room-sid-1"); got != evt.ID {
		t.Errorf("alias not stored: %q", got)
	}

	evt = single[*simplevent.Message[*incoming]](t, tc.handle(&xmppx.Message{
		From: roomJID + "/Anon", Type: xmppx.MessageGroupchat, ID: "g2", Body: "who am i",
	}))
	if evt.Sender.Sender != networkid.UserID("occ:"+roomJID+"/Anon") {
		t.Errorf("anonymous occupant: %+v", evt.Sender)
	}

	evt = single[*simplevent.Message[*incoming]](t, tc.handle(&xmppx.Message{
		From: roomJID + "/Alice", Type: xmppx.MessageGroupchat, ID: "g3", Body: "reflected",
	}))
	if !evt.Sender.IsFromMe {
		t.Errorf("own reflection: %+v", evt.Sender)
	}

	// Reactions in group chats reference the room's stanza-id.
	rs := single[*simplevent.ReactionSync](t, tc.handle(&xmppx.Message{
		From: roomJID + "/Anon", Type: xmppx.MessageGroupchat, ID: "r1",
		Reactions: &xmppx.Reactions{ID: "room-sid-1", Reactions: []string{"👍", "👍", "🎉"}},
	}))
	if rs.TargetMessage != makeMessageID(portal.ID, "g1") {
		t.Errorf("reaction target = %q", rs.TargetMessage)
	}
	user := rs.Reactions.Users[networkid.UserID("occ:"+roomJID+"/Anon")]
	if user == nil || !user.HasAllReactions || len(user.Reactions) != 2 {
		t.Errorf("reaction sync = %+v", user)
	}

	info := single[*simplevent.ChatInfoChange](t, tc.handle(&xmppx.Message{
		From: roomJID, Type: xmppx.MessageGroupchat, Subject: ptrTo("New topic"),
	}))
	if info.ChatInfoChange.ChatInfo.Topic == nil || *info.ChatInfoChange.ChatInfo.Topic != "New topic" {
		t.Errorf("topic = %+v", info.ChatInfoChange.ChatInfo)
	}
}

func ptrTo[T any](v T) *T { return &v }

func TestIncomingUpdates(t *testing.T) {
	tc := newTestClient(t)
	dm := makeDMPortalKey(aliceJID, jid.MustParse(bobJID))
	bobMsg := makeMessageID(dm.ID, "b1")
	myMsg := makeMessageID(dm.ID, "a1")
	tc.messages[bobMsg] = &database.Message{ID: bobMsg, SenderID: bobJID}
	tc.messages[myMsg] = &database.Message{ID: myMsg, SenderID: aliceJID}

	edit := single[*simplevent.Message[*incoming]](t, tc.handle(&xmppx.Message{
		From: bobJID + "/r", Type: xmppx.MessageChat, ID: "e1", Body: "fixed", Replace: &xmppx.Replace{ID: "b1"},
	}))
	if edit.Type != bridgev2.RemoteEventEdit || edit.TargetMessage != bobMsg {
		t.Errorf("edit = %+v", edit)
	}
	ce, err := tc.convertEdit(context.Background(), nil, nil, []*database.Message{tc.messages[bobMsg]}, edit.Data)
	if err != nil || ce.ModifiedParts[0].Content.Body != "fixed" {
		t.Errorf("convert edit: %+v %v", ce, err)
	}
	if _, err = tc.convertEdit(context.Background(), nil, nil, []*database.Message{tc.messages[myMsg]}, edit.Data); err == nil {
		t.Error("bob must not be able to correct alice's message")
	}

	rm := single[*simplevent.MessageRemove](t, tc.handle(&xmppx.Message{
		From: bobJID + "/r", Type: xmppx.MessageChat, ID: "x1", Body: retractFallback, Retract: &xmppx.Retract{ID: "b1"},
	}))
	if rm.TargetMessage != bobMsg {
		t.Errorf("retract target = %q", rm.TargetMessage)
	}
	if events := tc.handle(&xmppx.Message{
		From: bobJID + "/r", Type: xmppx.MessageChat, ID: "x2", Retract: &xmppx.Retract{ID: "a1"},
	}); len(events) != 0 {
		t.Errorf("bob retracted alice's message: %#v", events)
	}
	rm = single[*simplevent.MessageRemove](t, tc.handle(&xmppx.Message{
		From: bobJID + "/r", Type: xmppx.MessageChat, ID: "x3",
		ApplyTo: &xmppx.ApplyTo{ID: "b1", Retract: &xmppx.Empty{}},
	}))
	if rm.TargetMessage != bobMsg {
		t.Errorf("v0 retract target = %q", rm.TargetMessage)
	}

	rr := single[*simplevent.Receipt](t, tc.handle(&xmppx.Message{
		From: bobJID + "/r", Type: xmppx.MessageChat, ID: "d1", Displayed: &xmppx.Marker{ID: "a1"},
	}))
	if rr.Type != bridgev2.RemoteEventReadReceipt || rr.LastTarget != myMsg {
		t.Errorf("read receipt = %+v", rr)
	}
	dr := single[*simplevent.Receipt](t, tc.handle(&xmppx.Message{
		From: bobJID + "/r", Type: xmppx.MessageChat, ID: "d2", ReceiptReceived: &xmppx.Receipt{ID: "a1"},
	}))
	if dr.Type != bridgev2.RemoteEventDeliveryReceipt || dr.LastTarget != myMsg {
		t.Errorf("delivery receipt = %+v", dr)
	}
	typing := single[*simplevent.Typing](t, tc.handle(&xmppx.Message{
		From: bobJID + "/r", Type: xmppx.MessageChat, Composing: &xmppx.Empty{},
	}))
	if typing.Timeout == 0 {
		t.Error("composing should start typing")
	}
	typing = single[*simplevent.Typing](t, tc.handle(&xmppx.Message{
		From: bobJID + "/r", Type: xmppx.MessageChat, Paused: &xmppx.Empty{},
	}))
	if typing.Timeout != 0 {
		t.Error("paused should stop typing")
	}
	reply := single[*simplevent.Message[*incoming]](t, tc.handle(&xmppx.Message{
		From: bobJID + "/r", Type: xmppx.MessageChat, ID: "rp", Body: "> x\nyes",
		Reply:     &xmppx.Reply{ID: "a1", To: aliceJID},
		Fallbacks: []xmppx.Fallback{{For: xmppx.NSReply, Bodies: []xmppx.FallbackRange{{Start: intp(0), End: intp(4)}}}},
	}))
	cm, err := tc.convertMessage(context.Background(), nil, nil, reply.Data)
	if err != nil || cm.ReplyTo == nil || cm.ReplyTo.MessageID != myMsg || cm.Parts[0].Content.Body != "yes" {
		t.Errorf("reply: %+v %v", cm, err)
	}
}

func dmPortal() *bridgev2.Portal {
	return &bridgev2.Portal{Portal: &database.Portal{PortalKey: makeDMPortalKey(aliceJID, jid.MustParse(bobJID))}}
}

func mucPortal() *bridgev2.Portal {
	return &bridgev2.Portal{Portal: &database.Portal{PortalKey: makeMUCPortalKey(aliceJID, jid.MustParse(roomJID))}}
}

func matrixMsg(portal *bridgev2.Portal, content *event.MessageEventContent) *bridgev2.MatrixMessage {
	return &bridgev2.MatrixMessage{MatrixEventBase: bridgev2.MatrixEventBase[*event.MessageEventContent]{
		Event: &event.Event{}, Content: content, Portal: portal,
	}}
}

func TestOutgoingMessages(t *testing.T) {
	ctx := context.Background()
	tc := newTestClient(t)
	portal := dmPortal()
	resp, err := tc.HandleMatrixMessage(ctx, matrixMsg(portal, &event.MessageEventContent{MsgType: event.MsgEmote, Body: "waves"}))
	if err != nil {
		t.Fatal(err)
	}
	out := tc.sent[0]
	if out.To != bobJID || out.Type != xmppx.MessageChat || out.Body != "/me waves" ||
		out.Markable == nil || out.ReceiptRequest == nil || out.Active == nil {
		t.Errorf("stanza = %+v", out)
	}
	if resp.DB.ID != makeMessageID(portal.ID, out.ID) || resp.DB.SenderID != aliceJID {
		t.Errorf("db = %+v", resp.DB)
	}
	sentMsg := resp.DB

	// Edits reference the original message.
	tc.sent = nil
	err = tc.HandleMatrixEdit(ctx, &bridgev2.MatrixEdit{
		MatrixEventBase: bridgev2.MatrixEventBase[*event.MessageEventContent]{Content: &event.MessageEventContent{MsgType: event.MsgText, Body: "new"}, Portal: portal},
		EditTarget:      sentMsg,
	})
	if err != nil || tc.sent[0].Replace == nil || tc.sent[0].Replace.ID != out.ID || tc.sent[0].Body != "new" {
		t.Errorf("edit: %+v %v", tc.sent, err)
	}

	// Retractions carry the v1 element, the v0 fasten wrapper and a fallback.
	tc.sent = nil
	err = tc.HandleMatrixMessageRemove(ctx, &bridgev2.MatrixMessageRemove{
		MatrixEventBase: bridgev2.MatrixEventBase[*event.RedactionEventContent]{Portal: portal},
		TargetMessage:   sentMsg,
	})
	r := tc.sent[0]
	if err != nil || r.Retract == nil || r.Retract.ID != out.ID || r.ApplyTo == nil || r.ApplyTo.ID != out.ID || r.Body != retractFallback || r.Store == nil {
		t.Errorf("retract: %+v %v", r, err)
	}
	err = tc.HandleMatrixMessageRemove(ctx, &bridgev2.MatrixMessageRemove{
		MatrixEventBase: bridgev2.MatrixEventBase[*event.RedactionEventContent]{Portal: portal},
		TargetMessage:   &database.Message{ID: makeMessageID(portal.ID, "b1"), SenderID: bobJID},
	})
	if err == nil {
		t.Error("retracting someone else's message must fail")
	}

	// Replies point at the target's sender-assigned ID and author.
	tc.sent = nil
	bobMsg := &database.Message{ID: makeMessageID(portal.ID, "b1"), SenderID: bobJID, Metadata: &MessageMetadata{XMPPID: "b1", SenderJID: bobJID}}
	reply := matrixMsg(portal, &event.MessageEventContent{MsgType: event.MsgText, Body: "re"})
	reply.ReplyTo = bobMsg
	if _, err = tc.HandleMatrixMessage(ctx, reply); err != nil {
		t.Fatal(err)
	}
	if rp := tc.sent[0].Reply; rp == nil || rp.ID != "b1" || rp.To != bobJID {
		t.Errorf("reply = %+v", rp)
	}

	// Reactions send the complete set.
	tc.sent = nil
	tc.reactions[bobMsg.ID] = []*database.Reaction{{EmojiID: "👍"}}
	pre, _ := tc.PreHandleMatrixReaction(ctx, &bridgev2.MatrixReaction{MatrixEventBase: bridgev2.MatrixEventBase[*event.ReactionEventContent]{
		Content: &event.ReactionEventContent{RelatesTo: event.RelatesTo{Key: "🎉"}},
	}})
	if pre.SenderID != aliceJID || pre.EmojiID != "🎉" {
		t.Errorf("pre = %+v", pre)
	}
	_, err = tc.HandleMatrixReaction(ctx, &bridgev2.MatrixReaction{
		MatrixEventBase: bridgev2.MatrixEventBase[*event.ReactionEventContent]{Portal: portal},
		TargetMessage:   bobMsg, PreHandleResp: &pre,
	})
	if rs := tc.sent[0].Reactions; err != nil || rs == nil || rs.ID != "b1" || len(rs.Reactions) != 2 {
		t.Errorf("reactions = %+v %v", rs, err)
	}
	tc.sent = nil
	tc.messages[bobMsg.ID] = bobMsg
	tc.reactions[bobMsg.ID] = []*database.Reaction{{EmojiID: "👍"}, {EmojiID: "🎉"}}
	err = tc.HandleMatrixReactionRemove(ctx, &bridgev2.MatrixReactionRemove{
		MatrixEventBase: bridgev2.MatrixEventBase[*event.RedactionEventContent]{Portal: portal},
		TargetReaction:  &database.Reaction{MessageID: bobMsg.ID, EmojiID: "👍"},
	})
	if rs := tc.sent[0].Reactions; err != nil || len(rs.Reactions) != 1 || rs.Reactions[0] != "🎉" {
		t.Errorf("reaction removal = %+v %v", rs, err)
	}

	// Read markers only for other people's messages.
	tc.sent = nil
	_ = tc.HandleMatrixReadReceipt(ctx, &bridgev2.MatrixReadReceipt{Portal: portal, ExactMessage: sentMsg})
	_ = tc.HandleMatrixReadReceipt(ctx, &bridgev2.MatrixReadReceipt{Portal: portal, ExactMessage: bobMsg})
	if len(tc.sent) != 1 || tc.sent[0].Displayed == nil || tc.sent[0].Displayed.ID != "b1" {
		t.Errorf("markers = %+v", tc.sent)
	}

	tc.sent = nil
	_ = tc.HandleMatrixTyping(ctx, &bridgev2.MatrixTyping{Portal: portal, IsTyping: true})
	_ = tc.HandleMatrixTyping(ctx, &bridgev2.MatrixTyping{Portal: portal, IsTyping: false})
	if len(tc.sent) != 2 || tc.sent[0].Composing == nil || tc.sent[1].Active == nil || tc.sent[0].NoStore == nil {
		t.Errorf("typing = %+v", tc.sent)
	}

	if _, err = tc.HandleMatrixMessage(ctx, matrixMsg(portal, &event.MessageEventContent{MsgType: event.MsgLocation, Body: "geo"})); err == nil {
		t.Error("expected unsupported message type error")
	}
}

func TestOutgoingGroupchat(t *testing.T) {
	ctx := context.Background()
	tc := newTestClient(t)
	portal := mucPortal()
	if _, err := tc.HandleMatrixMessage(ctx, matrixMsg(portal, &event.MessageEventContent{MsgType: event.MsgText, Body: "hi"})); err == nil {
		t.Fatal("sending to a room we're not in must fail")
	}
	tc.joinRoom()
	resp, err := tc.HandleMatrixMessage(ctx, matrixMsg(portal, &event.MessageEventContent{MsgType: event.MsgText, Body: "hi"}))
	if err != nil {
		t.Fatal(err)
	}
	out := tc.sent[0]
	if out.To != roomJID || out.Type != xmppx.MessageGroupchat || out.ReceiptRequest != nil {
		t.Errorf("stanza = %+v", out)
	}
	if meta := resp.DB.Metadata.(*MessageMetadata); meta.SenderJID != roomJID+"/Alice" {
		t.Errorf("sender JID = %q", meta.SenderJID)
	}

	// Group chat references use the room-assigned stanza-id when known.
	target := &database.Message{ID: makeMessageID(portal.ID, "g1"), SenderID: bobJID, Metadata: &MessageMetadata{XMPPID: "g1"}}
	_ = tc.aliases.Put(ctx, aliceJID, portal.ID, "room-sid-1", target.ID)
	tc.sent = nil
	_, err = tc.HandleMatrixReaction(ctx, &bridgev2.MatrixReaction{
		MatrixEventBase: bridgev2.MatrixEventBase[*event.ReactionEventContent]{Portal: portal},
		TargetMessage:   target, PreHandleResp: &bridgev2.MatrixReactionPreResponse{EmojiID: "👍"},
	})
	if err != nil || tc.sent[0].Reactions.ID != "room-sid-1" || tc.sent[0].Type != xmppx.MessageGroupchat {
		t.Errorf("groupchat reaction = %+v %v", tc.sent[0], err)
	}
	// No typing notifications in group chats.
	tc.sent = nil
	_ = tc.HandleMatrixTyping(ctx, &bridgev2.MatrixTyping{Portal: portal, IsTyping: true})
	if len(tc.sent) != 0 {
		t.Errorf("typing sent to room: %+v", tc.sent)
	}
}

func TestChatInfo(t *testing.T) {
	ctx := context.Background()
	tc := newTestClient(t)
	tc.accountMAM = true
	info, err := tc.GetChatInfo(ctx, dmPortal())
	if err != nil || *info.Type != database.RoomTypeDM || info.Members.OtherUserID != bobJID || !info.CanBackfill || len(info.Members.MemberMap) != 2 {
		t.Errorf("dm info = %+v %v", info, err)
	}
	tc.joinRoom()
	tc.rooms[roomJID].name = "Developers"
	info, err = tc.GetChatInfo(ctx, mucPortal())
	if err != nil || *info.Name != "Developers" || !info.CanBackfill {
		t.Fatalf("muc info = %+v %v", info, err)
	}
	if _, ok := info.Members.MemberMap[networkid.UserID("occ:"+roomJID+"/Anon")]; !ok {
		t.Errorf("anonymous occupant missing from %+v", info.Members.MemberMap)
	}
	if _, ok := info.Members.MemberMap[bobJID]; !ok {
		t.Errorf("bob missing from %+v", info.Members.MemberMap)
	}
	caps := tc.GetCapabilities(ctx, mucPortal())
	if caps.TypingNotifications || caps.Edit != event.CapLevelFullySupported || len(caps.File) != 0 {
		t.Errorf("caps without upload service = %+v", caps)
	}
	tc.upload = &xmppx.UploadService{JID: "upload.example.org", MaxFileSize: 1000}
	caps = tc.GetCapabilities(ctx, dmPortal())
	if !caps.TypingNotifications || caps.File[event.MsgImage] == nil || caps.File[event.MsgImage].MaxSize != 1000 {
		t.Errorf("caps with upload service = %+v", caps)
	}
}

func TestVCardAvatar(t *testing.T) {
	vc := &xmppx.VCard{}
	if vcardAvatar(vc) != nil {
		t.Error("expected no avatar")
	}
	vc.Photo = &struct {
		Type   string `xml:"TYPE"`
		BinVal string `xml:"BINVAL"`
		ExtVal string `xml:"EXTVAL"`
	}{Type: "image/png", BinVal: "aGVs\nbG8="}
	av := vcardAvatar(vc)
	if av == nil || av.ID == "" {
		t.Fatal("expected avatar")
	}
	data, _ := av.Get(context.Background())
	if string(data) != "hello" {
		t.Errorf("avatar data = %q", data)
	}
}
