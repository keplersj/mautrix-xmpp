package connector

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"mellium.im/xmpp/jid"

	"github.com/keplersj/mautrix-xmpp/pkg/xmppx"
)

// XMPPClient implements bridgev2.NetworkAPI for one XMPP account.
type XMPPClient struct {
	main    *XMPPConnector
	login   *bridgev2.UserLogin
	account jid.JID
	meta    *UserLoginMetadata
	log     zerolog.Logger
	mgr     *xmppx.Manager

	badCredentials atomic.Bool
	connected      atomic.Bool

	mu         sync.Mutex
	roster     map[string]xmppx.RosterItem
	rooms      map[string]*mucRoom
	accountMAM bool
	upload     *xmppx.UploadService
	lastSeen   time.Time
	lastSaved  time.Time

	// Seams for unit tests; set to the real implementations by
	// newXMPPClient.
	queueRemoteEvent  func(bridgev2.RemoteEvent)
	aliases           aliasStore
	lookupMessage     func(ctx context.Context, id networkid.MessageID) (*database.Message, error)
	existingReactions func(ctx context.Context, msg networkid.MessageID) ([]*database.Reaction, error)
	sendStanza        func(ctx context.Context, msg *xmppx.Message, waitAck bool) error

	stopSaver chan struct{}
}

// aliasStore is the part of aliasDB the client uses.
type aliasStore interface {
	Put(ctx context.Context, login networkid.UserLoginID, portal networkid.PortalID, alias string, msg networkid.MessageID) error
	Get(ctx context.Context, login networkid.UserLoginID, portal networkid.PortalID, alias string) (networkid.MessageID, error)
	GetAlias(ctx context.Context, login networkid.UserLoginID, portal networkid.PortalID, msg networkid.MessageID) (string, error)
	DeleteLogin(ctx context.Context, login networkid.UserLoginID) error
}

var (
	_ bridgev2.NetworkAPI                      = (*XMPPClient)(nil)
	_ bridgev2.NetworkAPIWithUserID            = (*XMPPClient)(nil)
	_ bridgev2.IdentifierResolvingNetworkAPI   = (*XMPPClient)(nil)
	_ bridgev2.ContactListingNetworkAPI        = (*XMPPClient)(nil)
	_ bridgev2.BackfillingNetworkAPI           = (*XMPPClient)(nil)
	_ bridgev2.BackfillingNetworkAPIWithLimits = (*XMPPClient)(nil)
	_ bridgev2.EditHandlingNetworkAPI          = (*XMPPClient)(nil)
	_ bridgev2.RedactionHandlingNetworkAPI     = (*XMPPClient)(nil)
	_ bridgev2.ReactionHandlingNetworkAPI      = (*XMPPClient)(nil)
	_ bridgev2.ReadReceiptHandlingNetworkAPI   = (*XMPPClient)(nil)
	_ bridgev2.TypingHandlingNetworkAPI        = (*XMPPClient)(nil)
	_ xmppx.Handler                            = (*XMPPClient)(nil)
	_ xmppx.ManagerEvents                      = (*XMPPClient)(nil)
)

