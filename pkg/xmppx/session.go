package xmppx

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"mellium.im/sasl"
	"mellium.im/xmlstream"
	"mellium.im/xmpp"
	"mellium.im/xmpp/jid"
	"mellium.im/xmpp/stanza"
	"mellium.im/xmpp/stream"
)

// Config describes how to connect one XMPP account.
type Config struct {
	// JID is the account's bare JID.
	JID jid.JID
	// Password is the account password. It is only passed to SASL.
	Password string
	// Resource is the resource to request when binding.
	Resource string
	// Server optionally overrides SRV discovery ("host:port" for STARTTLS,
	// "tls://host:port" for direct TLS).
	Server string
	// Proxy is an optional socks5:// URL.
	Proxy string
	// TLSConfig is an optional base TLS config (for example with custom
	// RootCAs). ServerName and verification are always enforced.
	TLSConfig *tls.Config
	// ConnectTimeout bounds dialing and stream negotiation.
	ConnectTimeout time.Duration
	// LogMessageContent logs the XML stream after authentication at trace level.
	// This includes message bodies.
	LogMessageContent bool
	// SoftwareName and SoftwareVersion are returned for XEP-0092 queries.
	SoftwareName, SoftwareVersion string
	// Observer optionally receives protocol events for metrics.
	Observer Observer
}

// Handler receives inbound stanzas. Calls are made from a single goroutine in
// the order the stanzas arrived, and the handler may make blocking calls on the
// session (such as IQs) because the read loop keeps running independently.
type Handler interface {
	HandleMessage(s *Session, msg *Message)
	HandlePresence(s *Session, p *Presence)
	HandleRosterPush(s *Session, items []RosterItem)
}

// AuthError is returned when the server rejects the credentials.
type AuthError struct {
	// Condition is the SASL failure condition, like "not-authorized".
	Condition string
	Text      string
}

func (e *AuthError) Error() string {
	if e.Text != "" {
		return "authentication failed: " + e.Condition + ": " + e.Text
	}
	return "authentication failed: " + e.Condition
}

// Permanent reports whether retrying with the same credentials is pointless.
func (e *AuthError) Permanent() bool {
	return e.Condition != "temporary-auth-failure" && e.Condition != "aborted"
}

// TLSError wraps certificate verification and handshake failures.
type TLSError struct {
	Err error
}

func (e *TLSError) Error() string { return "TLS error: " + e.Err.Error() }
func (e *TLSError) Unwrap() error { return e.Err }

// classifyError turns the errors mellium returns during negotiation into
// AuthError/TLSError where possible.
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	// mellium's SASL failure type lives in an internal package, so look at it
	// through reflection rather than string matching.
	v := reflect.ValueOf(err)
	if v.Kind() == reflect.Struct && v.Type().PkgPath() == "mellium.im/xmpp/internal/saslerr" && v.Type().Name() == "Error" {
		ae := &AuthError{Condition: "not-authorized"}
		if cond := v.FieldByName("Condition"); cond.IsValid() {
			if s, ok := cond.Interface().(fmt.Stringer); ok && s.String() != "none" {
				ae.Condition = s.String()
			}
		}
		if text := v.FieldByName("Text"); text.IsValid() {
			ae.Text = text.String()
		}
		return ae
	}
	var (
		certErr     *tls.CertificateVerificationError
		unknownAuth x509.UnknownAuthorityError
		hostErr     x509.HostnameError
		invalidErr  x509.CertificateInvalidError
		recordErr   tls.RecordHeaderError
		alertErr    tls.AlertError
	)
	if errors.As(err, &certErr) || errors.As(err, &unknownAuth) || errors.As(err, &hostErr) ||
		errors.As(err, &invalidErr) || errors.As(err, &recordErr) || errors.As(err, &alertErr) {
		return &TLSError{Err: err}
	}
	return err
}

// Session is one negotiated XMPP stream.
type Session struct {
	cfg     *Config
	log     zerolog.Logger
	xs      *xmpp.Session
	conn    net.Conn
	handler Handler
	obs     Observer
	sm      *streamMgmt
	smOffer bool

	bound jid.JID

	queue *dispatchQueue
	mam   mamState
	joins joinState

	ready     atomic.Bool
	closeOnce sync.Once
	done      chan struct{}
	err       error
}

