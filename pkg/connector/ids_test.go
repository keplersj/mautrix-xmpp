package connector

import (
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2/networkid"
	"mellium.im/xmpp/jid"
)

func TestPortalKeys(t *testing.T) {
	login := makeUserLoginID(jid.MustParse("Alice@Example.ORG/phone"))
	if login != "alice@example.org" {
		t.Fatalf("login ID = %q", login)
	}
	dm := makeDMPortalKey(login, jid.MustParse("bob@example.net/laptop"))
	if dm.ID != "dm:bob@example.net" || dm.Receiver != login {
		t.Fatalf("dm key = %+v", dm)
	}
	j, isMUC, err := parsePortalID(dm.ID)
	if err != nil || isMUC || j.String() != "bob@example.net" {
		t.Fatalf("parse dm = %v %v %v", j, isMUC, err)
	}
	muc := makeMUCPortalKey(login, jid.MustParse("Dev@Conference.Example.net/Nick"))
	if muc.ID != "muc:dev@conference.example.net" {
		t.Fatalf("muc key = %+v", muc)
	}
	j, isMUC, err = parsePortalID(muc.ID)
	if err != nil || !isMUC || j.String() != "dev@conference.example.net" {
		t.Fatalf("parse muc = %v %v %v", j, isMUC, err)
	}
	if _, _, err = parsePortalID("telegram:123"); err == nil {
		t.Fatal("expected error for unknown portal ID")
	}
}

func TestUserIDs(t *testing.T) {
	u := makeUserID(jid.MustParse("bob@example.net/res"))
	if u != "bob@example.net" {
		t.Fatalf("user ID = %q", u)
	}
	j, occ, err := parseUserID(u)
	if err != nil || occ || !j.Equal(jid.MustParse("bob@example.net")) {
		t.Fatalf("parse = %v %v %v", j, occ, err)
	}
	o := makeOccupantUserID(jid.MustParse("dev@conference.example.net/Bob Smith"))
	if o != "occ:dev@conference.example.net/Bob Smith" {
		t.Fatalf("occupant ID = %q", o)
	}
	j, occ, err = parseUserID(o)
	if err != nil || !occ || j.Resourcepart() != "Bob Smith" {
		t.Fatalf("parse occupant = %v %v %v", j, occ, err)
	}
	if _, _, err = parseUserID("bob@example.net/res"); err == nil {
		t.Fatal("expected error for resource in plain user ID")
	}
}

func TestMessageIDs(t *testing.T) {
	id := makeMessageID("muc:dev@conference.example.net", "abc|def")
	p, x, ok := parseMessageID(id)
	if !ok || p != "muc:dev@conference.example.net" || x != "abc|def" {
		t.Fatalf("parse = %q %q %v", p, x, ok)
	}
	if _, _, ok = parseMessageID(networkid.MessageID("nope")); ok {
		t.Fatal("expected failure")
	}
	ts := time.Unix(1700000000, 0)
	a := syntheticMessageID("bob@example.net", ts, "hi")
	if a != syntheticMessageID("bob@example.net", ts, "hi") || a == syntheticMessageID("bob@example.net", ts, "hello") {
		t.Fatal("synthetic IDs must be deterministic and content-dependent")
	}
}
