//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"mellium.im/xmpp/jid"

	"github.com/keplersj/mautrix-xmpp/pkg/xmppx"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var (
	matrixURL    = env("MATRIX_URL", "http://localhost:8008")
	sharedSecret = env("MATRIX_SHARED_SECRET", "integration-registration-secret")
	xmppDomain   = env("XMPP_DOMAIN", "xmpp.test")
	mucDomain    = env("XMPP_MUC_DOMAIN", "conference.xmpp.test")
	matrixDomain = env("MATRIX_DOMAIN", "matrix.test")
)

const waitTimeout = 45 * time.Second

// ghostMXID returns the Matrix ID the bridge uses for a ghost.
func ghostMXID(userID string) id.UserID {
	return id.NewUserID("xmpp_"+id.EncodeUserLocalpart(userID), matrixDomain)
}

// --- Matrix side ---

type matrixUser struct {
	t      *testing.T
	client *mautrix.Client

	mu     sync.Mutex
	cond   *sync.Cond
	events []*event.Event
}

// registerMatrixUser creates a user with Synapse's shared-secret registration
// API and starts syncing.
func registerMatrixUser(t *testing.T, ctx context.Context, localpart string) *matrixUser {
	t.Helper()
	password := "pw-" + localpart
	var nonceResp struct {
		Nonce string `json:"nonce"`
	}
	resp, err := http.Get(matrixURL + "/_synapse/admin/v1/register")
	if err != nil {
		t.Fatalf("get registration nonce: %v", err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&nonceResp)
	_ = resp.Body.Close()
	mac := hmac.New(sha1.New, []byte(sharedSecret))
	mac.Write([]byte(nonceResp.Nonce + "\x00" + localpart + "\x00" + password + "\x00notadmin"))
	body, _ := json.Marshal(map[string]any{
		"nonce": nonceResp.Nonce, "username": localpart, "password": password,
		"admin": false, "mac": hex.EncodeToString(mac.Sum(nil)),
	})
	resp, err = http.Post(matrixURL+"/_synapse/admin/v1/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var reg struct {
		UserID      id.UserID `json:"user_id"`
		AccessToken string    `json:"access_token"`
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || json.Unmarshal(data, &reg) != nil {
		t.Fatalf("register failed: %s %s", resp.Status, data)
	}
	client, err := mautrix.NewClient(matrixURL, reg.UserID, reg.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	u := &matrixUser{t: t, client: client}
	u.cond = sync.NewCond(&u.mu)
	syncer := client.Syncer.(*mautrix.DefaultSyncer)
	syncer.OnEvent(func(ctx context.Context, evt *event.Event) {
		if evt.Type == event.StateMember && evt.GetStateKey() == string(reg.UserID) &&
			evt.Content.AsMember().Membership == event.MembershipInvite {
			go func() {
				if _, err := client.JoinRoomByID(context.Background(), evt.RoomID); err != nil {
					t.Logf("failed to join %s: %v", evt.RoomID, err)
				}
			}()
		}
		u.mu.Lock()
		u.events = append(u.events, evt)
		u.cond.Broadcast()
		u.mu.Unlock()
	})
	go func() {
		for ctx.Err() == nil {
			if err := client.SyncWithContext(ctx); err != nil && ctx.Err() == nil {
				t.Logf("sync error: %v", err)
				time.Sleep(time.Second)
			}
		}
	}()
	return u
}

// waitEvent waits for an event matching pred, including events that already
// arrived.
func (u *matrixUser) waitEvent(desc string, pred func(*event.Event) bool) *event.Event {
	u.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	timer := time.AfterFunc(waitTimeout, func() {
		u.mu.Lock()
		u.cond.Broadcast()
		u.mu.Unlock()
	})
	defer timer.Stop()
	u.mu.Lock()
	defer u.mu.Unlock()
	for {
		for _, evt := range u.events {
			if pred(evt) {
				return evt
			}
		}
		if time.Now().After(deadline) {
			u.t.Fatalf("timed out waiting for Matrix event: %s", desc)
		}
		u.cond.Wait()
	}
}

func (u *matrixUser) waitMessage(desc string, pred func(*event.Event, *event.MessageEventContent) bool) (*event.Event, *event.MessageEventContent) {
	u.t.Helper()
	evt := u.waitEvent(desc, func(evt *event.Event) bool {
		if evt.Type != event.EventMessage {
			return false
		}
		c, ok := evt.Content.Parsed.(*event.MessageEventContent)
		return ok && pred(evt, c)
	})
	return evt, evt.Content.Parsed.(*event.MessageEventContent)
}

func (u *matrixUser) waitBody(body string) *event.Event {
	u.t.Helper()
	evt, _ := u.waitMessage("message "+body, func(evt *event.Event, c *event.MessageEventContent) bool {
		return c.Body == body && (c.RelatesTo == nil || c.RelatesTo.Type != event.RelReplace)
	})
	return evt
}

func (u *matrixUser) send(ctx context.Context, room id.RoomID, content *event.MessageEventContent) id.EventID {
	u.t.Helper()
	resp, err := u.client.SendMessageEvent(ctx, room, event.EventMessage, content)
	if err != nil {
		u.t.Fatalf("send message: %v", err)
	}
	return resp.EventID
}

// --- XMPP side ---

type xmppUser struct {
	t       *testing.T
	session *xmppx.Session

	mu       sync.Mutex
	cond     *sync.Cond
	messages []*xmppx.Message
	presence []*xmppx.Presence
}

func (x *xmppUser) HandleMessage(_ *xmppx.Session, m *xmppx.Message) {
	x.mu.Lock()
	x.messages = append(x.messages, m)
	x.cond.Broadcast()
	x.mu.Unlock()
}

func (x *xmppUser) HandlePresence(_ *xmppx.Session, p *xmppx.Presence) {
	x.mu.Lock()
	x.presence = append(x.presence, p)
	x.cond.Broadcast()
	x.mu.Unlock()
}

func (x *xmppUser) HandleRosterPush(*xmppx.Session, []xmppx.RosterItem) {}

func connectXMPP(t *testing.T, ctx context.Context, user, resource string) *xmppUser {
	t.Helper()
	x := &xmppUser{t: t}
	x.cond = sync.NewCond(&x.mu)
	if resource == "" {
		resource = "e2e-" + xmppx.RandomID()[:6]
	}
	s, err := xmppx.Connect(ctx, &xmppx.Config{
		JID:            jid.MustParse(user + "@" + xmppDomain),
		Password:       user + "-password",
		Resource:       resource,
		ConnectTimeout: 15 * time.Second,
		SoftwareName:   "mautrix-xmpp-e2e",
	}, zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.InfoLevel), x, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", user, err)
	}
	x.session = s
	if err = s.SendPresence(ctx, &xmppx.Presence{}); err != nil {
		t.Fatal(err)
	}
	return x
}

func (x *xmppUser) waitMessage(desc string, pred func(*xmppx.Message) bool) *xmppx.Message {
	x.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	timer := time.AfterFunc(waitTimeout, func() {
		x.mu.Lock()
		x.cond.Broadcast()
		x.mu.Unlock()
	})
	defer timer.Stop()
	x.mu.Lock()
	defer x.mu.Unlock()
	for {
		for _, m := range x.messages {
			if pred(m) {
				return m
			}
		}
		if time.Now().After(deadline) {
			x.t.Fatalf("timed out waiting for XMPP message: %s", desc)
		}
		x.cond.Wait()
	}
}

func (x *xmppUser) waitPresence(desc string, pred func(*xmppx.Presence) bool) *xmppx.Presence {
	x.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	timer := time.AfterFunc(waitTimeout, func() {
		x.mu.Lock()
		x.cond.Broadcast()
		x.mu.Unlock()
	})
	defer timer.Stop()
	x.mu.Lock()
	defer x.mu.Unlock()
	for {
		for _, p := range x.presence {
			if pred(p) {
				return p
			}
		}
		if time.Now().After(deadline) {
			x.t.Fatalf("timed out waiting for presence: %s", desc)
		}
		x.cond.Wait()
	}
}

func (x *xmppUser) send(ctx context.Context, msg *xmppx.Message) *xmppx.Message {
	x.t.Helper()
	if err := x.session.SendMessage(ctx, msg); err != nil {
		x.t.Fatalf("send XMPP message: %v", err)
	}
	return msg
}

// uploadFile uploads data with XEP-0363 and returns the download URL.
func (x *xmppUser) uploadFile(ctx context.Context, name, mimeType string, data []byte) string {
	x.t.Helper()
	svc, err := x.session.FindUploadService(ctx)
	if err != nil || svc == nil {
		x.t.Fatalf("no upload service: %v", err)
	}
	slot, err := x.session.RequestUploadSlot(ctx, svc.JID, name, int64(len(data)), mimeType)
	if err != nil {
		x.t.Fatalf("request slot: %v", err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, slot.Put.URL, bytes.NewReader(data))
	req.Header.Set("Content-Type", mimeType)
	for k, v := range slot.PutHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		x.t.Fatalf("upload: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		x.t.Fatalf("upload status %s", resp.Status)
	}
	return slot.Get.URL
}

func httpGet(t *testing.T, ctx context.Context, url string) []byte {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s", url, resp.Status)
	}
	return data
}

func uniq(prefix string) string {
	return fmt.Sprintf("%s %s", prefix, strings.ToUpper(xmppx.RandomID()[:6]))
}
