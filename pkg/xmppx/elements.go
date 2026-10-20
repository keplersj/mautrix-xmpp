// Package xmppx is the XMPP client layer of mautrix-xmpp. It wraps
// mellium.im/xmpp for dialing, TLS, SASL and IQ correlation, and adds the
// pieces mellium does not have: XEP-0198 stream management, a reconnecting
// connection manager and typed structs for the messaging extensions the bridge
// uses.
package xmppx

import (
	"encoding/xml"
	"time"
)

// Namespaces of the extensions the bridge reads or writes.
const (
	NSClient      = "jabber:client"
	NSStreamMgmt  = "urn:xmpp:sm:3"
	NSStanzaID    = "urn:xmpp:sid:0"
	NSCorrect     = "urn:xmpp:message-correct:0"
	NSRetract     = "urn:xmpp:message-retract:1"
	NSRetract0    = "urn:xmpp:message-retract:0"
	NSFasten      = "urn:xmpp:fasten:0"
	NSReply       = "urn:xmpp:reply:0"
	NSFallback    = "urn:xmpp:fallback:0"
	NSReactions   = "urn:xmpp:reactions:0"
	NSMarkers     = "urn:xmpp:chat-markers:0"
	NSReceipts    = "urn:xmpp:receipts"
	NSChatStates  = "http://jabber.org/protocol/chatstates"
	NSOOB         = "jabber:x:oob"
	NSDelay       = "urn:xmpp:delay"
	NSForward     = "urn:xmpp:forward:0"
	NSCarbons     = "urn:xmpp:carbons:2"
	NSMAM         = "urn:xmpp:mam:2"
	NSRSM         = "http://jabber.org/protocol/rsm"
	NSDataForm    = "jabber:x:data"
	NSMUC         = "http://jabber.org/protocol/muc"
	NSMUCUser     = "http://jabber.org/protocol/muc#user"
	NSOccupantID  = "urn:xmpp:occupant-id:0"
	NSHints       = "urn:xmpp:hints"
	NSOMEMO       = "eu.siacs.conversations.axolotl"
	NSOMEMO2      = "urn:xmpp:omemo:2"
	NSOpenPGP     = "urn:xmpp:openpgp:0"
	NSDiscoInfo   = "http://jabber.org/protocol/disco#info"
	NSDiscoItems  = "http://jabber.org/protocol/disco#items"
	NSRoster      = "jabber:iq:roster"
	NSVCard       = "vcard-temp"
	NSPing        = "urn:xmpp:ping"
	NSUpload      = "urn:xmpp:http:upload:0"
	NSBookmarks2  = "urn:xmpp:bookmarks:1"
	NSBookmarks1  = "storage:bookmarks"
	NSPrivate     = "jabber:iq:private"
	NSPubSub      = "http://jabber.org/protocol/pubsub"
	NSStanzaError = "urn:ietf:params:xml:ns:xmpp-stanzas"
	NSVersion     = "jabber:iq:version"
)

// Message types (RFC 6121 §5.2.2).
const (
	MessageChat      = "chat"
	MessageGroupchat = "groupchat"
	MessageNormal    = "normal"
	MessageHeadline  = "headline"
	MessageError     = "error"
)

// Empty is an element with no attributes or content, like <markable/>.
type Empty struct{}

