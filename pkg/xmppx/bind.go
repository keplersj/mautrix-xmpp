package xmppx

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"

	"mellium.im/xmlstream"
	"mellium.im/xmpp"
	"mellium.im/xmpp/jid"
)

const nsBind = "urn:ietf:params:xml:ns:xmpp-bind"

// bindFeature is a client-only replacement for xmpp.BindResource. mellium
// v0.23.0 sends an empty <resource/> instead of the requested resource, so the
// server always picks a random one; the bridge wants a stable, recognizable
// resource per login.
func bindFeature(resource string) xmpp.StreamFeature {
	return xmpp.StreamFeature{
		Name:       xml.Name{Space: nsBind, Local: "bind"},
		Necessary:  xmpp.Authn,
		Prohibited: xmpp.Ready,
		Parse: func(ctx context.Context, d *xml.Decoder, start *xml.StartElement) (bool, interface{}, error) {
			return true, nil, d.Skip()
		},
		Negotiate: func(ctx context.Context, session *xmpp.Session, _ interface{}) (xmpp.SessionState, io.ReadWriter, error) {
			reqID := RandomID()
			req := struct {
				XMLName xml.Name `xml:"jabber:client iq"`
				ID      string   `xml:"id,attr"`
				Type    string   `xml:"type,attr"`
				Bind    struct {
					XMLName  xml.Name `xml:"urn:ietf:params:xml:ns:xmpp-bind bind"`
					Resource string   `xml:"resource,omitempty"`
				}
			}{ID: reqID, Type: "set"}
			req.Bind.Resource = resource

			raw, err := xml.Marshal(req)
			if err != nil {
				return 0, nil, err
			}
			w := session.TokenWriter()
			defer func() { _ = w.Close() }()
			if _, err = xmlstream.Copy(w, rawTokenReader{xml.NewDecoder(bytes.NewReader(raw))}); err != nil {
				return 0, nil, err
			}
			if err := w.Flush(); err != nil {
				return 0, nil, err
			}

			r := session.TokenReader()
			defer func() { _ = r.Close() }()
			var resp struct {
				XMLName xml.Name     `xml:"jabber:client iq"`
				ID      string       `xml:"id,attr"`
				Type    string       `xml:"type,attr"`
				JID     string       `xml:"urn:ietf:params:xml:ns:xmpp-bind bind>jid"`
				Error   *StanzaError `xml:"error"`
			}
			if err := xml.NewTokenDecoder(r).Decode(&resp); err != nil {
				return 0, nil, fmt.Errorf("failed to read bind response: %w", err)
			}
			switch {
			case resp.ID != reqID:
				return 0, nil, errors.New("unexpected bind response id")
			case resp.Type == "error":
				return 0, nil, fmt.Errorf("resource binding failed: %w", resp.Error)
			case resp.Type != "result":
				return 0, nil, fmt.Errorf("unexpected bind response type %q", resp.Type)
			}
			bound, err := jid.Parse(resp.JID)
			if err != nil {
				return 0, nil, fmt.Errorf("server bound an invalid JID: %w", err)
			}
			session.UpdateAddr(bound)
			return xmpp.Ready, nil, nil
		},
	}
}
