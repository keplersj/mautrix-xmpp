// Command mautrix-xmpp is a Matrix bridge that puppets XMPP accounts.
package main

import (
	"os"

	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"

	"github.com/keplersj/mautrix-xmpp/pkg/connector"
)

// Set at build time with -ldflags -X.
var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

const version = "0.1.0"

func main() {
	preflight()
	c := &connector.XMPPConnector{Version: version, Commit: Commit}
	m := mxmain.BridgeMain{
		Name:        "mautrix-xmpp",
		Description: "A Matrix-XMPP puppeting bridge",
		URL:         "https://github.com/keplersj/mautrix-xmpp",
		Version:     version,
		Connector:   c,
	}
	m.InitVersion(Tag, Commit, BuildTime)
	// Same as m.Run(), with the logging config adjusted between loading the
	// config and creating the loggers.
	m.PreInit()
	if !c.Config.LogMessageContent {
		scrubLogging(&m.Config.Logging)
	}
	m.Init()
	m.Start()
	exitCode := m.WaitForInterrupt()
	m.Stop()
	os.Exit(exitCode)
}
