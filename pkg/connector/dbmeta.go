package connector

import (
	"time"

	"maunium.net/go/mautrix/bridgev2/database"
)

// UserLoginMetadata is stored per XMPP account in the bridge database.
type UserLoginMetadata struct {
	// Password is the account password. It is only ever sent to the XMPP
	// server during SASL and is never logged.
	Password string `json:"password"`
	// Server optionally overrides SRV discovery (host:port or tls://host:port).
	Server string `json:"server,omitempty"`
	// Proxy optionally overrides the connector-wide SOCKS5 proxy.
	Proxy string `json:"proxy,omitempty"`
	// Resource is the stable resource the bridge binds for this login.
	Resource string `json:"resource"`
	// LastSeen is the timestamp of the newest message seen, used as the start
	// of the MAM catch-up query after a reconnect.
	LastSeen time.Time `json:"last_seen,omitempty"`
}

// PortalMetadata is stored per bridged chat.
type PortalMetadata struct {
	// Nick is our nickname in a group chat.
	Nick string `json:"nick,omitempty"`
	// Password is the room password from the bookmark, if any.
	Password string `json:"password,omitempty"`
	// LastArchiveID is the newest room archive ID seen, used for MUC catch-up.
	LastArchiveID string `json:"last_archive_id,omitempty"`
}

// MessageMetadata is stored per bridged message.
type MessageMetadata struct {
	// XMPPID is the sender-assigned ID (origin-id or id attribute), used for
	// corrections, 1:1 references and markers.
	XMPPID string `json:"xmpp_id,omitempty"`
	// ArchiveID is the stanza-id assigned by the archive: the room for group
	// chats, our own server for 1:1 chats. Group chat references use it.
	ArchiveID string `json:"archive_id,omitempty"`
	// SenderJID is the author's JID as XEP-0461 replies want it: the bare JID
	// in 1:1 chats, the occupant JID in group chats.
	SenderJID string `json:"sender_jid,omitempty"`
}

var _ database.MetaMerger = (*MessageMetadata)(nil)

// CopyFrom implements database.MetaMerger, used when an edit updates a part.
func (m *MessageMetadata) CopyFrom(other any) {
	o, ok := other.(*MessageMetadata)
	if !ok || o == nil {
		return
	}
	if o.XMPPID != "" {
		m.XMPPID = o.XMPPID
	}
	if o.ArchiveID != "" {
		m.ArchiveID = o.ArchiveID
	}
	if o.SenderJID != "" {
		m.SenderJID = o.SenderJID
	}
}

// GhostMetadata is stored per ghost user.
type GhostMetadata struct {
	// AvatarHash is the SHA-1 of the vCard photo last applied.
	AvatarHash string `json:"avatar_hash,omitempty"`
}
