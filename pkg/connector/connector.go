package connector

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/rs/zerolog"
	"go.mau.fi/util/configupgrade"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"mellium.im/xmpp/jid"

	"github.com/keplersj/mautrix-xmpp/pkg/metrics"
)

// XMPPConnector implements bridgev2.NetworkConnector.
type XMPPConnector struct {
	br     *bridgev2.Bridge
	Config Config
	db     *aliasDB

	metrics       *metrics.Metrics
	metricsServer *metrics.Server

	// Version is reported in XEP-0092 software version replies.
	Version string
	// Commit is reported in the build_info metric.
	Commit string
}

var (
	_ bridgev2.NetworkConnector            = (*XMPPConnector)(nil)
	_ bridgev2.ConfigValidatingNetwork     = (*XMPPConnector)(nil)
	_ bridgev2.IdentifierValidatingNetwork = (*XMPPConnector)(nil)
	_ bridgev2.MaxFileSizeingNetwork       = (*XMPPConnector)(nil)
	_ bridgev2.StoppableNetwork            = (*XMPPConnector)(nil)
)

func (xc *XMPPConnector) Init(br *bridgev2.Bridge) {
	xc.br = br
	xc.db = newAliasDB(br.DB.Database, br.Log.With().Str("db_section", "xmpp").Logger())
	xc.metrics = metrics.New(xc.Version, xc.Commit)
	xc.metrics.SetSnapshotFunc(xc.metricsSnapshot)
	if xc.Config.Metrics.Enabled {
		// Listen here rather than in Start, which only runs once the Matrix
		// connection is up: health checks and metrics should work while the
		// bridge is stuck waiting for the homeserver.
		// Init can't return an error, so exit like mxmain does for other
		// startup failures.
		log := br.Log.With().Str("component", "metrics").Logger()
		srv, err := metrics.Listen(xc.Config.Metrics.Listen, xc.metrics, xc.Config.Metrics.Pprof, log)
		if err != nil {
			log.WithLevel(zerolog.FatalLevel).Err(err).Str("address", xc.Config.Metrics.Listen).Msg("Failed to listen for metrics")
			os.Exit(1)
		}
		xc.metricsServer = srv
		srv.Serve()
	}
}

func (xc *XMPPConnector) Start(ctx context.Context) error {
	if err := xc.db.Upgrade(ctx); err != nil {
		return bridgev2.DBUpgradeError{Err: err, Section: "xmpp"}
	}
	if xc.metricsServer != nil {
		xc.metricsServer.SetReady(true)
	}
	return nil
}

// Stop implements bridgev2.StoppableNetwork.
func (xc *XMPPConnector) Stop() {
	if xc.metricsServer != nil {
		xc.metricsServer.Close()
	}
}

// metricsSnapshot reports per-login state for the metrics scrape.
func (xc *XMPPConnector) metricsSnapshot() metrics.Snapshot {
	snap := metrics.Snapshot{Logins: make(map[string]int)}
	for _, login := range xc.br.GetAllCachedUserLogins() {
		c, ok := login.Client.(*XMPPClient)
		if !ok || c == nil {
			continue
		}
		snap.Logins[c.metricsState()]++
		if s := c.mgr.Session(); s != nil {
			snap.Unacked += s.PendingAcks()
		}
	}
	return snap
}

func (xc *XMPPConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:          "XMPP",
		NetworkURL:           "https://xmpp.org",
		NetworkIcon:          "mxc://maunium.net/aoqOxTxfFBfXGRwshkrOulDe",
		NetworkID:            "xmpp",
		BeeperBridgeType:     "github.com/keplersj/mautrix-xmpp",
		DefaultPort:          29371,
		DefaultCommandPrefix: "!xmpp",
	}
}

func (xc *XMPPConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{}
}

func (xc *XMPPConnector) GetBridgeInfoVersion() (info, capabilities int) {
	return 1, 1
}

func (xc *XMPPConnector) GetConfig() (example string, data any, upgrader configupgrade.Upgrader) {
	return ExampleConfig, &xc.Config, configupgrade.SimpleUpgrader(upgradeConfig)
}

// ValidateConfig implements bridgev2.ConfigValidatingNetwork.
func (xc *XMPPConnector) ValidateConfig() error {
	return xc.Config.Validate()
}

// SetMaxFileSize implements bridgev2.MaxFileSizeingNetwork: the homeserver's
// media limit also caps what we download from XMPP.
func (xc *XMPPConnector) SetMaxFileSize(maxSize int64) {
	if maxSize > 0 && maxSize < xc.Config.Media.MaxDownloadSize {
		xc.Config.Media.MaxDownloadSize = maxSize
	}
}

// ValidateUserID implements bridgev2.IdentifierValidatingNetwork.
func (xc *XMPPConnector) ValidateUserID(id networkid.UserID) bool {
	_, _, err := parseUserID(id)
	return err == nil
}

func (xc *XMPPConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		Portal:    func() any { return &PortalMetadata{} },
		Ghost:     func() any { return &GhostMetadata{} },
		Message:   func() any { return &MessageMetadata{} },
		Reaction:  nil,
		UserLogin: func() any { return &UserLoginMetadata{} },
	}
}

func (xc *XMPPConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	meta, ok := login.Metadata.(*UserLoginMetadata)
	if !ok {
		return fmt.Errorf("unexpected metadata type %T", login.Metadata)
	}
	account, err := parseBareJID(string(login.ID))
	if err != nil {
		return fmt.Errorf("invalid login ID %q: %w", login.ID, err)
	}
	if meta.Resource == "" {
		meta.Resource = xc.newResource()
	}
	login.Client = newXMPPClient(xc, login, account, meta)
	return nil
}

func (xc *XMPPConnector) newResource() string {
	return strings.TrimSuffix(xc.Config.ResourcePrefix, ".") + "." + randomHex(3)
}

// resolveProxy picks the per-login proxy if set, otherwise the default.
func (xc *XMPPConnector) resolveProxy(meta *UserLoginMetadata) string {
	if meta != nil && meta.Proxy != "" {
		return meta.Proxy
	}
	return xc.Config.Proxy
}

func validateAccountJID(s string) (jid.JID, error) {
	j, err := jid.Parse(strings.TrimSpace(s))
	if err != nil {
		return jid.JID{}, fmt.Errorf("invalid JID: %w", err)
	}
	if j.Localpart() == "" {
		return jid.JID{}, fmt.Errorf("the JID must include a username, like user@example.org")
	}
	if !strings.Contains(j.Domainpart(), ".") && j.Domainpart() != "localhost" {
		return jid.JID{}, fmt.Errorf("%q does not look like a domain", j.Domainpart())
	}
	return j.Bare(), nil
}
