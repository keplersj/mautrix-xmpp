package connector

import (
	"mime"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"

	"maunium.net/go/mautrix/event"

	"github.com/keplersj/mautrix-xmpp/pkg/xmppx"
)

const encryptedNotice = "🔒 This message is end-to-end encrypted (OMEMO/OpenPGP), which the bridge does not support. Read it in an XMPP client."

// retractFallback is the body sent along with retractions for clients that
// don't support XEP-0424 (the text suggested by the XEP).
const retractFallback = "This person attempted to retract a previous message, but it's unsupported by your client."

// isEncrypted reports whether the message is end-to-end encrypted.
func isEncrypted(msg *xmppx.Message) bool {
	return msg.OMEMO != nil || msg.OMEMO2 != nil || msg.OpenPGP != nil
}

// stripFallback removes the body ranges that XEP-0428 marks as fallback for
// the given namespace. Offsets are in Unicode code points. Malformed ranges
// are ignored.
func stripFallback(body string, fallbacks []xmppx.Fallback, ns string) string {
	type rng struct{ start, end int }
	var ranges []rng
	n := utf8.RuneCountInString(body)
	for _, fb := range fallbacks {
		if fb.For != ns {
			continue
		}
		if len(fb.Bodies) == 0 {
			// A fallback without ranges means the whole body is fallback.
			return ""
		}
		for _, b := range fb.Bodies {
			if b.Start == nil || b.End == nil {
				return ""
			}
			if *b.Start < 0 || *b.End > n || *b.Start >= *b.End {
				continue
			}
			ranges = append(ranges, rng{*b.Start, *b.End})
		}
	}
	if len(ranges) == 0 {
		return body
	}
	var sb strings.Builder
	i := 0
	for _, r := range body {
		inRange := false
		for _, rg := range ranges {
			if i >= rg.start && i < rg.end {
				inRange = true
				break
			}
		}
		if !inRange {
			sb.WriteRune(r)
		}
		i++
	}
	return sb.String()
}

// textContent converts the body of an XMPP message to Matrix content. It
// returns nil if there is nothing to show.
func textContent(msg *xmppx.Message) *event.MessageEventContent {
	if isEncrypted(msg) {
		return &event.MessageEventContent{MsgType: event.MsgNotice, Body: encryptedNotice}
	}
	body := msg.Body
	if msg.Reply != nil {
		body = stripFallback(body, msg.Fallbacks, xmppx.NSReply)
	}
	if body == "" {
		return nil
	}
	if rest, ok := strings.CutPrefix(body, "/me "); ok {
		return &event.MessageEventContent{MsgType: event.MsgEmote, Body: rest}
	}
	return &event.MessageEventContent{MsgType: event.MsgText, Body: body}
}

// attachment is a file linked from an XMPP message.
type attachment struct {
	URL     string
	Caption string
}

// findAttachment returns the XEP-0066 link of a message, if it should be
// bridged as a file. Following the convention used by Conversations and
// others, a message is a file transfer when the body equals the OOB URL; any
// other text in the body is used as a caption.
func findAttachment(msg *xmppx.Message) (attachment, bool) {
	for _, oob := range msg.OOB {
		u, err := url.Parse(strings.TrimSpace(oob.URL))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			continue
		}
		body := strings.TrimSpace(msg.Body)
		caption := ""
		if body != "" && body != oob.URL {
			caption = strings.TrimSpace(strings.Replace(body, oob.URL, "", 1))
		}
		if caption == "" {
			caption = strings.TrimSpace(oob.Desc)
		}
		return attachment{URL: oob.URL, Caption: caption}, true
	}
	return attachment{}, false
}

// fileNameFromURL returns the last path segment of a URL, unescaped.
func fileNameFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "file"
	}
	name := path.Base(u.Path)
	if unescaped, err := url.PathUnescape(name); err == nil {
		name = unescaped
	}
	if name == "" || name == "/" || name == "." {
		return "file"
	}
	return name
}

// msgTypeForMime picks the Matrix msgtype for a MIME type.
func msgTypeForMime(mimeType string) event.MessageType {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return event.MsgImage
	case strings.HasPrefix(mimeType, "video/"):
		return event.MsgVideo
	case strings.HasPrefix(mimeType, "audio/"):
		return event.MsgAudio
	default:
		return event.MsgFile
	}
}

// guessMime determines a file's MIME type from the HTTP header, falling back to
// the extension.
func guessMime(header, fileName string) string {
	if header != "" {
		if mt, _, err := mime.ParseMediaType(header); err == nil && mt != "application/octet-stream" {
			return mt
		}
	}
	if byExt := mime.TypeByExtension(path.Ext(fileName)); byExt != "" {
		if mt, _, err := mime.ParseMediaType(byExt); err == nil {
			return mt
		}
	}
	return "application/octet-stream"
}

// xmppID returns the sender-assigned ID of a message: origin-id, then the id
// attribute.
func xmppID(msg *xmppx.Message) string {
	if msg.OriginID != nil && msg.OriginID.ID != "" {
		return msg.OriginID.ID
	}
	return msg.ID
}

// archiveIDBy returns the stanza-id assigned by the given archive.
func archiveIDBy(msg *xmppx.Message, by string) string {
	for _, sid := range msg.StanzaIDs {
		if strings.EqualFold(sid.By, by) {
			return sid.ID
		}
	}
	return ""
}

// matrixBody returns the plain text to send to XMPP for Matrix content.
func matrixBody(content *event.MessageEventContent) string {
	content.RemoveReplyFallback()
	body := content.Body
	if content.MsgType == event.MsgEmote {
		body = "/me " + body
	}
	return body
}
