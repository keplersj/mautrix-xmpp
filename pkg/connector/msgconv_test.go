package connector

import (
	"testing"

	"maunium.net/go/mautrix/event"

	"github.com/keplersj/mautrix-xmpp/pkg/xmppx"
)

func intp(i int) *int { return &i }

func TestStripFallback(t *testing.T) {
	reply := []xmppx.Fallback{{For: xmppx.NSReply, Bodies: []xmppx.FallbackRange{{Start: intp(0), End: intp(9)}}}}
	tests := []struct {
		name      string
		body      string
		fallbacks []xmppx.Fallback
		want      string
	}{
		{"no fallback", "hello", nil, "hello"},
		{"reply quote", "> quoted\nanswer", reply, "answer"},
		{"code points not bytes", "> ünïcö\nok", []xmppx.Fallback{{For: xmppx.NSReply, Bodies: []xmppx.FallbackRange{{Start: intp(0), End: intp(8)}}}}, "ok"},
		{"other namespace ignored", "> quoted\nanswer", []xmppx.Fallback{{For: "urn:example", Bodies: reply[0].Bodies}}, "> quoted\nanswer"},
		{"out of range ignored", "short", []xmppx.Fallback{{For: xmppx.NSReply, Bodies: []xmppx.FallbackRange{{Start: intp(2), End: intp(99)}}}}, "short"},
		{"whole body fallback", "all of it", []xmppx.Fallback{{For: xmppx.NSReply}}, ""},
		{"middle range", "abcdef", []xmppx.Fallback{{For: xmppx.NSReply, Bodies: []xmppx.FallbackRange{{Start: intp(1), End: intp(3)}}}}, "adef"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripFallback(tt.body, tt.fallbacks, xmppx.NSReply); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTextContent(t *testing.T) {
	if c := textContent(&xmppx.Message{Body: "hi"}); c.MsgType != event.MsgText || c.Body != "hi" {
		t.Errorf("plain: %+v", c)
	}
	if c := textContent(&xmppx.Message{Body: "/me waves"}); c.MsgType != event.MsgEmote || c.Body != "waves" {
		t.Errorf("emote: %+v", c)
	}
	if c := textContent(&xmppx.Message{Body: "I sent you an OMEMO encrypted message", OMEMO: &xmppx.Empty{}}); c.MsgType != event.MsgNotice || c.Body != encryptedNotice {
		t.Errorf("omemo: %+v", c)
	}
	if c := textContent(&xmppx.Message{Body: "x", OMEMO2: &xmppx.Empty{}}); c.Body != encryptedNotice {
		t.Errorf("omemo2: %+v", c)
	}
	msg := &xmppx.Message{
		Body:      "> q\nreply",
		Reply:     &xmppx.Reply{ID: "1"},
		Fallbacks: []xmppx.Fallback{{For: xmppx.NSReply, Bodies: []xmppx.FallbackRange{{Start: intp(0), End: intp(4)}}}},
	}
	if c := textContent(msg); c.Body != "reply" {
		t.Errorf("reply fallback not stripped: %+v", c)
	}
	if c := textContent(&xmppx.Message{}); c != nil {
		t.Errorf("empty: %+v", c)
	}
}

func TestFindAttachment(t *testing.T) {
	url := "https://upload.example.org/abc/My%20Photo.jpg"
	att, ok := findAttachment(&xmppx.Message{Body: url, OOB: []xmppx.OOB{{URL: url}}})
	if !ok || att.URL != url || att.Caption != "" {
		t.Fatalf("plain file: %+v %v", att, ok)
	}
	att, ok = findAttachment(&xmppx.Message{Body: "look at this " + url, OOB: []xmppx.OOB{{URL: url}}})
	if !ok || att.Caption != "look at this" {
		t.Fatalf("caption: %+v", att)
	}
	att, ok = findAttachment(&xmppx.Message{Body: url, OOB: []xmppx.OOB{{URL: url, Desc: "holiday"}}})
	if !ok || att.Caption != "holiday" {
		t.Fatalf("desc as caption: %+v", att)
	}
	if _, ok = findAttachment(&xmppx.Message{Body: "aesgcm://x/y", OOB: []xmppx.OOB{{URL: "aesgcm://x/y#key"}}}); ok {
		t.Fatal("encrypted aesgcm:// links must not be downloaded")
	}
	if _, ok = findAttachment(&xmppx.Message{Body: url}); ok {
		t.Fatal("a bare URL without OOB is text")
	}
	if name := fileNameFromURL(url); name != "My Photo.jpg" {
		t.Errorf("file name = %q", name)
	}
	if name := fileNameFromURL("https://example.org/"); name != "file" {
		t.Errorf("file name = %q", name)
	}
}

func TestMime(t *testing.T) {
	tests := []struct {
		header, name, want string
		msgType            event.MessageType
	}{
		{"image/png", "x", "image/png", event.MsgImage},
		{"application/octet-stream", "clip.mp4", "video/mp4", event.MsgVideo},
		{"", "voice.ogg", "audio/ogg", event.MsgAudio},
		{"text/plain; charset=utf-8", "notes.txt", "text/plain", event.MsgFile},
		{"", "unknown.zzz", "application/octet-stream", event.MsgFile},
	}
	for _, tt := range tests {
		got := guessMime(tt.header, tt.name)
		if got != tt.want {
			t.Errorf("guessMime(%q, %q) = %q, want %q", tt.header, tt.name, got, tt.want)
		}
		if mt := msgTypeForMime(got); mt != tt.msgType {
			t.Errorf("msgTypeForMime(%q) = %q, want %q", got, mt, tt.msgType)
		}
	}
}

func TestIDsFromStanza(t *testing.T) {
	msg := &xmppx.Message{
		ID:        "attr-id",
		OriginID:  &xmppx.OriginID{ID: "origin"},
		StanzaIDs: []xmppx.StanzaID{{ID: "spoof", By: "evil@example.org"}, {ID: "real", By: "Room@Conference.example.org"}},
	}
	if id := xmppID(msg); id != "origin" {
		t.Errorf("xmppID = %q", id)
	}
	msg.OriginID = nil
	if id := xmppID(msg); id != "attr-id" {
		t.Errorf("xmppID without origin-id = %q", id)
	}
	if id := archiveIDBy(msg, "room@conference.example.org"); id != "real" {
		t.Errorf("archiveIDBy = %q", id)
	}
	if id := archiveIDBy(msg, "other@example.org"); id != "" {
		t.Errorf("archiveIDBy unknown = %q", id)
	}
}

func TestMatrixBody(t *testing.T) {
	if b := matrixBody(&event.MessageEventContent{MsgType: event.MsgEmote, Body: "dances"}); b != "/me dances" {
		t.Errorf("emote = %q", b)
	}
	c := &event.MessageEventContent{
		MsgType:       event.MsgText,
		Body:          "> <@a:b> quoted\n\nactual",
		Format:        event.FormatHTML,
		FormattedBody: "<mx-reply><blockquote>quoted</blockquote></mx-reply>actual",
	}
	c.RelatesTo = (&event.RelatesTo{}).SetReplyTo("$x")
	if b := matrixBody(c); b != "actual" {
		t.Errorf("reply fallback = %q", b)
	}
}