func newXMPPClient(main *XMPPConnector, login *bridgev2.UserLogin, account jid.JID, meta *UserLoginMetadata) *XMPPClient {
	c := &XMPPClient{
		main:     main,
		login:    login,
		account:  account,
		meta:     meta,
		log:      login.Log.With().Str("component", "xmpp").Logger(),
		roster:   make(map[string]xmppx.RosterItem),
		rooms:    make(map[string]*mucRoom),
		lastSeen: meta.LastSeen,
	}
	c.queueRemoteEvent = func(evt bridgev2.RemoteEvent) { login.QueueRemoteEvent(evt) }
	c.aliases = main.db
	c.lookupMessage = func(ctx context.Context, id networkid.MessageID) (*database.Message, error) {
		return main.br.DB.Message.GetFirstPartByID(ctx, login.ID, id)
	}
	c.existingReactions = func(ctx context.Context, msg networkid.MessageID) ([]*database.Reaction, error) {
		return main.br.DB.Reaction.GetAllToMessageBySender(ctx, login.ID, msg, makeUserID(account))
	}
	c.sendStanza = c.sendViaSession
	cfg := &xmppx.Config{
		JID:               account,
		Password:          meta.Password,
		Resource:          meta.Resource,
		Server:            meta.Server,
		Proxy:             main.resolveProxy(meta),
		ConnectTimeout:    time.Duration(main.Config.ConnectTimeout),
		LogMessageContent: main.Config.LogMessageContent,
		SoftwareName:      "mautrix-xmpp",
		SoftwareVersion:   main.Version,
	}
	c.mgr = xmppx.NewManager(cfg, xmppx.ManagerConfig{
		InitialDelay: time.Duration(main.Config.Reconnect.InitialDelay),
		MaxDelay:     time.Duration(main.Config.Reconnect.MaxDelay),
		PingInterval: time.Duration(main.Config.PingInterval),
	}, c.log, c, c)
	return c
}

// Connect starts the connection manager. Errors are reported via bridge state.
func (c *XMPPClient) Connect(ctx context.Context) {
	if c.badCredentials.Load() {
		c.login.BridgeState.Send(status.BridgeState{StateEvent: status.StateBadCredentials, Error: "xmpp-bad-credentials", Message: "The XMPP server rejected the password. Log in again."})
		return
	}
	c.mu.Lock()
	if c.stopSaver == nil {
		c.stopSaver = make(chan struct{})
		go c.lastSeenSaver(c.stopSaver)
	}
	c.mu.Unlock()
	c.mgr.Start()
}

// Disconnect closes the connection and stops reconnecting.
func (c *XMPPClient) Disconnect() {
	c.mgr.Stop()
	c.connected.Store(false)
	c.mu.Lock()
	if c.stopSaver != nil {
		close(c.stopSaver)
		c.stopSaver = nil
	}
	c.mu.Unlock()
	c.saveLastSeen(context.Background(), true)
}

func (c *XMPPClient) IsLoggedIn() bool {
	return !c.badCredentials.Load()
}

// LogoutRemote disconnects. XMPP has no session tokens to revoke; the password
// is deleted with the login.
func (c *XMPPClient) LogoutRemote(ctx context.Context) {
	c.Disconnect()
	if err := c.aliases.DeleteLogin(ctx, c.login.ID); err != nil {
		c.log.Warn().Err(err).Msg("Failed to delete message aliases")
	}
}

func (c *XMPPClient) IsThisUser(_ context.Context, userID networkid.UserID) bool {
	return userID == makeUserID(c.account)
}

func (c *XMPPClient) GetUserID() networkid.UserID {
	return makeUserID(c.account)
}

func (c *XMPPClient) session() (*xmppx.Session, error) {
	s := c.mgr.Session()
	if s == nil || !c.connected.Load() {
		return nil, errNotConnected
	}
	return s, nil
}

var errNotConnected = errors.New("not connected to the XMPP server")

func (c *XMPPClient) OnConnecting(attempt int) {
	if attempt == 1 {
		c.login.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnecting})
	}
}

func (c *XMPPClient) OnDisconnected(err error, retryIn time.Duration) {
	c.connected.Store(false)
	c.log.Warn().Err(err).Dur("retry_in", retryIn).Msg("Disconnected from XMPP server")
	st := status.BridgeState{
		StateEvent: status.StateTransientDisconnect,
		Error:      "xmpp-disconnected",
		Message:    fmt.Sprintf("Disconnected from the XMPP server, retrying in %s", retryIn.Round(time.Second)),
		Info:       map[string]any{"go_error": err.Error()},
	}
	var tlsErr *xmppx.TLSError
	var authErr *xmppx.AuthError
	switch {
	case errors.As(err, &tlsErr):
		// The user (or the server admin) has to act on certificate problems.
		st.StateEvent = status.StateUnknownError
		st.Error = "xmpp-tls-error"
		st.Message = "Could not establish a trusted TLS connection to the XMPP server: " + tlsErr.Err.Error()
	case errors.As(err, &authErr):
		st.Error = "xmpp-auth-temporary"
		st.Message = "The XMPP server could not check the password right now, retrying"
	}
	c.login.BridgeState.Send(st)
}

