package xmppx

import (
	"strings"
	"testing"
)

func scanAll(t *testing.T, capture bool, chunks ...string) []*topLevelElement {
	t.Helper()
	var got []*topLevelElement
	s := newStanzaScanner(capture, func(e *topLevelElement) { got = append(got, e) })
	for _, c := range chunks {
		_, _ = s.Write([]byte(c))
	}
	return got
}

func names(els []*topLevelElement) string {
	var n []string
	for _, e := range els {
		n = append(n, e.Name)
	}
	return strings.Join(n, ",")
}

func TestScannerStreamRestartsAndNesting(t *testing.T) {
	stream := `<?xml version='1.0'?><stream:stream xmlns='jabber:client' xmlns:stream='http://etherx.jabber.org/streams' version='1.0'>` +
		`<stream:features><starttls xmlns='urn:ietf:params:xml:ns:xmpp-tls'><required/></starttls></stream:features>` +
		`<proceed xmlns='urn:ietf:params:xml:ns:xmpp-tls'/>` +
		// TLS restart: a fresh header without closing the previous stream.
		`<?xml version='1.0'?><stream:stream xmlns='jabber:client' xmlns:stream='http://etherx.jabber.org/streams' version='1.0'>` +
		`<stream:features><sm xmlns='urn:xmpp:sm:3'/></stream:features>` +
		`<iq type='result' id='b1'><bind xmlns='urn:ietf:params:xml:ns:xmpp-bind'><jid>a@b/c</jid></bind></iq>` +
		`<enabled xmlns='urn:xmpp:sm:3'/>` +
		`<message from='x@y' attr='a > b'><body>1 &gt; 0 &amp; "quotes" don't matter</body><reply xmlns='urn:xmpp:reply:0' id="q'q"/></message>` +
		"\n " + // whitespace keepalive
		`<presence from='x@y/r'/>` +
		`<r xmlns='urn:xmpp:sm:3'/>` +
		`<a xmlns='urn:xmpp:sm:3' h='42'/>` +
		`</stream:stream>`
	// Feed it one byte at a time to exercise chunk boundaries.
	var chunks []string
	for i := range stream {
		chunks = append(chunks, stream[i:i+1])
	}
	got := scanAll(t, true, chunks...)
	want := "features,proceed,features,iq,enabled,message,presence,r,a"
	if names(got) != want {
		t.Fatalf("got %s, want %s", names(got), want)
	}
	if h := got[8].Attr("h"); h != "42" {
		t.Errorf("h = %q", h)
	}
	if from := got[5].Attr("from"); from != "x@y" {
		t.Errorf("from = %q", from)
	}
	wantRaw := `<message from='x@y' attr='a > b'><body>1 &gt; 0 &amp; "quotes" don't matter</body><reply xmlns='urn:xmpp:reply:0' id="q'q"/></message>`
	if string(got[5].Raw) != wantRaw {
		t.Errorf("raw = %s", got[5].Raw)
	}
}

func TestScannerNoCapture(t *testing.T) {
	got := scanAll(t, false, `<stream:stream><message id='1'><body>x</body></message>`)
	if len(got) != 1 || got[0].Raw != nil || got[0].Attr("id") != "1" {
		t.Fatalf("unexpected %+v", got)
	}
}

func TestTagAttr(t *testing.T) {
	tag := []byte(`message xmlns="jabber:client" xid="no" id = "a&amp;b" to='c&apos;d'`)
	if v := tagAttr(tag, "id"); v != "a&b" {
		t.Errorf("id = %q", v)
	}
	if v := tagAttr(tag, "to"); v != "c'd" {
		t.Errorf("to = %q", v)
	}
	if v := tagAttr(tag, "type"); v != "" {
		t.Errorf("type = %q", v)
	}
}