// smFeature is an informational stream feature: it only records that the
// server offers XEP-0198. Enabling happens after the session is up.
func smFeature() xmpp.StreamFeature {
	return xmpp.StreamFeature{
		Name:      xml.Name{Space: NSStreamMgmt, Local: "sm"},
		Necessary: xmpp.Authn,
		Parse: func(ctx context.Context, d *xml.Decoder, start *xml.StartElement) (bool, interface{}, error) {
			return false, true, d.Skip()
		},
	}
}

// teeWriter feeds a stanza scanner and, after authentication, the stanza log.
type teeWriter struct {
	scanner *stanzaScanner
	ready   *atomic.Bool
	log     *zerolog.Logger
	dir     string
}

func (t *teeWriter) Write(p []byte) (int, error) {
	_, _ = t.scanner.Write(p)
	if t.log != nil && t.ready.Load() {
		t.log.Trace().Str("direction", t.dir).Str("xml", string(p)).Msg("XMPP stream data")
	}
	return len(p), nil
}

// Connect dials, negotiates and starts serving a new session. prevUnacked are
// messages from a previous session that the server never acknowledged; they
// are re-sent once the new session is up.
func Connect(ctx context.Context, cfg *Config, log zerolog.Logger, handler Handler, prevUnacked [][]byte) (*Session, error) {
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 30 * time.Second
	}
	dialCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()

	var endpoints []endpoint
	if cfg.Server != "" {
		ep, err := ParseServerOverride(cfg.Server)
		if err != nil {
			return nil, err
		}
		endpoints = []endpoint{ep}
	} else {
		endpoints = resolveEndpoints(dialCtx, net.DefaultResolver, cfg.JID.Domainpart())
	}
	dialer, err := makeDialer(cfg.Proxy)
	if err != nil {
		return nil, err
	}
	conn, ep, err := dial(dialCtx, dialer, endpoints, cfg.JID, cfg.TLSConfig)
	if err != nil {
		return nil, classifyError(err)
	}
	log = log.With().Str("endpoint", ep.String()).Logger()
	log.Debug().Msg("Connected to XMPP server, negotiating stream")

	s := &Session{
		cfg:     cfg,
		log:     log,
		conn:    conn,
		handler: handler,
		obs:     cfg.observer(),
		sm:      newStreamMgmt(),
		queue:   newDispatchQueue(),
		mam:     mamState{queries: make(map[string]*mamCollector)},
		joins:   joinState{waiters: make(map[string]chan *Presence)},
		done:    make(chan struct{}),
	}
	var stanzaLog *zerolog.Logger
	if cfg.LogMessageContent {
		l := log.With().Str("component", "xmpp-stream").Logger()
		stanzaLog = &l
	}
	scanIn := func(el *topLevelElement) {
		s.sm.handleIn(el)
		if isStanzaName(el.Name) {
			s.obs.Stanza(false, el.Name)
		}
	}
	scanOut := func(el *topLevelElement) {
		s.sm.handleOut(el)
		if isStanzaName(el.Name) {
			s.obs.Stanza(true, el.Name)
		}
	}
	teeIn := &teeWriter{scanner: newStanzaScanner(false, scanIn), ready: &s.ready, log: stanzaLog, dir: "in"}
	teeOut := &teeWriter{scanner: newStanzaScanner(true, scanOut), ready: &s.ready, log: stanzaLog, dir: "out"}

	tlsCfg := tlsConfigFor(cfg.TLSConfig, cfg.JID.Domainpart())
	origin, err := cfg.JID.WithResource(cfg.Resource)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("invalid resource: %w", err)
	}
	var state xmpp.SessionState
	if ep.directTLS {
		state |= xmpp.Secure
	}
	features := []xmpp.StreamFeature{
		xmpp.StartTLS(tlsCfg),
		xmpp.SASL("", cfg.Password,
			sasl.ScramSha256Plus, sasl.ScramSha256,
			sasl.ScramSha1Plus, sasl.ScramSha1,
			sasl.Plain,
		),
		bindFeature(cfg.Resource),
		smFeature(),
	}
	negotiator := xmpp.NewNegotiator(func(*xmpp.Session, *xmpp.StreamConfig) xmpp.StreamConfig {
		return xmpp.StreamConfig{
			Features: features,
			TeeIn:    teeIn,
			TeeOut:   teeOut,
		}
	})
	xs, err := xmpp.NewSession(dialCtx, cfg.JID.Domain(), origin, conn, state, negotiator)
	if err != nil {
		_ = conn.Close()
		return nil, classifyError(err)
	}
	if xs.State()&xmpp.Secure == 0 {
		// StartTLS is attempted unconditionally, so this should be impossible.
		_ = conn.Close()
		return nil, errors.New("refusing to use an unencrypted stream")
	}
	s.xs = xs
	s.bound = xs.LocalAddr()
	_, s.smOffer = xs.Feature(NSStreamMgmt)
	s.ready.Store(true)
	s.log = s.log.With().Str("bound_jid", s.bound.String()).Logger()

	go s.queue.run()
	go s.serve()

	if s.smOffer {
		if err = s.enableSM(dialCtx); err != nil {
			s.log.Warn().Err(err).Msg("Failed to enable stream management")
		}
	}
	for _, raw := range prevUnacked {
		if err = s.sendRaw(dialCtx, raw); err != nil {
			s.Close()
			return nil, fmt.Errorf("failed to re-send unacknowledged message: %w", err)
		}
	}
	if len(prevUnacked) > 0 {
		s.obs.Resent(len(prevUnacked))
		s.log.Info().Int("count", len(prevUnacked)).Msg("Re-sent messages the previous session did not get acknowledged")
		s.RequestAck(dialCtx)
	}
	return s, nil
}