func (c *XMPPClient) OnFatal(err error) {
	c.connected.Store(false)
	c.badCredentials.Store(true)
	c.log.Error().Err(err).Msg("XMPP server rejected the credentials, not reconnecting")
	c.login.BridgeState.Send(status.BridgeState{
		StateEvent: status.StateBadCredentials,
		Error:      "xmpp-bad-credentials",
		Message:    "The XMPP server rejected the password. Log in again.",
		Info:       map[string]any{"go_error": err.Error()},
	})
}

// OnSessionStarted runs the per-session setup: roster, presence, carbons,
// capability discovery, group chat joins and history catch-up.
func (c *XMPPClient) OnSessionStarted(ctx context.Context, s *xmppx.Session) error {
	log := c.log.With().Str("bound_jid", s.JID().String()).Logger()
	ctx = log.WithContext(ctx)
	// Remember where we were before this session delivers anything new.
	c.mu.Lock()
	since := c.lastSeen
	c.mu.Unlock()

	roster, err := s.Roster(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch roster: %w", err)
	}
	c.mu.Lock()
	c.roster = make(map[string]xmppx.RosterItem, len(roster))
	for _, item := range roster {
		if j, err := parseBareJID(item.JID); err == nil {
			c.roster[j.String()] = item
		}
	}
	c.mu.Unlock()

	if err = s.EnableCarbons(ctx); err != nil {
		log.Warn().Err(err).Msg("Failed to enable message carbons")
	}
	prio := c.main.Config.PresencePriority
	if err = s.SendPresence(ctx, &xmppx.Presence{Priority: &prio}); err != nil {
		return fmt.Errorf("failed to send presence: %w", err)
	}

	info, err := s.DiscoInfo(ctx, "")
	if err != nil {
		log.Warn().Err(err).Msg("Failed to query account features")
	}
	upload, err := s.FindUploadService(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to discover HTTP upload service")
	}
	c.mu.Lock()
	c.accountMAM = info.HasFeature(xmppx.NSMAM)
	c.upload = upload
	c.mu.Unlock()
	log.Info().
		Bool("mam", info.HasFeature(xmppx.NSMAM)).
		Bool("upload", upload != nil).
		Bool("stream_management", s.StreamManagementEnabled()).
		Int("roster_size", len(roster)).
		Msg("Connected to XMPP server")

	c.joinBookmarkedRooms(ctx, s)

	c.connected.Store(true)
	c.login.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})

	go c.catchUp(log.WithContext(context.Background()), s, since)
	return nil
}

// noteSeen records the newest message timestamp for the catch-up query.
func (c *XMPPClient) noteSeen(ts time.Time) {
	c.mu.Lock()
	if ts.After(c.lastSeen) && !ts.After(time.Now().Add(time.Minute)) {
		c.lastSeen = ts
	}
	c.mu.Unlock()
}

func (c *XMPPClient) lastSeenSaver(stop chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			c.saveLastSeen(context.Background(), false)
		}
	}
}

func (c *XMPPClient) saveLastSeen(ctx context.Context, force bool) {
	c.mu.Lock()
	ts := c.lastSeen
	changed := ts.After(c.lastSaved)
	if changed {
		c.lastSaved = ts
	}
	c.mu.Unlock()
	if !changed && !force {
		return
	}
	c.meta.LastSeen = ts
	if err := c.login.Save(ctx); err != nil {
		c.log.Warn().Err(err).Msg("Failed to save login metadata")
	}
}
