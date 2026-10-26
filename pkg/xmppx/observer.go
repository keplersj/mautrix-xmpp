package xmppx

import "time"

// IQ operations reported to Observer.IQDone.
const (
	IQOpPing       = "ping"
	IQOpMAM        = "mam"
	IQOpUploadSlot = "upload_slot"
	IQOpOther      = "other"
)

// Observer receives protocol events, for metrics. Methods are called from the
// session's goroutines (including the read loop), so they must be fast and
// safe for concurrent use.
type Observer interface {
	// Stanza is called for every top-level message, presence or iq on the
	// stream. name is one of those three element names.
	Stanza(outbound bool, name string)
	// IQDone is called when an IQ we sent gets a result, an error or times
	// out. op is one of the IQOp constants.
	IQDone(op string, d time.Duration, err error)
	// MAMPage is called with the number of messages in each archive page.
	MAMPage(messages int)
	// Resent is called with the number of unacknowledged messages a new
	// session re-sent for its predecessor.
	Resent(messages int)
}

type nopObserver struct{}

func (nopObserver) Stanza(bool, string)                 {}
func (nopObserver) IQDone(string, time.Duration, error) {}
func (nopObserver) MAMPage(int)                         {}
func (nopObserver) Resent(int)                          {}

func (cfg *Config) observer() Observer {
	if cfg.Observer == nil {
		return nopObserver{}
	}
	return cfg.Observer
}