// enableSM sends <enable/> and waits for <enabled/> or <failed/>.
func (s *Session) enableSM(ctx context.Context) error {
	result := make(chan bool, 1)
	s.sm.mu.Lock()
	s.sm.onEnabled = func(ok bool) {
		select {
		case result <- ok:
		default:
		}
	}
	s.sm.sendAck = func(h uint32) {
		// Answer from a separate goroutine: this is called from the read loop.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := s.sendNonza(ctx, "a", xml.Attr{Name: xml.Name{Local: "h"}, Value: fmt.Sprint(h)}); err != nil {
				s.log.Debug().Err(err).Msg("Failed to send stream management ack")
			}
		}()
	}
	s.sm.mu.Unlock()
	if err := s.sendNonza(ctx, "enable"); err != nil {
		return err
	}
	select {
	case ok := <-result:
		if !ok {
			return errors.New("server refused to enable stream management")
		}
		s.log.Debug().Msg("Stream management enabled")
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return s.err
	}
}

func (s *Session) sendNonza(ctx context.Context, name string, attrs ...xml.Attr) error {
	start := xml.StartElement{Name: xml.Name{Space: NSStreamMgmt, Local: name}, Attr: attrs}
	return s.xs.Send(ctx, xmlstream.Wrap(nil, start))
}

// RequestAck asks the server to acknowledge what it has received so far.
func (s *Session) RequestAck(ctx context.Context) {
	if !s.sm.Enabled() {
		return
	}
	if err := s.sendNonza(ctx, "r"); err != nil {
		s.log.Debug().Err(err).Msg("Failed to request stream management ack")
	}
}

type rawTokenReader struct{ *xml.Decoder }

func (r rawTokenReader) Token() (xml.Token, error) { return r.RawToken() }

func (s *Session) sendRaw(ctx context.Context, raw []byte) error {
	return s.xs.Send(ctx, rawTokenReader{xml.NewDecoder(bytes.NewReader(raw))})
}

// serve runs the read loop until the stream ends.
func (s *Session) serve() {
	err := s.xs.Serve(xmpp.HandlerFunc(s.handleXMPP))
	if err == nil {
		err = io.EOF
	}
	s.finish(err)
}

func (s *Session) finish(err error) {
	s.closeOnce.Do(func() {
		s.err = err
		_ = s.conn.Close()
		s.queue.stop()
		close(s.done)
	})
}

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err returns why the session ended. Only valid after Done is closed.
func (s *Session) Err() error { return s.err }

// Close ends the stream gracefully and closes the connection.
func (s *Session) Close() {
	select {
	case <-s.done:
		return
	default:
	}
	_ = s.xs.Close()
	// Give the server a moment to close its side, then force it.
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
	}
	s.finish(errors.New("closed"))
}

// JID returns the bound full JID.
func (s *Session) JID() jid.JID { return s.bound }

// StreamManagementEnabled reports whether XEP-0198 is active.
func (s *Session) StreamManagementEnabled() bool { return s.sm.Enabled() }

