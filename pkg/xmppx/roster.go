package xmppx

import (
	"context"
	"encoding/xml"
)

// RosterItem is an RFC 6121 roster item.
type RosterItem struct {
	JID          string   `xml:"jid,attr"`
	Name         string   `xml:"name,attr,omitempty"`
	Subscription string   `xml:"subscription,attr,omitempty"`
	Ask          string   `xml:"ask,attr,omitempty"`
	Groups       []string `xml:"group"`
}

type rosterQuery struct {
	XMLName xml.Name     `xml:"jabber:iq:roster query"`
	Items   []RosterItem `xml:"item"`
}

// Roster fetches the account's roster.
func (s *Session) Roster(ctx context.Context) ([]RosterItem, error) {
	var res rosterQuery
	if err := s.IQ(ctx, "get", "", rosterQuery{}, &res); err != nil {
		return nil, err
	}
	return res.Items, nil
}

// VCard is the subset of XEP-0054 vcard-temp the bridge uses.
type VCard struct {
	XMLName  xml.Name `xml:"vcard-temp vCard"`
	FullName string   `xml:"FN,omitempty"`
	Nickname string   `xml:"NICKNAME,omitempty"`
	Photo    *struct {
		Type   string `xml:"TYPE"`
		BinVal string `xml:"BINVAL"`
		ExtVal string `xml:"EXTVAL"`
	} `xml:"PHOTO"`
}

// GetVCard fetches an entity's vCard. A missing vCard is returned as an empty
// one.
func (s *Session) GetVCard(ctx context.Context, to string) (*VCard, error) {
	var res VCard
	err := s.IQ(ctx, "get", to, struct {
		XMLName xml.Name `xml:"vcard-temp vCard"`
	}{}, &res)
	if err != nil {
		if IsStanzaError(err, "item-not-found") || IsStanzaError(err, "service-unavailable") || IsStanzaError(err, "feature-not-implemented") {
			return &VCard{}, nil
		}
		return nil, err
	}
	return &res, nil
}

// EnableCarbons turns on XEP-0280 message carbons.
func (s *Session) EnableCarbons(ctx context.Context) error {
	return s.IQ(ctx, "set", "", struct {
		XMLName xml.Name `xml:"urn:xmpp:carbons:2 enable"`
	}{}, nil)
}