// Message is a <message/> stanza with every extension the bridge understands
// decoded in place.
type Message struct {
	XMLName xml.Name `xml:"jabber:client message"`
	ID      string   `xml:"id,attr,omitempty"`
	Type    string   `xml:"type,attr,omitempty"`
	From    string   `xml:"from,attr,omitempty"`
	To      string   `xml:"to,attr,omitempty"`

	Body    string       `xml:"body,omitempty"`
	Subject *string      `xml:"subject"`
	Error   *StanzaError `xml:"error"`

	OriginID  *OriginID  `xml:"urn:xmpp:sid:0 origin-id"`
	StanzaIDs []StanzaID `xml:"urn:xmpp:sid:0 stanza-id"`

	Replace   *Replace   `xml:"urn:xmpp:message-correct:0 replace"`
	Retract   *Retract   `xml:"urn:xmpp:message-retract:1 retract"`
	ApplyTo   *ApplyTo   `xml:"urn:xmpp:fasten:0 apply-to"`
	Retracted *Retracted `xml:"urn:xmpp:message-retract:1 retracted"`
	Reply     *Reply     `xml:"urn:xmpp:reply:0 reply"`
	Fallbacks []Fallback `xml:"urn:xmpp:fallback:0 fallback"`
	Reactions *Reactions `xml:"urn:xmpp:reactions:0 reactions"`

	Markable  *Empty  `xml:"urn:xmpp:chat-markers:0 markable"`
	Displayed *Marker `xml:"urn:xmpp:chat-markers:0 displayed"`

	ReceiptRequest  *Empty   `xml:"urn:xmpp:receipts request"`
	ReceiptReceived *Receipt `xml:"urn:xmpp:receipts received"`

	Active    *Empty `xml:"http://jabber.org/protocol/chatstates active"`
	Composing *Empty `xml:"http://jabber.org/protocol/chatstates composing"`
	Paused    *Empty `xml:"http://jabber.org/protocol/chatstates paused"`
	Inactive  *Empty `xml:"http://jabber.org/protocol/chatstates inactive"`
	Gone      *Empty `xml:"http://jabber.org/protocol/chatstates gone"`

	OOB []OOB `xml:"jabber:x:oob x"`

	Delay *Delay `xml:"urn:xmpp:delay delay"`

	CarbonSent     *Carbon    `xml:"urn:xmpp:carbons:2 sent"`
	CarbonReceived *Carbon    `xml:"urn:xmpp:carbons:2 received"`
	CarbonPrivate  *Empty     `xml:"urn:xmpp:carbons:2 private"`
	MAMResult      *MAMResult `xml:"urn:xmpp:mam:2 result"`

	MUCUser    *MUCUser    `xml:"http://jabber.org/protocol/muc#user x"`
	OccupantID *OccupantID `xml:"urn:xmpp:occupant-id:0 occupant-id"`

	Store   *Empty `xml:"urn:xmpp:hints store"`
	NoStore *Empty `xml:"urn:xmpp:hints no-store"`

	OMEMO   *Empty `xml:"eu.siacs.conversations.axolotl encrypted"`
	OMEMO2  *Empty `xml:"urn:xmpp:omemo:2 encrypted"`
	OpenPGP *Empty `xml:"urn:xmpp:openpgp:0 openpgp"`
}

// OriginID is XEP-0359 <origin-id/>.
type OriginID struct {
	ID string `xml:"id,attr"`
}

// StanzaID is XEP-0359 <stanza-id/>, assigned by the archiving entity.
type StanzaID struct {
	ID string `xml:"id,attr"`
	By string `xml:"by,attr"`
}

// Replace is XEP-0308 <replace/>.
type Replace struct {
	ID string `xml:"id,attr"`
}

// Retract is XEP-0424 v1 <retract/>.
type Retract struct {
	ID string `xml:"id,attr"`
}

// Retracted is the XEP-0424 tombstone that archives put in place of a
// retracted message.
type Retracted struct {
	Stamp string `xml:"stamp,attr,omitempty"`
}

// ApplyTo is XEP-0422 <apply-to/>, used by XEP-0424 v0 retractions.
type ApplyTo struct {
	ID      string `xml:"id,attr"`
	Retract *Empty `xml:"urn:xmpp:message-retract:0 retract"`
}

// Reply is XEP-0461 <reply/>.
type Reply struct {
	To string `xml:"to,attr,omitempty"`
	ID string `xml:"id,attr"`
}

// Fallback is XEP-0428 <fallback/>.
type Fallback struct {
	For    string          `xml:"for,attr,omitempty"`
	Bodies []FallbackRange `xml:"urn:xmpp:fallback:0 body"`
}

// FallbackRange is a range of the message body that is fallback text. Offsets
// count Unicode code points.
type FallbackRange struct {
	Start *int `xml:"start,attr"`
	End   *int `xml:"end,attr"`
}

// Reactions is XEP-0444 <reactions/>: the complete set of the sender's
// reactions to the message with the given ID.
type Reactions struct {
	ID        string   `xml:"id,attr"`
	Reactions []string `xml:"urn:xmpp:reactions:0 reaction"`
}

// Marker is a XEP-0333 chat marker.
type Marker struct {
	ID string `xml:"id,attr"`
}

// Receipt is a XEP-0184 <received/>.
type Receipt struct {
	ID string `xml:"id,attr"`
}

// OOB is XEP-0066 <x xmlns='jabber:x:oob'/>.
type OOB struct {
	URL  string `xml:"jabber:x:oob url"`
	Desc string `xml:"jabber:x:oob desc,omitempty"`
}

// Delay is XEP-0203 <delay/>.
type Delay struct {
	Stamp time.Time `xml:"stamp,attr"`
	From  string    `xml:"from,attr,omitempty"`
}

// Forwarded is XEP-0297 <forwarded/>.
type Forwarded struct {
	Delay   *Delay   `xml:"urn:xmpp:delay delay"`
	Message *Message `xml:"jabber:client message"`
}

