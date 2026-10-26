package metrics

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/keplersj/mautrix-xmpp/pkg/xmppx"
)

// Disconnect reasons.
const (
	ReasonBadCredentials = "bad_credentials"
	ReasonAuthTemporary  = "auth_temporary"
	ReasonTLS            = "tls"
	ReasonConflict       = "stream_conflict"
	ReasonStreamError    = "stream_error"
	ReasonTimeout        = "timeout"
	ReasonNetwork        = "network"
	ReasonEOF            = "eof"
	ReasonOther          = "other"
)

// DisconnectReasons lists every reason DisconnectReason can return.
var DisconnectReasons = []string{
	ReasonBadCredentials, ReasonAuthTemporary, ReasonTLS, ReasonConflict, ReasonStreamError,
	ReasonTimeout, ReasonNetwork, ReasonEOF, ReasonOther,
}

// DisconnectReason maps why a session ended to a fixed label value.
func DisconnectReason(err error) string {
	var (
		authErr *xmppx.AuthError
		tlsErr  *xmppx.TLSError
		netErr  net.Error
	)
	switch {
	case errors.As(err, &authErr):
		if authErr.Permanent() {
			return ReasonBadCredentials
		}
		return ReasonAuthTemporary
	case errors.As(err, &tlsErr):
		return ReasonTLS
	case xmppx.IsStreamError(err, "conflict"):
		return ReasonConflict
	case xmppx.IsStreamError(err, ""):
		return ReasonStreamError
	case errors.Is(err, context.DeadlineExceeded):
		return ReasonTimeout
	case errors.As(err, &netErr):
		if netErr.Timeout() {
			return ReasonTimeout
		}
		return ReasonNetwork
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return ReasonEOF
	default:
		return ReasonOther
	}
}
