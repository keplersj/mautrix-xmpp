package connector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"

	"github.com/keplersj/mautrix-xmpp/pkg/xmppx"
)

const (
	loginFlowPassword = "password"
	loginStepCreds    = "fi.mau.xmpp.login.credentials"
	loginStepComplete = "fi.mau.xmpp.login.complete"
)

func (xc *XMPPConnector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{{
		ID:          loginFlowPassword,
		Name:        "JID and password",
		Description: "Log in to an existing XMPP account with its address (user@example.org) and password.",
	}}
}

func (xc *XMPPConnector) CreateLogin(_ context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	if flowID != loginFlowPassword {
		return nil, bridgev2.ErrInvalidLoginFlowID
	}
	return &PasswordLogin{user: user, main: xc}, nil
}

// PasswordLogin is the JID + password login flow.
type PasswordLogin struct {
	user *bridgev2.User
	main *XMPPConnector
}

var _ bridgev2.LoginProcessUserInput = (*PasswordLogin)(nil)

func (pl *PasswordLogin) Start(context.Context) (*bridgev2.LoginStep, error) {
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       loginStepCreds,
		Instructions: "Enter your XMPP address and password. The bridge connects as a normal XMPP client; messages are not end-to-end encrypted.",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{
				{
					Type:        bridgev2.LoginInputFieldTypeUsername,
					ID:          "jid",
					Name:        "XMPP address",
					Description: "Your Jabber ID, like user@example.org",
					Validate:    validateJIDInput,
				},
				{
					Type: bridgev2.LoginInputFieldTypePassword,
					ID:   "password",
					Name: "Password",
				},
				{
					Type:        bridgev2.LoginInputFieldTypeDomain,
					ID:          "server",
					Name:        "Server address (optional)",
					Description: "Only needed if your server has no DNS SRV records: host:port for STARTTLS or tls://host:port for direct TLS.",
					Optional:    true,
					Validate:    validateServerInput,
				},
			},
		},
	}, nil
}

func validateJIDInput(s string) (string, error) {
	j, err := validateAccountJID(s)
	if err != nil {
		return "", err
	}
	return j.String(), nil
}

func validateServerInput(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if _, err := xmppx.ParseServerOverride(s); err != nil {
		return "", err
	}
	return s, nil
}

// loginRespError wraps login errors so the provisioning API returns a 4xx
// with a readable message instead of a 500.
func loginRespError(err error) error {
	var authErr *xmppx.AuthError
	var tlsErr *xmppx.TLSError
	var dialErr *xmppx.DialError
	switch {
	case errors.As(err, &authErr):
		return bridgev2.WrapRespErrManual(fmt.Errorf("the server rejected the address or password (%s)", authErr.Condition), "FI.MAU.XMPP.BAD_CREDENTIALS", 403)
	case errors.As(err, &tlsErr):
		return bridgev2.WrapRespErrManual(fmt.Errorf("could not establish a trusted TLS connection: %w", tlsErr.Err), "FI.MAU.XMPP.TLS_ERROR", 502)
	case errors.As(err, &dialErr):
		return bridgev2.WrapRespErrManual(err, "FI.MAU.XMPP.CONNECT_FAILED", 502)
	default:
		return bridgev2.WrapRespErrManual(err, "FI.MAU.XMPP.LOGIN_FAILED", 500)
	}
}

func (pl *PasswordLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	account, err := validateAccountJID(input["jid"])
	if err != nil {
		return nil, bridgev2.WrapRespErrManual(err, "M_INVALID_PARAM", 400)
	}
	password := input["password"]
	if password == "" {
		return nil, bridgev2.WrapRespErrManual(errors.New("password is required"), "M_INVALID_PARAM", 400)
	}
	server, err := validateServerInput(input["server"])
	if err != nil {
		return nil, bridgev2.WrapRespErrManual(err, "M_INVALID_PARAM", 400)
	}
	meta := &UserLoginMetadata{
		Password: password,
		Server:   server,
		Resource: pl.main.newResource(),
	}
	log := zerolog.Ctx(ctx).With().Stringer("jid", account).Logger()
	log.Info().Msg("Checking XMPP credentials")

	// Verify the credentials with a throwaway session before saving them.
	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(pl.main.Config.ConnectTimeout)+15*time.Second)
	defer cancel()
	sess, err := xmppx.Connect(checkCtx, &xmppx.Config{
		JID:            account,
		Password:       password,
		Resource:       meta.Resource + ".login",
		Server:         server,
		Proxy:          pl.main.Config.Proxy,
		ConnectTimeout: time.Duration(pl.main.Config.ConnectTimeout),
		SoftwareName:   "mautrix-xmpp",
	}, log, nopHandler{}, nil)
	if err != nil {
		log.Info().Err(err).Msg("XMPP login failed")
		return nil, loginRespError(err)
	}
	bound := sess.JID()
	sess.Close()

	ul, err := pl.user.NewLogin(ctx, &database.UserLogin{
		ID:         makeUserLoginID(account),
		RemoteName: account.String(),
		Metadata:   meta,
	}, &bridgev2.NewLoginParams{
		DeleteOnConflict: true,
		LoadUserLogin: func(ctx context.Context, login *bridgev2.UserLogin) error {
			login.Client = newXMPPClient(pl.main, login, account, meta)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to save login: %w", err)
	}
	ul.Client.Connect(ul.Log.WithContext(context.Background()))
	log.Info().Stringer("bound_jid", bound).Msg("XMPP login successful")
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       loginStepComplete,
		Instructions: fmt.Sprintf("Successfully logged in as %s", account),
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: ul.ID,
			UserLogin:   ul,
		},
	}, nil
}

func (pl *PasswordLogin) Cancel() {}

type nopHandler struct{}

func (nopHandler) HandleMessage(*xmppx.Session, *xmppx.Message)        {}
func (nopHandler) HandlePresence(*xmppx.Session, *xmppx.Presence)      {}
func (nopHandler) HandleRosterPush(*xmppx.Session, []xmppx.RosterItem) {}