// TakeUnacked returns unacknowledged messages so the next session can re-send
// them. Call it only after the session ended.
func (s *Session) TakeUnacked() [][]byte { return s.sm.takeUnackedMessages() }

// PendingAcks returns the number of outbound stanzas not yet acknowledged.
func (s *Session) PendingAcks() int { return s.sm.pendingCount() }

func (s *Session) handleXMPP(t xmlstream.TokenReadEncoder, start *xml.StartElement) error {
	if start.Name.Space != NSClient && start.Name.Space != "" {
		// Stream management nonzas are handled by the byte scanners.
		return nil
	}
	d := xml.NewTokenDecoder(xmlstream.MultiReader(xmlstream.Token(*start), t))
	switch start.Name.Local {
	case "message":
		var msg Message
		if err := d.Decode(&msg); err != nil {
			s.log.Warn().Err(err).Msg("Failed to decode message stanza")
			return nil
		}
		if msg.MAMResult != nil && s.collectMAMResult(&msg) {
			return nil
		}
		s.queue.push(func() { s.handler.HandleMessage(s, &msg) })
	case "presence":
		var p Presence
		if err := d.Decode(&p); err != nil {
			s.log.Warn().Err(err).Msg("Failed to decode presence stanza")
			return nil
		}
		s.matchJoin(&p)
		s.queue.push(func() { s.handler.HandlePresence(s, &p) })
	case "iq":
		var iq incomingIQ
		if err := d.Decode(&iq); err != nil {
			s.log.Warn().Err(err).Msg("Failed to decode IQ stanza")
			return nil
		}
		return s.handleIQ(t, &iq)
	}
	return nil
}

type incomingIQ struct {
	XMLName xml.Name `xml:"jabber:client iq"`
	ID      string   `xml:"id,attr"`
	Type    string   `xml:"type,attr"`
	From    string   `xml:"from,attr"`
	To      string   `xml:"to,attr"`
	Payload struct {
		XMLName xml.Name
		Node    string `xml:"node,attr"`
		Inner   []byte `xml:",innerxml"`
	} `xml:",any"`
}

type iqResponse struct {
	XMLName xml.Name `xml:"jabber:client iq"`
	ID      string   `xml:"id,attr"`
	Type    string   `xml:"type,attr"`
	To      string   `xml:"to,attr,omitempty"`
	Payload any
}

type discoInfoResult struct {
	XMLName    xml.Name        `xml:"http://jabber.org/protocol/disco#info query"`
	Node       string          `xml:"node,attr,omitempty"`
	Identities []DiscoIdentity `xml:"identity"`
	Features   []DiscoFeature  `xml:"feature"`
}

type versionResult struct {
	XMLName xml.Name `xml:"jabber:iq:version query"`
	Name    string   `xml:"name"`
	Version string   `xml:"version"`
}

// ClientFeatures are the features advertised in our own disco#info.
var ClientFeatures = []string{
	NSDiscoInfo, NSPing, NSReceipts, NSMarkers, NSChatStates, NSReactions,
	NSCorrect, NSRetract, NSReply, NSFallback, NSOOB, NSVersion, NSStanzaID,
}

func (s *Session) isFromServerOrSelf(from string) bool {
	if from == "" {
		return true
	}
	j, err := jid.Parse(from)
	if err != nil {
		return false
	}
	return j.Equal(s.bound.Domain()) || j.Bare().Equal(s.bound.Bare())
}

func (s *Session) handleIQ(t xmlstream.TokenReadEncoder, iq *incomingIQ) error {
	if iq.Type != "get" && iq.Type != "set" {
		return nil
	}
	reply := func(payload any) error {
		return t.Encode(iqResponse{ID: iq.ID, Type: "result", To: iq.From, Payload: payload})
	}
	p := iq.Payload.XMLName
	switch {
	case iq.Type == "get" && p.Space == NSPing && p.Local == "ping":
		return reply(nil)
	case iq.Type == "get" && p.Space == NSDiscoInfo && p.Local == "query":
		res := discoInfoResult{
			Node:       iq.Payload.Node,
			Identities: []DiscoIdentity{{Category: "client", Type: "bot", Name: s.cfg.SoftwareName}},
		}
		for _, f := range ClientFeatures {
			res.Features = append(res.Features, DiscoFeature{Var: f})
		}
		return reply(res)
	case iq.Type == "get" && p.Space == NSVersion && p.Local == "query":
		return reply(versionResult{Name: s.cfg.SoftwareName, Version: s.cfg.SoftwareVersion})
	case iq.Type == "set" && p.Space == NSRoster && p.Local == "query":
		// RFC 6121 §2.1.6: only accept roster pushes from our own account.
		if !s.isFromServerOrSelf(iq.From) {
			return nil
		}
		var q rosterQuery
		if err := xml.Unmarshal(append(append([]byte(`<query xmlns="jabber:iq:roster">`), iq.Payload.Inner...), []byte(`</query>`)...), &q); err != nil {
			s.log.Warn().Err(err).Msg("Failed to decode roster push")
		} else {
			items := q.Items
			s.queue.push(func() { s.handler.HandleRosterPush(s, items) })
		}
		return reply(nil)
	}
	// mellium answers unhandled get/set IQs with service-unavailable.
	return nil
}

