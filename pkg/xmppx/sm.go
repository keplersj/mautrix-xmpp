package xmppx

import (
	"strconv"
	"sync"
)

// streamMgmt implements the counting side of XEP-0198 Stream Management. It is
// fed by two stanzaScanners: one on the decrypted inbound byte stream and one on
// the outbound stream. Because it sees bytes rather than handler calls, it
// counts every stanza exactly, including IQ responses that mellium routes
// straight to the waiting caller.
//
// Session resumption is not implemented. Unacknowledged outgoing messages
// survive a disconnect and are handed to the next session to re-send.
type streamMgmt struct {
	mu sync.Mutex

	enabledOut bool // we sent <enable/>, count outbound stanzas
	enabledIn  bool // server answered <enabled/>, count inbound stanzas
	failed     bool

	inCount  uint32
	outCount uint32

	unacked []unackedStanza
	waiters map[string][]chan struct{}

	// sendAck is called (from the read goroutine) when the server sends <r/>.
	sendAck func(h uint32)
	// onEnabled is called when the server confirms or rejects SM.
	onEnabled func(ok bool)
}

type unackedStanza struct {
	seq  uint32
	name string
	id   string
	raw  []byte
}

func newStreamMgmt() *streamMgmt {
	return &streamMgmt{waiters: make(map[string][]chan struct{})}
}

// handleIn processes one inbound top-level element.
func (sm *streamMgmt) handleIn(el *topLevelElement) {
	sm.mu.Lock()
	var sendAck func(uint32)
	var onEnabled func(bool)
	var h uint32
	var enabledResult bool
	switch {
	case isStanzaName(el.Name):
		if sm.enabledIn {
			sm.inCount++
		}
	case el.Name == "enabled":
		sm.enabledIn = true
		sm.inCount = 0
		onEnabled, enabledResult = sm.onEnabled, true
	case el.Name == "failed":
		sm.failed = true
		sm.enabledOut = false
		onEnabled, enabledResult = sm.onEnabled, false
	case el.Name == "r":
		if sm.enabledIn {
			sendAck, h = sm.sendAck, sm.inCount
		}
	case el.Name == "a":
		if v, err := strconv.ParseUint(el.Attr("h"), 10, 32); err == nil {
			sm.ackLocked(uint32(v))
		}
	}
	sm.mu.Unlock()
	if sendAck != nil {
		sendAck(h)
	}
	if onEnabled != nil {
		onEnabled(enabledResult)
	}
}

// handleOut processes one outbound top-level element.
func (sm *streamMgmt) handleOut(el *topLevelElement) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	switch {
	case el.Name == "enable":
		sm.enabledOut = true
		sm.outCount = 0
	case isStanzaName(el.Name) && sm.enabledOut:
		sm.outCount++
		sm.unacked = append(sm.unacked, unackedStanza{
			seq:  sm.outCount,
			name: el.Name,
			id:   el.Attr("id"),
			raw:  el.Raw,
		})
	}
}

// ackLocked drops every queued stanza up to and including h. Sequence numbers
// wrap at 2^32 (XEP-0198 §4), so the comparison uses modular arithmetic.
func (sm *streamMgmt) ackLocked(h uint32) {
	n := 0
	for _, u := range sm.unacked {
		if int32(h-u.seq) < 0 {
			break
		}
		n++
		if u.id != "" {
			for _, ch := range sm.waiters[u.id] {
				close(ch)
			}
			delete(sm.waiters, u.id)
		}
	}
	if n > 0 {
		sm.unacked = append(sm.unacked[:0:0], sm.unacked[n:]...)
	}
}

// Enabled reports whether the server confirmed stream management.
func (sm *streamMgmt) Enabled() bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.enabledIn && sm.enabledOut
}

// waitAck returns a channel that is closed when the outbound stanza with the
// given ID is acknowledged. ok is false if that stanza is not awaiting an ack
// (stream management is off, or it was already acknowledged).
func (sm *streamMgmt) waitAck(id string) (ch chan struct{}, ok bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if !sm.enabledOut || id == "" {
		return nil, false
	}
	for _, u := range sm.unacked {
		if u.id == id {
			ch = make(chan struct{})
			sm.waiters[id] = append(sm.waiters[id], ch)
			return ch, true
		}
	}
	return nil, false
}

// takeUnackedMessages returns the raw bytes of every unacknowledged <message/>
// and stops tracking them. Presences and IQs are bound to the old session and
// are dropped.
func (sm *streamMgmt) takeUnackedMessages() [][]byte {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	var out [][]byte
	for _, u := range sm.unacked {
		if u.name == "message" && len(u.raw) > 0 {
			out = append(out, u.raw)
		}
	}
	sm.unacked = nil
	return out
}

// pendingCount returns the number of unacknowledged outbound stanzas.
func (sm *streamMgmt) pendingCount() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return len(sm.unacked)
}
