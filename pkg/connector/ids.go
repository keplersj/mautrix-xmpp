package connector

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2/networkid"
	"mellium.im/xmpp/jid"
)

const (
	dmPortalPrefix  = "dm:"
	mucPortalPrefix = "muc:"
	occupantPrefix  = "occ:"
)

// parseBareJID parses and normalizes a JID and strips the resource.
func parseBareJID(s string) (jid.JID, error) {
	j, err := jid.Parse(strings.TrimSpace(s))
	if err != nil {
		return jid.JID{}, err
	}
	return j.Bare(), nil
}

// makeUserLoginID returns the login ID for an account: its bare JID.
func makeUserLoginID(account jid.JID) networkid.UserLoginID {
	return networkid.UserLoginID(account.Bare().String())
}

// makeUserID returns the ghost ID for a user identified by a real JID.
func makeUserID(j jid.JID) networkid.UserID {
	return networkid.UserID(j.Bare().String())
}

// makeOccupantUserID returns the ghost ID for a MUC occupant whose real JID is
// not known: the occupant JID (room@service/nick) with a prefix.
func makeOccupantUserID(occupant jid.JID) networkid.UserID {
	return networkid.UserID(occupantPrefix + occupant.String())
}

// parseUserID returns the JID behind a ghost ID. For occupant ghosts it
// returns the occupant JID and isOccupant=true.
func parseUserID(id networkid.UserID) (j jid.JID, isOccupant bool, err error) {
	s := string(id)
	if strings.HasPrefix(s, occupantPrefix) {
		j, err = jid.Parse(strings.TrimPrefix(s, occupantPrefix))
		return j, true, err
	}
	j, err = jid.Parse(s)
	if err == nil && j.Resourcepart() != "" {
		err = fmt.Errorf("unexpected resource in user ID %q", s)
	}
	return j, false, err
}

// makeDMPortalKey returns the portal key of the 1:1 chat with a contact.
func makeDMPortalKey(login networkid.UserLoginID, contact jid.JID) networkid.PortalKey {
	return networkid.PortalKey{ID: networkid.PortalID(dmPortalPrefix + contact.Bare().String()), Receiver: login}
}

// makeMUCPortalKey returns the portal key of a group chat.
func makeMUCPortalKey(login networkid.UserLoginID, room jid.JID) networkid.PortalKey {
	return networkid.PortalKey{ID: networkid.PortalID(mucPortalPrefix + room.Bare().String()), Receiver: login}
}

// parsePortalID returns the chat JID and whether the portal is a group chat.
func parsePortalID(id networkid.PortalID) (j jid.JID, isMUC bool, err error) {
	s := string(id)
	switch {
	case strings.HasPrefix(s, dmPortalPrefix):
		j, err = parseBareJID(strings.TrimPrefix(s, dmPortalPrefix))
	case strings.HasPrefix(s, mucPortalPrefix):
		j, err = parseBareJID(strings.TrimPrefix(s, mucPortalPrefix))
		isMUC = true
	default:
		err = fmt.Errorf("unknown portal ID format %q", s)
	}
	return
}

// makeMessageID returns the bridged ID of a message: the portal ID plus the
// sender-assigned XMPP message ID.
func makeMessageID(portal networkid.PortalID, xmppID string) networkid.MessageID {
	return networkid.MessageID(string(portal) + "|" + xmppID)
}

// parseMessageID splits a message ID into the portal ID and XMPP message ID.
func parseMessageID(id networkid.MessageID) (portal networkid.PortalID, xmppID string, ok bool) {
	p, x, found := strings.Cut(string(id), "|")
	if !found || x == "" {
		return "", "", false
	}
	return networkid.PortalID(p), x, true
}

// syntheticMessageID returns a deterministic ID for messages that carry no ID
// at all, so the same message dedupes when seen again (for example via MAM).
func syntheticMessageID(sender string, ts time.Time, body string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%d\x00%s", sender, ts.Unix(), body)
	return "synthetic-" + base64.RawURLEncoding.EncodeToString(h.Sum(nil)[:15])
}
