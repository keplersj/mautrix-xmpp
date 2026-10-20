package xmppx

import (
	"context"
	"encoding/xml"
	"strings"

	"mellium.im/xmpp/jid"
)

// Bookmark is a group chat bookmark from XEP-0402 or XEP-0048.
type Bookmark struct {
	JID      jid.JID
	Name     string
	Nick     string
	Password string
	Autojoin bool
}

type pubsubItemsRequest struct {
	XMLName xml.Name `xml:"http://jabber.org/protocol/pubsub pubsub"`
	Items   struct {
		Node string `xml:"node,attr"`
	} `xml:"items"`
}

type bookmarks2Conference struct {
	Name     string `xml:"name,attr"`
	Autojoin string `xml:"autojoin,attr"`
	Nick     string `xml:"nick"`
	Password string `xml:"password"`
}

type pubsubBookmarksResult struct {
	XMLName xml.Name `xml:"http://jabber.org/protocol/pubsub pubsub"`
	Items   []struct {
		ID         string               `xml:"id,attr"`
		Conference bookmarks2Conference `xml:"urn:xmpp:bookmarks:1 conference"`
	} `xml:"items>item"`
}

type privateStorage struct {
	XMLName xml.Name `xml:"jabber:iq:private query"`
	Storage struct {
		XMLName     xml.Name `xml:"storage:bookmarks storage"`
		Conferences []struct {
			JID      string `xml:"jid,attr"`
			Name     string `xml:"name,attr"`
			Autojoin string `xml:"autojoin,attr"`
			Nick     string `xml:"nick"`
			Password string `xml:"password"`
		} `xml:"conference"`
	}
}

func xmlBool(s string) bool {
	s = strings.TrimSpace(s)
	return s == "true" || s == "1"
}

// Bookmarks returns the account's group chat bookmarks. It reads XEP-0402 PEP
// bookmarks and the XEP-0048 private XML storage fallback, preferring XEP-0402
// entries when both list the same room.
func (s *Session) Bookmarks(ctx context.Context) ([]Bookmark, error) {
	var out []Bookmark
	seen := make(map[string]bool)

	var req pubsubItemsRequest
	req.Items.Node = NSBookmarks2
	var res pubsubBookmarksResult
	err402 := s.IQ(ctx, "get", "", req, &res)
	if err402 == nil {
		for _, item := range res.Items {
			j, err := jid.Parse(item.ID)
			if err != nil {
				continue
			}
			j = j.Bare()
			seen[j.String()] = true
			out = append(out, Bookmark{
				JID:      j,
				Name:     item.Conference.Name,
				Nick:     item.Conference.Nick,
				Password: item.Conference.Password,
				Autojoin: xmlBool(item.Conference.Autojoin),
			})
		}
	}

	var priv privateStorage
	err48 := s.IQ(ctx, "get", "", privateStorage{}, &priv)
	if err48 == nil {
		for _, c := range priv.Storage.Conferences {
			j, err := jid.Parse(c.JID)
			if err != nil {
				continue
			}
			j = j.Bare()
			if seen[j.String()] {
				continue
			}
			seen[j.String()] = true
			out = append(out, Bookmark{JID: j, Name: c.Name, Nick: c.Nick, Password: c.Password, Autojoin: xmlBool(c.Autojoin)})
		}
	}
	// "No bookmarks stored" is reported as item-not-found; only fail if both
	// mechanisms failed for another reason.
	if err402 != nil && err48 != nil && !IsStanzaError(err402, "item-not-found") && !IsStanzaError(err48, "item-not-found") {
		return nil, err402
	}
	return out, nil
}

type pubsubPublishBookmark struct {
	XMLName xml.Name `xml:"http://jabber.org/protocol/pubsub pubsub"`
	Publish struct {
		Node string `xml:"node,attr"`
		Item struct {
			ID         string `xml:"id,attr"`
			Conference struct {
				XMLName  xml.Name `xml:"urn:xmpp:bookmarks:1 conference"`
				Name     string   `xml:"name,attr,omitempty"`
				Autojoin bool     `xml:"autojoin,attr"`
				Nick     string   `xml:"nick,omitempty"`
			}
		} `xml:"item"`
	} `xml:"publish"`
	PublishOptions struct {
		Form DataForm `xml:"jabber:x:data x"`
	} `xml:"publish-options"`
}

// PublishBookmark stores a XEP-0402 bookmark. The bridge itself only reads
// bookmarks; this exists for tests and tooling.
func (s *Session) PublishBookmark(ctx context.Context, b Bookmark) error {
	var p pubsubPublishBookmark
	p.Publish.Node = NSBookmarks2
	p.Publish.Item.ID = b.JID.Bare().String()
	p.Publish.Item.Conference.Name = b.Name
	p.Publish.Item.Conference.Autojoin = b.Autojoin
	p.Publish.Item.Conference.Nick = b.Nick
	p.PublishOptions.Form = DataForm{Type: "submit", Fields: []FormField{
		{Var: "FORM_TYPE", Type: "hidden", Values: []string{"http://jabber.org/protocol/pubsub#publish-options"}},
		{Var: "pubsub#persist_items", Values: []string{"true"}},
		{Var: "pubsub#max_items", Values: []string{"max"}},
		{Var: "pubsub#send_last_published_item", Values: []string{"never"}},
		{Var: "pubsub#access_model", Values: []string{"whitelist"}},
	}}
	return s.IQ(ctx, "set", "", p, nil)
}
