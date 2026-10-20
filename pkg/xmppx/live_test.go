package xmppx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"mellium.im/xmpp/jid"
)

// These tests run against a real XMPP server and are skipped unless
// XMPPX_TEST_DOMAIN is set. See integration/docker-compose.dev.yml.
//
//	XMPPX_TEST_DOMAIN=xmpp.test XMPPX_TEST_CA=integration/.certs/ca.crt go test ./pkg/xmppx/

type recorder struct {
	mu       sync.Mutex
	messages []*Message
	presence []*Presence
	notify   chan struct{}
}

func newRecorder() *recorder { return &recorder{notify: make(chan struct{}, 100)} }

func (r *recorder) HandleMessage(_ *Session, m *Message) {
	r.mu.Lock()
	r.messages = append(r.messages, m)
	r.mu.Unlock()
	r.notify <- struct{}{}
}

func (r *recorder) HandlePresence(_ *Session, p *Presence) {
	r.mu.Lock()
	r.presence = append(r.presence, p)
	r.mu.Unlock()
}

func (r *recorder) HandleRosterPush(*Session, []RosterItem) {}

func (r *recorder) waitMessage(t *testing.T, pred func(*Message) bool) *Message {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		r.mu.Lock()
		for _, m := range r.messages {
			if pred(m) {
				r.mu.Unlock()
				return m
			}
		}
		r.mu.Unlock()
		select {
		case <-r.notify:
		case <-deadline:
			t.Fatal("timed out waiting for message")
		}
	}
}

func liveConfig(t *testing.T, user string) *Config {
	t.Helper()
	domain := os.Getenv("XMPPX_TEST_DOMAIN")
	if domain == "" {
		t.Skip("XMPPX_TEST_DOMAIN not set")
	}
	var tlsCfg *tls.Config
	if caFile := os.Getenv("XMPPX_TEST_CA"); caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			t.Fatal(err)
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(pem)
		tlsCfg = &tls.Config{RootCAs: pool}
	}
	return &Config{
		JID:            jid.MustParse(user + "@" + domain),
		Password:       user + "-password",
		Resource:       "test-" + RandomID()[:6],
		Server:         os.Getenv("XMPPX_TEST_SERVER"),
		TLSConfig:      tlsCfg,
		ConnectTimeout: 10 * time.Second,
		SoftwareName:   "xmppx-test",
	}
}

func testLogger(t *testing.T) zerolog.Logger {
	return zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.DebugLevel)
}

func TestLiveConnectAndMessage(t *testing.T) {
	ctx := context.Background()
	aliceRec, bobRec := newRecorder(), newRecorder()
	alice, err := Connect(ctx, liveConfig(t, "alice"), testLogger(t), aliceRec, nil)
	if err != nil {
		t.Fatalf("alice connect: %v", err)
	}
	defer alice.Close()
	bob, err := Connect(ctx, liveConfig(t, "bob"), testLogger(t), bobRec, nil)
	if err != nil {
		t.Fatalf("bob connect: %v", err)
	}
	defer bob.Close()
	if alice.JID().Resourcepart() != liveResource(alice) {
		t.Errorf("bound %s, expected the requested resource", alice.JID())
	}
	if !alice.StreamManagementEnabled() {
		t.Error("expected stream management to be enabled")
	}
	if err := alice.Ping(ctx); err != nil {
		t.Errorf("ping: %v", err)
	}
	info, err := alice.DiscoInfo(ctx, "")
	if err != nil {
		t.Fatalf("disco: %v", err)
	}
	if !info.HasFeature(NSMAM) {
		t.Errorf("expected MAM on account, got %+v", info.Features)
	}
	if _, err := alice.Roster(ctx); err != nil {
		t.Errorf("roster: %v", err)
	}
	for _, s := range []*Session{alice, bob} {
		if err := s.SendPresence(ctx, &Presence{}); err != nil {
			t.Fatal(err)
		}
	}

	msg := &Message{To: bob.JID().Bare().String(), Type: MessageChat, Body: "hello bob", Markable: &Empty{}}
	if err := alice.SendMessage(ctx, msg); err != nil {
		t.Fatal(err)
	}
	ackCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if !alice.WaitAck(ackCtx, msg.ID) {
		t.Error("message was not acknowledged")
	}
	cancel()
	got := bobRec.waitMessage(t, func(m *Message) bool { return m.Body == "hello bob" })
	if got.OriginID == nil || got.OriginID.ID != msg.ID || got.Markable == nil {
		t.Errorf("unexpected message %+v", got)
	}
	if len(got.StanzaIDs) == 0 {
		t.Error("expected the server to add a stanza-id")
	}
	if alice.PendingAcks() != 0 {
		t.Errorf("pending acks = %d", alice.PendingAcks())
	}

	bad := liveConfig(t, "alice")
	bad.Password = "wrong"
	_, err = Connect(ctx, bad, testLogger(t), newRecorder(), nil)
	var ae *AuthError
	if !errors.As(err, &ae) || !ae.Permanent() {
		t.Errorf("expected permanent auth error, got %T %v", err, err)
	}

	untrusted := liveConfig(t, "alice")
	untrusted.TLSConfig = nil
	_, err = Connect(ctx, untrusted, testLogger(t), newRecorder(), nil)
	var te *TLSError
	if !errors.As(err, &te) {
		t.Errorf("expected TLS error with untrusted CA, got %T %v", err, err)
	}
}