// Carbon is XEP-0280 <sent/> or <received/>.
type Carbon struct {
	Forwarded Forwarded `xml:"urn:xmpp:forward:0 forwarded"`
}

// MAMResult is a XEP-0313 <result/> wrapping one archived message.
type MAMResult struct {
	QueryID   string    `xml:"queryid,attr"`
	ID        string    `xml:"id,attr"`
	Forwarded Forwarded `xml:"urn:xmpp:forward:0 forwarded"`
}

// MUCUser is XEP-0045 <x xmlns='http://jabber.org/protocol/muc#user'/>.
type MUCUser struct {
	Items    []MUCItem   `xml:"http://jabber.org/protocol/muc#user item"`
	Statuses []MUCStatus `xml:"http://jabber.org/protocol/muc#user status"`
	Invite   *MUCInvite  `xml:"http://jabber.org/protocol/muc#user invite"`
}

// HasStatus reports whether the given status code is present.
func (u *MUCUser) HasStatus(code int) bool {
	if u == nil {
		return false
	}
	for _, s := range u.Statuses {
		if s.Code == code {
			return true
		}
	}
	return false
}

// MUCItem is an occupant item.
type MUCItem struct {
	Affiliation string `xml:"affiliation,attr,omitempty"`
	Role        string `xml:"role,attr,omitempty"`
	JID         string `xml:"jid,attr,omitempty"`
	Nick        string `xml:"nick,attr,omitempty"`
}

// MUCStatus is a status code.
type MUCStatus struct {
	Code int `xml:"code,attr"`
}

// MUCInvite is a mediated invitation.
type MUCInvite struct {
	From   string `xml:"from,attr,omitempty"`
	Reason string `xml:"reason,omitempty"`
}

// OccupantID is XEP-0421 <occupant-id/>.
type OccupantID struct {
	ID string `xml:"id,attr"`
}

// StanzaError is an RFC 6120 stanza <error/>.
type StanzaError struct {
	Type      string    `xml:"type,attr,omitempty"`
	Condition xml.Name  `xml:"-"`
	Text      string    `xml:"-"`
	Inner     []anyElem `xml:",any"`
}

type anyElem struct {
	XMLName xml.Name
	Content string `xml:",chardata"`
}

// UnmarshalXML implements xml.Unmarshaler.
func (e *StanzaError) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	type raw StanzaError
	if err := d.DecodeElement((*raw)(e), &start); err != nil {
		return err
	}
	for _, el := range e.Inner {
		if el.XMLName.Space != NSStanzaError {
			continue
		}
		if el.XMLName.Local == "text" {
			e.Text = el.Content
		} else if e.Condition.Local == "" {
			e.Condition = el.XMLName
		}
	}
	return nil
}

func (e *StanzaError) Error() string {
	if e == nil {
		return "<nil>"
	}
	s := e.Condition.Local
	if s == "" {
		s = "undefined-condition"
	}
	if e.Text != "" {
		s += ": " + e.Text
	}
	return s
}

// Presence is a <presence/> stanza.
type Presence struct {
	XMLName  xml.Name `xml:"jabber:client presence"`
	ID       string   `xml:"id,attr,omitempty"`
	Type     string   `xml:"type,attr,omitempty"`
	From     string   `xml:"from,attr,omitempty"`
	To       string   `xml:"to,attr,omitempty"`
	Show     string   `xml:"show,omitempty"`
	Status   string   `xml:"status,omitempty"`
	Priority *int8    `xml:"priority,omitempty"`

	Error      *StanzaError `xml:"error"`
	MUC        *MUCJoin     `xml:"http://jabber.org/protocol/muc x"`
	MUCUser    *MUCUser     `xml:"http://jabber.org/protocol/muc#user x"`
	OccupantID *OccupantID  `xml:"urn:xmpp:occupant-id:0 occupant-id"`
	VCard      *VCardUpdate `xml:"vcard-temp:x:update x"`
}

// MUCJoin is the <x xmlns='http://jabber.org/protocol/muc'/> sent when joining.
type MUCJoin struct {
	Password string      `xml:"password,omitempty"`
	History  *MUCHistory `xml:"history"`
}

// MUCHistory limits the history the room sends on join.
type MUCHistory struct {
	MaxStanzas *int   `xml:"maxstanzas,attr,omitempty"`
	Since      string `xml:"since,attr,omitempty"`
}

// VCardUpdate is XEP-0153 vCard-based avatar announcement.
type VCardUpdate struct {
	Photo *string `xml:"photo"`
}
