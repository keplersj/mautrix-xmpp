package xmppx

import (
	"bytes"
	"html"
)

// topLevelElement is one complete child of <stream:stream>, as seen on the
// wire.
type topLevelElement struct {
	// Name is the local name, without any namespace prefix.
	Name string
	// StartTag is the raw start tag, without the angle brackets.
	StartTag []byte
	// Raw is the full serialized element. Only filled when capture is enabled.
	Raw []byte
}

// Attr returns the value of an attribute of the start tag. It is a minimal
// parser that is only used for simple attributes like id and h.
func (e *topLevelElement) Attr(name string) string {
	return tagAttr(e.StartTag, name)
}

func tagAttr(tag []byte, name string) string {
	i := 0
	for {
		idx := bytes.Index(tag[i:], []byte(name))
		if idx < 0 {
			return ""
		}
		idx += i
		i = idx + len(name)
		// Must be preceded by whitespace and followed by optional space and '='.
		if idx == 0 || !isXMLSpace(tag[idx-1]) {
			continue
		}
		j := i
		for j < len(tag) && isXMLSpace(tag[j]) {
			j++
		}
		if j >= len(tag) || tag[j] != '=' {
			continue
		}
		j++
		for j < len(tag) && isXMLSpace(tag[j]) {
			j++
		}
		if j >= len(tag) || (tag[j] != '"' && tag[j] != '\'') {
			continue
		}
		quote := tag[j]
		end := bytes.IndexByte(tag[j+1:], quote)
		if end < 0 {
			return ""
		}
		return unescapeAttr(string(tag[j+1 : j+1+end]))
	}
}

func unescapeAttr(s string) string {
	// The XML predefined entities and character references are a subset of
	// what html.UnescapeString understands.
	return html.UnescapeString(s)
}

func isXMLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// stanzaScanner reads a raw XMPP byte stream incrementally and reports every
// complete top-level element. It understands just enough XML for XMPP streams:
// tags, quoted attribute values, character data and processing instructions.
// Comments, CDATA sections and DTDs are forbidden in XMPP (RFC 6120 §11.1), so
// they are not handled. A <stream:stream> start tag (re)starts the stream, so
// the scanner follows stream restarts after STARTTLS and SASL.
type stanzaScanner struct {
	capture bool
	emit    func(*topLevelElement)

	depth  int
	inTag  bool
	quote  byte
	tag    []byte
	cur    *topLevelElement
	rawBuf bytes.Buffer
}

func newStanzaScanner(capture bool, emit func(*topLevelElement)) *stanzaScanner {
	return &stanzaScanner{capture: capture, emit: emit}
}

// Write implements io.Writer. It never returns an error, so it is safe to use
// as a tee target.
func (s *stanzaScanner) Write(p []byte) (int, error) {
	for _, c := range p {
		s.feed(c)
	}
	return len(p), nil
}

func (s *stanzaScanner) feed(c byte) {
	if !s.inTag {
		if c == '<' {
			s.inTag = true
			s.quote = 0
			s.tag = s.tag[:0]
			return
		}
		if s.capture && s.depth >= 2 {
			s.rawBuf.WriteByte(c)
		}
		return
	}
	if s.quote != 0 {
		if c == s.quote {
			s.quote = 0
		}
		s.tag = append(s.tag, c)
		return
	}
	switch c {
	case '"', '\'':
		s.quote = c
		s.tag = append(s.tag, c)
	case '>':
		s.inTag = false
		s.handleTag(s.tag)
	default:
		s.tag = append(s.tag, c)
	}
}

func localName(tag []byte) string {
	end := len(tag)
	for i, c := range tag {
		if isXMLSpace(c) || c == '/' {
			end = i
			break
		}
	}
	name := tag[:end]
	if colon := bytes.IndexByte(name, ':'); colon >= 0 {
		name = name[colon+1:]
	}
	return string(name)
}

func (s *stanzaScanner) handleTag(tag []byte) {
	if len(tag) == 0 {
		return
	}
	switch tag[0] {
	case '?', '!':
		return
	case '/':
		name := localName(tag[1:])
		if s.depth >= 2 && s.capture {
			s.rawBuf.WriteByte('<')
			s.rawBuf.Write(tag)
			s.rawBuf.WriteByte('>')
		}
		if s.depth == 2 {
			s.finish()
		}
		if s.depth > 0 {
			s.depth--
		}
		if name == "stream" && s.depth <= 1 {
			s.depth = 0
		}
		return
	}
	name := localName(tag)
	selfClosing := tag[len(tag)-1] == '/'
	if name == "stream" && s.depth <= 1 {
		// Stream header: (re)start the stream.
		s.depth = 1
		s.cur = nil
		s.rawBuf.Reset()
		return
	}
	s.depth++
	if s.depth == 2 {
		s.cur = &topLevelElement{Name: name, StartTag: append([]byte(nil), tag...)}
		s.rawBuf.Reset()
	}
	if s.depth >= 2 && s.capture {
		s.rawBuf.WriteByte('<')
		s.rawBuf.Write(tag)
		s.rawBuf.WriteByte('>')
	}
	if selfClosing {
		if s.depth == 2 {
			s.finish()
		}
		s.depth--
	}
}

func (s *stanzaScanner) finish() {
	if s.cur == nil {
		return
	}
	if s.capture {
		s.cur.Raw = append([]byte(nil), s.rawBuf.Bytes()...)
	}
	s.rawBuf.Reset()
	el := s.cur
	s.cur = nil
	if s.emit != nil {
		s.emit(el)
	}
}

func isStanzaName(name string) bool {
	return name == "message" || name == "presence" || name == "iq"
}
