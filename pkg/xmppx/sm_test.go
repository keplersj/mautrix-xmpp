package xmppx

import "testing"

func newStanzaScannerFor(sm *streamMgmt, out bool) *stanzaScanner {
	s := newStanzaScanner(false, sm.handleIn)
	if out {
		s = newStanzaScanner(true, sm.handleOut)
	}
	// Pretend we're inside an established stream.
	s.depth = 1
	return s
}

func TestStreamManagementCounting(t *testing.T) {
	sm := newStreamMgmt()
	var acks []uint32
	sm.sendAck = func(h uint32) { acks = append(acks, h) }
	in := newStanzaScannerFor(sm, false)
	out := newStanzaScannerFor(sm, true)

	// Nothing is counted before SM is enabled.
	_, _ = out.Write([]byte(`<message id="before"><body>x</body></message>`))
	_, _ = out.Write([]byte(`<enable xmlns="urn:xmpp:sm:3"/>`))
	_, _ = in.Write([]byte(`<message><body>early</body></message><enabled xmlns="urn:xmpp:sm:3"/>`))
	if !sm.Enabled() {
		t.Fatal("expected SM enabled")
	}
	_, _ = out.Write([]byte(`<message id="m1"><body>1</body></message><iq id="q1" type="get"/><message id="m2"><body>2</body></message><r xmlns="urn:xmpp:sm:3"/>`))
	if sm.pendingCount() != 3 {
		t.Fatalf("pending = %d", sm.pendingCount())
	}
	ch, ok := sm.waitAck("m2")
	if !ok {
		t.Fatal("m2 should be awaiting ack")
	}
	_, _ = in.Write([]byte(`<presence/><iq type="result" id="x"/><r xmlns="urn:xmpp:sm:3"/>`))
	if len(acks) != 1 || acks[0] != 2 {
		t.Fatalf("acks = %v (inbound count should be 2)", acks)
	}
	_, _ = in.Write([]byte(`<a xmlns="urn:xmpp:sm:3" h="2"/>`))
	if sm.pendingCount() != 1 {
		t.Fatalf("pending after ack = %d", sm.pendingCount())
	}
	select {
	case <-ch:
		t.Fatal("m2 must not be acked yet")
	default:
	}
	unacked := sm.takeUnackedMessages()
	if len(unacked) != 1 || string(unacked[0]) != `<message id="m2"><body>2</body></message>` {
		t.Fatalf("unacked = %q", unacked)
	}
	if _, ok := sm.waitAck("m2"); ok {
		t.Error("taken messages are no longer tracked")
	}
}

func TestStreamManagementWrap(t *testing.T) {
	sm := newStreamMgmt()
	sm.enabledOut = true
	sm.outCount = ^uint32(0) - 1 // 2^32-2
	out := newStanzaScannerFor(sm, true)
	_, _ = out.Write([]byte(`<message id="a"/><message id="b"/><message id="c"/>`)) // seq 2^32-1, 0, 1
	sm.mu.Lock()
	sm.ackLocked(0)
	sm.mu.Unlock()
	if sm.pendingCount() != 1 {
		t.Fatalf("pending after wrapped ack = %d", sm.pendingCount())
	}
}

func TestStreamManagementFailed(t *testing.T) {
	sm := newStreamMgmt()
	var result *bool
	sm.onEnabled = func(ok bool) { result = &ok }
	out := newStanzaScannerFor(sm, true)
	in := newStanzaScannerFor(sm, false)
	_, _ = out.Write([]byte(`<enable xmlns="urn:xmpp:sm:3"/>`))
	_, _ = in.Write([]byte(`<failed xmlns="urn:xmpp:sm:3"><unexpected-request xmlns="urn:ietf:params:xml:ns:xmpp-stanzas"/></failed>`))
	if result == nil || *result || sm.Enabled() {
		t.Fatal("expected SM to be reported as failed")
	}
	_, _ = out.Write([]byte(`<message id="x"/>`))
	if sm.pendingCount() != 0 {
		t.Error("messages must not be queued when SM failed")
	}
}