func TestLiveMUCMAMBookmarksUpload(t *testing.T) {
	ctx := context.Background()
	aliceRec, bobRec := newRecorder(), newRecorder()
	alice, err := Connect(ctx, liveConfig(t, "alice"), testLogger(t), aliceRec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	bob, err := Connect(ctx, liveConfig(t, "bob"), testLogger(t), bobRec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()
	_ = alice.SendPresence(ctx, &Presence{})

	room := jid.MustParse("live-" + RandomID()[:8] + "@conference." + alice.JID().Domainpart())
	if err := alice.PublishBookmark(ctx, Bookmark{JID: room, Name: "Live test", Nick: "Alice", Autojoin: true}); err != nil {
		t.Fatalf("publish bookmark: %v", err)
	}
	bms, err := alice.Bookmarks(ctx)
	if err != nil {
		t.Fatalf("bookmarks: %v", err)
	}
	found := false
	for _, b := range bms {
		if b.JID.Equal(room) && b.Autojoin && b.Nick == "Alice" && b.Name == "Live test" {
			found = true
		}
	}
	if !found {
		t.Errorf("bookmark not found in %+v", bms)
	}

	if _, err := alice.JoinMUC(ctx, room, "Alice", ""); err != nil {
		t.Fatalf("alice join: %v", err)
	}
	res, err := bob.JoinMUC(ctx, room, "Bob", "")
	if err != nil {
		t.Fatalf("bob join: %v", err)
	}
	if res.Occupant.Resourcepart() != "Bob" {
		t.Errorf("occupant = %s", res.Occupant)
	}
	for i := range 3 {
		if err := bob.SendMessage(ctx, &Message{To: room.String(), Type: MessageGroupchat, Body: fmt.Sprintf("muc %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	got := aliceRec.waitMessage(t, func(m *Message) bool { return m.Body == "muc 2" })
	if got.Type != MessageGroupchat || len(got.StanzaIDs) == 0 || got.StanzaIDs[0].By != room.String() {
		t.Errorf("unexpected groupchat message %+v", got)
	}

	info, err := alice.DiscoInfo(ctx, room.String())
	if err != nil || !info.HasFeature(NSMAM) {
		t.Fatalf("room disco: %v %+v", err, info)
	}
	page, err := alice.QueryMAM(ctx, MAMQuery{Archive: room.String(), BeforeLast: true, Max: 2})
	if err != nil {
		t.Fatalf("muc mam: %v", err)
	}
	if len(page.Messages) != 2 || page.Messages[1].Message.Body != "muc 2" || page.Complete {
		t.Fatalf("unexpected page %+v", page)
	}
	older, err := alice.QueryMAM(ctx, MAMQuery{Archive: room.String(), Before: page.First, Max: 10})
	if err != nil || len(older.Messages) != 1 || older.Messages[0].Message.Body != "muc 0" || !older.Complete {
		t.Fatalf("unexpected older page %+v %v", older, err)
	}

	// Own archive filtered by conversation partner.
	if err := bob.SendMessage(ctx, &Message{To: alice.JID().Bare().String(), Type: MessageChat, Body: "for the archive"}); err != nil {
		t.Fatal(err)
	}
	aliceRec.waitMessage(t, func(m *Message) bool { return m.Body == "for the archive" })
	own, err := alice.QueryMAM(ctx, MAMQuery{With: bob.JID().Bare().String(), BeforeLast: true, Max: 5})
	if err != nil || len(own.Messages) == 0 || own.Messages[len(own.Messages)-1].Message.Body != "for the archive" {
		t.Fatalf("unexpected own archive page %+v %v", own, err)
	}
	if own.Messages[0].Timestamp.IsZero() {
		t.Error("expected archive timestamps")
	}

	svc, err := alice.FindUploadService(ctx)
	if err != nil || svc == nil {
		t.Fatalf("upload service: %v %v", svc, err)
	}
	if svc.MaxFileSize != 10*1024*1024 {
		t.Errorf("max file size = %d", svc.MaxFileSize)
	}
	slot, err := alice.RequestUploadSlot(ctx, svc.JID, "hello.txt", 5, "text/plain")
	if err != nil || slot.Put.URL == "" || slot.Get.URL == "" {
		t.Fatalf("slot: %+v %v", slot, err)
	}
}

func TestLiveResendUnacked(t *testing.T) {
	ctx := context.Background()
	bobRec := newRecorder()
	bob, err := Connect(ctx, liveConfig(t, "bob"), testLogger(t), bobRec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()
	_ = bob.SendPresence(ctx, &Presence{})

	// Pretend a previous session left an unacknowledged message behind.
	raw := []byte(`<message xmlns="jabber:client" type="chat" id="resend-1" to="` + bob.JID().Bare().String() + `"><body>resent</body><origin-id xmlns="urn:xmpp:sid:0" id="resend-1"></origin-id></message>`)
	alice, err := Connect(ctx, liveConfig(t, "alice"), testLogger(t), newRecorder(), [][]byte{raw})
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	got := bobRec.waitMessage(t, func(m *Message) bool { return m.Body == "resent" })
	if got.ID != "resend-1" {
		t.Errorf("id = %q", got.ID)
	}
	deadline := time.Now().Add(5 * time.Second)
	for alice.PendingAcks() != 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := alice.PendingAcks(); n != 0 {
		t.Errorf("pending acks = %d", n)
	}
}

func liveResource(s *Session) string { return s.cfg.Resource }
