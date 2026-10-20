//go:build integration

// Package integration contains the end-to-end test of mautrix-xmpp. It needs
// the Docker Compose environment in this directory: Prosody, Synapse and the
// bridge image. The test plays a Matrix user (who logs the bridge into the
// XMPP account alice) and the XMPP users bob and alice's other clients.
package integration

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"mellium.im/xmpp/jid"

	"github.com/keplersj/mautrix-xmpp/pkg/xmppx"
)

func TestEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	alice := "alice@" + xmppDomain
	bobJID := "bob@" + xmppDomain
	room := jid.MustParse("e2e-" + xmppx.RandomID()[:8] + "@" + mucDomain)
	const roomName = "E2E Room"

	// --- XMPP setup: bob creates the room, alice bookmarks it. ---
	bob := connectXMPP(t, ctx, "bob", "")
	defer bob.session.Close()
	if _, err := bob.session.JoinMUC(ctx, room, "Bob", ""); err != nil {
		t.Fatalf("bob join: %v", err)
	}
	setup := connectXMPP(t, ctx, "alice", "")
	if err := setup.session.PublishBookmark(ctx, xmppx.Bookmark{JID: room, Name: roomName, Nick: "Alice", Autojoin: true}); err != nil {
		t.Fatalf("publish bookmark: %v", err)
	}
	// History from before the bridge existed, for the initial backfill.
	history := uniq("history before login")
	bob.send(ctx, &xmppx.Message{To: alice, Type: xmppx.MessageChat, Body: history})
	setup.waitMessage("history message", func(m *xmppx.Message) bool { return m.Body == history })
	setup.session.Close()

	// --- Matrix login through the bridge bot, like a real user. ---
	me := registerMatrixUser(t, ctx, "tester"+strings.ToLower(xmppx.RandomID()[:6]))
	bot := id.NewUserID("xmppbot", matrixDomain)
	mgmt, err := me.client.CreateRoom(ctx, &mautrix.ReqCreateRoom{Invite: []id.UserID{bot}, IsDirect: true, Preset: "trusted_private_chat"})
	if err != nil {
		t.Fatalf("create management room: %v", err)
	}
	botSays := func(substr string) {
		t.Helper()
		me.waitMessage("bot: "+substr, func(evt *event.Event, c *event.MessageEventContent) bool {
			return evt.RoomID == mgmt.RoomID && evt.Sender == bot && strings.Contains(c.Body, substr)
		})
	}
	me.waitEvent("bot joins management room", func(evt *event.Event) bool {
		return evt.RoomID == mgmt.RoomID && evt.Type == event.StateMember && evt.GetStateKey() == string(bot) &&
			evt.Content.AsMember().Membership == event.MembershipJoin
	})
	me.send(ctx, mgmt.RoomID, &event.MessageEventContent{MsgType: event.MsgText, Body: "login"})
	botSays("XMPP address")
	me.send(ctx, mgmt.RoomID, &event.MessageEventContent{MsgType: event.MsgText, Body: alice})
	botSays("Password")
	me.send(ctx, mgmt.RoomID, &event.MessageEventContent{MsgType: event.MsgText, Body: "alice-password"})
	botSays("Server address")
	me.send(ctx, mgmt.RoomID, &event.MessageEventContent{MsgType: event.MsgText, Body: "skip"})
	botSays("Successfully logged in as " + alice)

	bobGhost := ghostMXID(bobJID)

	// --- Group chat portal from the bookmark. ---
	nameEvt := me.waitEvent("MUC portal", func(evt *event.Event) bool {
		return evt.Type == event.StateRoomName && evt.Content.AsRoomName().Name == roomName
	})
	mucRoom := nameEvt.RoomID
	me.waitEvent("joined MUC portal", func(evt *event.Event) bool {
		return evt.RoomID == mucRoom && evt.Type == event.StateMember && evt.GetStateKey() == string(me.client.UserID) &&
			evt.Content.AsMember().Membership == event.MembershipJoin
	})

	// --- 1:1 text, both directions, plus initial backfill. ---
	text1 := uniq("hello from bob")
	bobMsg1 := bob.send(ctx, &xmppx.Message{To: alice, Type: xmppx.MessageChat, Body: text1, Markable: &xmppx.Empty{}})
	evt1 := me.waitBody(text1)
	dmRoom := evt1.RoomID
	if evt1.Sender != bobGhost {
		t.Errorf("sender = %s, want %s", evt1.Sender, bobGhost)
	}
	step(t, "InitialBackfill", func() {
		evt := me.waitBody(history)
		if evt.RoomID != dmRoom {
			t.Errorf("history landed in %s, want %s", evt.RoomID, dmRoom)
		}
	})

	text2 := uniq("hello from matrix")
	mxEvt2 := me.send(ctx, dmRoom, &event.MessageEventContent{MsgType: event.MsgText, Body: text2})
	gotFromAlice := bob.waitMessage("matrix message", func(m *xmppx.Message) bool { return m.Body == text2 })
	if !strings.HasPrefix(gotFromAlice.From, alice+"/") || gotFromAlice.Type != xmppx.MessageChat || gotFromAlice.OriginID == nil {
		t.Errorf("unexpected stanza from bridge: %+v", gotFromAlice)
	}
	aliceMsgID := gotFromAlice.OriginID.ID

	step(t, "EditFromXMPP", func() {
		edited := text1 + " (edited)"
		bob.send(ctx, &xmppx.Message{To: alice, Type: xmppx.MessageChat, Body: edited, Replace: &xmppx.Replace{ID: bobMsg1.ID}})
		me.waitMessage("edit", func(evt *event.Event, c *event.MessageEventContent) bool {
			return evt.RoomID == dmRoom && c.RelatesTo != nil && c.RelatesTo.Type == event.RelReplace &&
				c.RelatesTo.EventID == evt1.ID && c.NewContent != nil && c.NewContent.Body == edited
		})
	})

	step(t, "EditFromMatrix", func() {
		edited := text2 + " (edited)"
		content := &event.MessageEventContent{MsgType: event.MsgText, Body: "* " + edited}
		content.SetEdit(mxEvt2)
		content.NewContent = &event.MessageEventContent{MsgType: event.MsgText, Body: edited}
		me.send(ctx, dmRoom, content)
		bob.waitMessage("correction", func(m *xmppx.Message) bool {
			return m.Replace != nil && m.Replace.ID == aliceMsgID && m.Body == edited
		})
	})

	step(t, "ReactionFromXMPP", func() {
		bob.send(ctx, &xmppx.Message{To: alice, Type: xmppx.MessageChat, Reactions: &xmppx.Reactions{ID: aliceMsgID, Reactions: []string{"👍"}}})
		me.waitEvent("reaction", func(evt *event.Event) bool {
			if evt.Type != event.EventReaction || evt.Sender != bobGhost {
				return false
			}
			rel := evt.Content.AsReaction().RelatesTo
			return rel.EventID == mxEvt2 && strings.TrimSuffix(rel.Key, "\ufe0f") == "👍"
		})
	})

	step(t, "ReactionFromMatrix", func() {
		if _, err := me.client.SendReaction(ctx, dmRoom, evt1.ID, "❤️"); err != nil {
			t.Fatal(err)
		}
		bob.waitMessage("reaction", func(m *xmppx.Message) bool {
			return m.Reactions != nil && m.Reactions.ID == bobMsg1.ID && len(m.Reactions.Reactions) == 1 && strings.TrimSuffix(m.Reactions.Reactions[0], "\ufe0f") == "❤"
		})
	})

	step(t, "RetractionFromXMPP", func() {
		body := uniq("retract me")
		m := bob.send(ctx, &xmppx.Message{To: alice, Type: xmppx.MessageChat, Body: body})
		target := me.waitBody(body)
		bob.send(ctx, &xmppx.Message{To: alice, Type: xmppx.MessageChat, Body: "retracted", Retract: &xmppx.Retract{ID: m.ID}})
		me.waitEvent("redaction", func(evt *event.Event) bool {
			return evt.Type == event.EventRedaction && evt.Redacts == target.ID
		})
	})

	step(t, "RedactionFromMatrix", func() {
		body := uniq("matrix retract me")
		evtID := me.send(ctx, dmRoom, &event.MessageEventContent{MsgType: event.MsgText, Body: body})
		orig := bob.waitMessage("message to retract", func(m *xmppx.Message) bool { return m.Body == body })
		if _, err := me.client.RedactEvent(ctx, dmRoom, evtID); err != nil {
			t.Fatal(err)
		}
		bob.waitMessage("retraction", func(m *xmppx.Message) bool {
			return m.Retract != nil && m.Retract.ID == orig.OriginID.ID
		})
	})

	step(t, "ReplyFromMatrix", func() {
		body := uniq("a reply")
		content := &event.MessageEventContent{MsgType: event.MsgText, Body: body}
		content.RelatesTo = (&event.RelatesTo{}).SetReplyTo(evt1.ID)
		me.send(ctx, dmRoom, content)
		bob.waitMessage("reply", func(m *xmppx.Message) bool {
			return m.Body == body && m.Reply != nil && m.Reply.ID == bobMsg1.ID
		})
	})

	step(t, "ReplyFromXMPP", func() {
		body := uniq("reply from bob")
		bob.send(ctx, &xmppx.Message{To: alice, Type: xmppx.MessageChat, Body: "> quoted\n" + body,
			Reply:     &xmppx.Reply{ID: aliceMsgID, To: alice},
			Fallbacks: []xmppx.Fallback{{For: xmppx.NSReply, Bodies: []xmppx.FallbackRange{{Start: ptr(0), End: ptr(9)}}}},
		})
		me.waitMessage("reply", func(evt *event.Event, c *event.MessageEventContent) bool {
			return c.Body == body && c.RelatesTo != nil && c.RelatesTo.GetReplyTo() == mxEvt2
		})
	})

	step(t, "TypingAndMarkers", func() {
		bob.send(ctx, &xmppx.Message{To: alice, Type: xmppx.MessageChat, Composing: &xmppx.Empty{}})
		me.waitEvent("typing", func(evt *event.Event) bool {
			return evt.Type == event.EphemeralEventTyping && evt.RoomID == dmRoom &&
				len(evt.Content.AsTyping().UserIDs) > 0 && evt.Content.AsTyping().UserIDs[0] == bobGhost
		})
		bob.send(ctx, &xmppx.Message{To: alice, Type: xmppx.MessageChat, Displayed: &xmppx.Marker{ID: aliceMsgID}})
		me.waitEvent("read receipt", func(evt *event.Event) bool {
			if evt.Type != event.EphemeralEventReceipt || evt.RoomID != dmRoom {
				return false
			}
			for _, receipts := range *evt.Content.AsReceipt() {
				if _, ok := receipts[event.ReceiptTypeRead][bobGhost]; ok {
					return true
				}
			}
			return false
		})
	})

	step(t, "FileFromMatrix", func() {
		data := []byte("file from matrix " + xmppx.RandomID())
		up, err := me.client.UploadBytes(ctx, data, "text/plain")
		if err != nil {
			t.Fatal(err)
		}
		me.send(ctx, dmRoom, &event.MessageEventContent{
			MsgType: event.MsgFile, Body: "notes.txt", URL: up.ContentURI.CUString(),
			Info: &event.FileInfo{MimeType: "text/plain", Size: len(data)},
		})
		m := bob.waitMessage("file", func(m *xmppx.Message) bool { return len(m.OOB) > 0 && strings.HasSuffix(m.OOB[0].URL, "notes.txt") })
		if m.Body != m.OOB[0].URL {
			t.Errorf("body %q != oob url %q", m.Body, m.OOB[0].URL)
		}
		if got := httpGet(t, ctx, m.OOB[0].URL); !bytes.Equal(got, data) {
			t.Errorf("downloaded %q, want %q", got, data)
		}
	})

	step(t, "FileFromXMPP", func() {
		data := append([]byte("\x89PNG\r\n\x1a\n"), []byte(xmppx.RandomID())...)
		url := bob.uploadFile(ctx, "picture.png", "image/png", data)
		bob.send(ctx, &xmppx.Message{To: alice, Type: xmppx.MessageChat, Body: url, OOB: []xmppx.OOB{{URL: url}}})
		_, c := me.waitMessage("image", func(evt *event.Event, c *event.MessageEventContent) bool {
			return evt.RoomID == dmRoom && c.MsgType == event.MsgImage && c.Body == "picture.png"
		})
		uri, err := c.URL.Parse()
		if err != nil {
			t.Fatal(err)
		}
		got, err := me.client.DownloadBytes(ctx, uri)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data) {
			t.Error("downloaded image differs from upload")
		}
	})

	step(t, "GroupChat", func() {
		body := uniq("hi room")
		bobGroupMsg := bob.send(ctx, &xmppx.Message{To: room.String(), Type: xmppx.MessageGroupchat, Body: body})
		evt := me.waitBody(body)
		if evt.RoomID != mucRoom {
			t.Errorf("groupchat message in %s, want %s", evt.RoomID, mucRoom)
		}
		if evt.Sender != bobGhost {
			t.Errorf("groupchat sender = %s, want %s (room has public JIDs)", evt.Sender, bobGhost)
		}
		// The room's stanza-id for bob's message, as bob saw it reflected.
		reflected := bob.waitMessage("reflection", func(m *xmppx.Message) bool { return m.ID == bobGroupMsg.ID && m.Type == xmppx.MessageGroupchat })
		stanzaID := ""
		for _, sid := range reflected.StanzaIDs {
			if sid.By == room.String() {
				stanzaID = sid.ID
			}
		}

		reply := uniq("hi from matrix room")
		me.send(ctx, mucRoom, &event.MessageEventContent{MsgType: event.MsgText, Body: reply})
		got := bob.waitMessage("groupchat from bridge", func(m *xmppx.Message) bool { return m.Body == reply })
		if got.From != room.String()+"/Alice" {
			t.Errorf("groupchat from %s, want %s/Alice", got.From, room)
		}
		if stanzaID == "" {
			t.Error("room did not assign a stanza-id")
			return
		}
		if _, err := me.client.SendReaction(ctx, mucRoom, evt.ID, "🎉"); err != nil {
			t.Fatal(err)
		}
		bob.waitMessage("groupchat reaction", func(m *xmppx.Message) bool {
			return m.Type == xmppx.MessageGroupchat && m.Reactions != nil && m.Reactions.ID == stanzaID
		})
	})

	step(t, "ReconnectAndCatchUp", func() {
		// Find the bridge's resource through presence from alice's own account.
		probe := connectXMPP(t, ctx, "alice", "")
		p := probe.waitPresence("bridge presence", func(p *xmppx.Presence) bool {
			from, err := jid.Parse(p.From)
			return err == nil && from.Bare().String() == alice && strings.HasPrefix(from.Resourcepart(), "mautrix-xmpp.") && p.Type == ""
		})
		bridgeJID := jid.MustParse(p.From)
		// Logging in with the same resource kicks the bridge's session
		// (stream conflict); the bridge reconnects after its backoff.
		thief := connectXMPP(t, ctx, "alice", bridgeJID.Resourcepart())
		dm := uniq("while you were away")
		bob.send(ctx, &xmppx.Message{To: alice, Type: xmppx.MessageChat, Body: dm})
		probe.waitMessage("message delivered to another resource", func(m *xmppx.Message) bool { return m.Body == dm })
		group := uniq("missed in the room")
		bob.send(ctx, &xmppx.Message{To: room.String(), Type: xmppx.MessageGroupchat, Body: group})
		time.Sleep(time.Second)
		thief.session.Close()
		probe.session.Close()

		evt := me.waitBody(dm)
		if evt.RoomID != dmRoom {
			t.Errorf("caught-up DM in %s, want %s", evt.RoomID, dmRoom)
		}
		evt = me.waitBody(group)
		if evt.RoomID != mucRoom {
			t.Errorf("caught-up groupchat in %s, want %s", evt.RoomID, mucRoom)
		}
		// And live traffic works again afterwards.
		after := uniq("after reconnect")
		me.send(ctx, dmRoom, &event.MessageEventContent{MsgType: event.MsgText, Body: after})
		bob.waitMessage("after reconnect", func(m *xmppx.Message) bool { return m.Body == after })
	})
}

func ptr[T any](v T) *T { return &v }

// step runs one part of the scenario. Steps share state and run in order, so
// a failure stops the test (they are not independent subtests).
func step(t *testing.T, name string, fn func()) {
	t.Helper()
	start := time.Now()
	fn()
	t.Logf("step %s passed in %s", name, time.Since(start).Round(time.Millisecond))
}
