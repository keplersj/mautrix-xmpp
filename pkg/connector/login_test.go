package connector

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"maunium.net/go/mautrix/bridgev2"

	"github.com/keplersj/mautrix-xmpp/pkg/xmppx"
)

func TestValidateJIDInput(t *testing.T) {
	valid := map[string]string{
		"alice@example.org":          "alice@example.org",
		"  Alice@Example.ORG ":       "alice@example.org",
		"alice@example.org/resource": "alice@example.org",
		"bob@localhost":              "bob@localhost",
	}
	for in, want := range valid {
		got, err := validateJIDInput(in)
		if err != nil || got != want {
			t.Errorf("validateJIDInput(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "example.org", "@example.org", "alice@", "alice@nodots", "a@b@c", "alice@exa mple.org"} {
		if _, err := validateJIDInput(in); err == nil {
			t.Errorf("validateJIDInput(%q) should fail", in)
		}
	}
}

func TestValidateServerInput(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "",
		"  ":                     "",
		"xmpp.example.org":       "xmpp.example.org",
		"xmpp.example.org:5222":  "xmpp.example.org:5222",
		"tls://xmpp.example.org": "tls://xmpp.example.org",
		"[::1]:5223":             "[::1]:5223",
	} {
		got, err := validateServerInput(in)
		if err != nil || got != want {
			t.Errorf("validateServerInput(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"https://example.org", "example.org:0", "example.org:99999", "tls://", ":5222"} {
		if _, err := validateServerInput(in); err == nil {
			t.Errorf("validateServerInput(%q) should fail", in)
		}
	}
}

func TestLoginFlow(t *testing.T) {
	xc := &XMPPConnector{Config: testConfig(t)}
	flows := xc.GetLoginFlows()
	if len(flows) != 1 || flows[0].ID != loginFlowPassword {
		t.Fatalf("flows = %+v", flows)
	}
	if _, err := xc.CreateLogin(t.Context(), nil, "qr"); !errors.Is(err, bridgev2.ErrInvalidLoginFlowID) {
		t.Errorf("unknown flow: %v", err)
	}
	proc, err := xc.CreateLogin(t.Context(), nil, loginFlowPassword)
	if err != nil {
		t.Fatal(err)
	}
	step, err := proc.Start(t.Context())
	if err != nil || step.Type != bridgev2.LoginStepTypeUserInput {
		t.Fatalf("start: %+v %v", step, err)
	}
	fields := step.UserInputParams.Fields
	if len(fields) != 3 || fields[0].ID != "jid" || fields[1].Type != bridgev2.LoginInputFieldTypePassword || !fields[2].Optional {
		t.Errorf("fields = %+v", fields)
	}
	// Bad input is rejected before any connection attempt.
	pl := proc.(bridgev2.LoginProcessUserInput)
	for _, input := range []map[string]string{
		{"jid": "not a jid", "password": "x"},
		{"jid": "alice@example.org", "password": ""},
		{"jid": "alice@example.org", "password": "x", "server": "https://nope"},
	} {
		_, err := pl.SubmitUserInput(t.Context(), input)
		var re bridgev2.RespError
		if !errors.As(err, &re) || re.StatusCode != http.StatusBadRequest {
			t.Errorf("input %v: expected 400, got %v", input, err)
		}
	}
}

func TestLoginRespError(t *testing.T) {
	for _, tt := range []struct {
		err    error
		status int
		code   string
	}{
		{&xmppx.AuthError{Condition: "not-authorized"}, 403, "FI.MAU.XMPP.BAD_CREDENTIALS"},
		{&xmppx.TLSError{Err: errors.New("x509: certificate signed by unknown authority")}, 502, "FI.MAU.XMPP.TLS_ERROR"},
		{&xmppx.DialError{Errs: []error{errors.New("refused")}}, 502, "FI.MAU.XMPP.CONNECT_FAILED"},
	} {
		var re bridgev2.RespError
		if !errors.As(loginRespError(tt.err), &re) || re.StatusCode != tt.status || re.ErrCode != tt.code {
			t.Errorf("%T: got %+v", tt.err, re)
		}
	}
	// Passwords never end up in error messages.
	msg := loginRespError(&xmppx.AuthError{Condition: "not-authorized"}).Error()
	if strings.Contains(msg, "hunter2") {
		t.Error("password leaked")
	}
}