// RandomID returns a random identifier suitable for stanza and origin IDs.
func RandomID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// SendMessage sends a message stanza. If the message has no ID one is
// generated, and an origin-id equal to the ID is added. It returns once the
// stanza has been written; use WaitAck to wait for the server's
// acknowledgement.
func (s *Session) SendMessage(ctx context.Context, msg *Message) error {
	if msg.ID == "" {
		msg.ID = RandomID()
	}
	if msg.OriginID == nil && msg.Type != MessageError {
		msg.OriginID = &OriginID{ID: msg.ID}
	}
	return s.xs.Encode(ctx, msg)
}

// WaitAck waits until the server acknowledges the stanza with the given ID
// (XEP-0198). It returns true if it was acknowledged, false if stream
// management is not active or the wait ended first.
func (s *Session) WaitAck(ctx context.Context, id string) bool {
	ch, ok := s.sm.waitAck(id)
	if !ok {
		return s.sm.Enabled()
	}
	s.RequestAck(ctx)
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	case <-s.done:
		return false
	}
}

// SendPresence sends a presence stanza.
func (s *Session) SendPresence(ctx context.Context, p *Presence) error {
	return s.xs.Encode(ctx, p)
}

type outgoingIQ struct {
	XMLName xml.Name `xml:"jabber:client iq"`
	ID      string   `xml:"id,attr"`
	Type    string   `xml:"type,attr"`
	To      string   `xml:"to,attr,omitempty"`
	Payload any
}

// IQ sends an IQ of the given type ("get" or "set") with the payload and
// decodes the first child of the result into resp (if not nil). Error results
// are returned as stanza.Error.
func (s *Session) IQ(ctx context.Context, typ string, to string, payload any, resp any) error {
	return s.iq(ctx, IQOpOther, typ, to, payload, resp)
}

// iq is IQ with the operation name reported to the Observer.
func (s *Session) iq(ctx context.Context, op, typ, to string, payload, resp any) error {
	var buf bytes.Buffer
	if err := xml.NewEncoder(&buf).Encode(outgoingIQ{ID: RandomID(), Type: typ, To: to, Payload: payload}); err != nil {
		return fmt.Errorf("failed to encode IQ: %w", err)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}
	// mellium only cancels waiting for the response when ctx is done, so make
	// sure a dead session also ends the wait.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	start := time.Now()
	err := s.xs.UnmarshalIQ(ctx, rawTokenReader{xml.NewDecoder(&buf)}, resp)
	s.obs.IQDone(op, time.Since(start), err)
	return err
}

// IsStanzaError reports whether err is an XMPP stanza error with the given
// condition (for example "item-not-found"). An empty condition matches any
// stanza error.
func IsStanzaError(err error, condition string) bool {
	var se stanza.Error
	if !errors.As(err, &se) {
		return false
	}
	return condition == "" || string(se.Condition) == condition
}

// Ping sends a XEP-0199 ping to the server.
func (s *Session) Ping(ctx context.Context) error {
	err := s.iq(ctx, IQOpPing, "get", s.bound.Domain().String(), struct {
		XMLName xml.Name `xml:"urn:xmpp:ping ping"`
	}{}, nil)
	if err != nil && IsStanzaError(err, "") {
		// Any reply, even an error, proves the stream is alive.
		return nil
	}
	return err
}

// IsStreamError reports whether err is a stream error with the given
// condition. An empty condition matches any stream error.
func IsStreamError(err error, condition string) bool {
	var se stream.Error
	if !errors.As(err, &se) {
		return false
	}
	return condition == "" || se.Err == condition
}
