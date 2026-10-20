package xmppx

import (
	"context"
	"encoding/xml"
	"slices"
)

// DiscoIdentity is a XEP-0030 identity.
type DiscoIdentity struct {
	Category string `xml:"category,attr"`
	Type     string `xml:"type,attr"`
	Name     string `xml:"name,attr,omitempty"`
}

// DiscoFeature is a XEP-0030 feature.
type DiscoFeature struct {
	Var string `xml:"var,attr"`
}

// DiscoInfo is the result of a disco#info query.
type DiscoInfo struct {
	XMLName    xml.Name        `xml:"http://jabber.org/protocol/disco#info query"`
	Identities []DiscoIdentity `xml:"identity"`
	Features   []DiscoFeature  `xml:"feature"`
	Forms      []DataForm      `xml:"jabber:x:data x"`
}

// HasFeature reports whether the entity advertises the feature.
func (d *DiscoInfo) HasFeature(v string) bool {
	if d == nil {
		return false
	}
	return slices.ContainsFunc(d.Features, func(f DiscoFeature) bool { return f.Var == v })
}

// HasIdentity reports whether the entity has an identity of the given category
// and type.
func (d *DiscoInfo) HasIdentity(category, typ string) bool {
	if d == nil {
		return false
	}
	return slices.ContainsFunc(d.Identities, func(i DiscoIdentity) bool { return i.Category == category && i.Type == typ })
}

// IdentityName returns the name of the first identity of the given category.
func (d *DiscoInfo) IdentityName(category string) string {
	if d == nil {
		return ""
	}
	for _, i := range d.Identities {
		if i.Category == category && i.Name != "" {
			return i.Name
		}
	}
	return ""
}

// FormValue returns a field value from the extended info form with the given
// FORM_TYPE (XEP-0128).
func (d *DiscoInfo) FormValue(formType, field string) string {
	if d == nil {
		return ""
	}
	for _, f := range d.Forms {
		if f.FormType() == formType {
			return f.Value(field)
		}
	}
	return ""
}

// DataForm is a XEP-0004 data form.
type DataForm struct {
	XMLName xml.Name    `xml:"jabber:x:data x"`
	Type    string      `xml:"type,attr"`
	Fields  []FormField `xml:"field"`
}

// FormField is a data form field.
type FormField struct {
	Var    string   `xml:"var,attr,omitempty"`
	Type   string   `xml:"type,attr,omitempty"`
	Values []string `xml:"value"`
}

// FormType returns the hidden FORM_TYPE value.
func (f *DataForm) FormType() string { return f.Value("FORM_TYPE") }

// Value returns the first value of a field.
func (f *DataForm) Value(v string) string {
	for _, field := range f.Fields {
		if field.Var == v && len(field.Values) > 0 {
			return field.Values[0]
		}
	}
	return ""
}

type discoInfoQuery struct {
	XMLName xml.Name `xml:"http://jabber.org/protocol/disco#info query"`
	Node    string   `xml:"node,attr,omitempty"`
}

// DiscoItem is a XEP-0030 item.
type DiscoItem struct {
	JID  string `xml:"jid,attr"`
	Node string `xml:"node,attr,omitempty"`
	Name string `xml:"name,attr,omitempty"`
}

type discoItemsQuery struct {
	XMLName xml.Name    `xml:"http://jabber.org/protocol/disco#items query"`
	Node    string      `xml:"node,attr,omitempty"`
	Items   []DiscoItem `xml:"item,omitempty"`
}

// DiscoInfo queries an entity's identities and features. An empty to queries
// our own account.
func (s *Session) DiscoInfo(ctx context.Context, to string) (*DiscoInfo, error) {
	var info DiscoInfo
	if err := s.IQ(ctx, "get", to, discoInfoQuery{}, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// DiscoItems lists an entity's items.
func (s *Session) DiscoItems(ctx context.Context, to string) ([]DiscoItem, error) {
	var res discoItemsQuery
	if err := s.IQ(ctx, "get", to, discoItemsQuery{}, &res); err != nil {
		return nil, err
	}
	return res.Items, nil
}
