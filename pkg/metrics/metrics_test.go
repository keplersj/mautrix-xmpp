package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"mellium.im/xmpp/stanza"
	"mellium.im/xmpp/stream"

	"github.com/keplersj/mautrix-xmpp/pkg/xmppx"
)

func TestDisconnectReason(t *testing.T) {
	for want, err := range map[string]error{
		ReasonBadCredentials: &xmppx.AuthError{Condition: "not-authorized"},
		ReasonAuthTemporary:  &xmppx.AuthError{Condition: "temporary-auth-failure"},
		ReasonTLS:            &xmppx.TLSError{Err: errors.New("bad cert")},
		ReasonConflict:       fmt.Errorf("serve: %w", stream.Conflict),
		ReasonStreamError:    stream.SystemShutdown,
		ReasonTimeout:        fmt.Errorf("ping: %w", context.DeadlineExceeded),
		ReasonNetwork:        &net.OpError{Op: "dial", Err: errors.New("connection refused")},
		ReasonEOF:            io.EOF,
		ReasonOther:          errors.New("closed"),
	} {
		if got := DisconnectReason(err); got != want {
			t.Errorf("DisconnectReason(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestIQResult(t *testing.T) {
	for want, err := range map[string]error{
		"ok":      nil,
		"error":   stanza.Error{Condition: stanza.ItemNotFound},
		"timeout": context.DeadlineExceeded,
		"failed":  errors.New("session closed"),
	} {
		if got := iqResult(err); got != want {
			t.Errorf("iqResult(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestNilMetricsIsSafe(t *testing.T) {
	var m *Metrics
	m.SetSnapshotFunc(func() Snapshot { return Snapshot{} })
	m.ConnectAttempt()
	m.Disconnect(ReasonEOF)
	m.MatrixSend(SendOK, time.Second)
	m.MediaTransfer(MediaUpload, MediaOK, 10)
	m.Stanza(true, "message")
	m.IQDone(xmppx.IQOpPing, time.Second, nil)
	m.MAMPage(3)
	m.Resent(1)
}

func TestMetricsValues(t *testing.T) {
	m := New("1.2.3", "abc")
	m.SetSnapshotFunc(func() Snapshot {
		return Snapshot{Logins: map[string]int{StateConnected: 2, StateBadCredentials: 1}, Unacked: 4}
	})
	m.Stanza(false, "message")
	m.Stanza(false, "message")
	m.Stanza(true, "iq")
	m.Disconnect(ReasonTLS)
	m.MediaTransfer(MediaDownload, MediaOK, 100)
	m.MediaTransfer(MediaDownload, MediaTooLarge, 999)

	if v := testutil.ToFloat64(m.stanzas.WithLabelValues("in", "message")); v != 2 {
		t.Errorf("inbound messages = %v", v)
	}
	if v := testutil.ToFloat64(m.disconnects.WithLabelValues(ReasonTLS)); v != 1 {
		t.Errorf("tls disconnects = %v", v)
	}
	if v := testutil.ToFloat64(m.mediaBytes.WithLabelValues(MediaDownload)); v != 100 {
		t.Errorf("download bytes = %v, failed transfers must not count", v)
	}
	want := `
# HELP mautrix_xmpp_logins Logged in XMPP accounts by connection state.
# TYPE mautrix_xmpp_logins gauge
mautrix_xmpp_logins{state="bad_credentials"} 1
mautrix_xmpp_logins{state="connected"} 2
mautrix_xmpp_logins{state="connecting"} 0
mautrix_xmpp_logins{state="disconnected"} 0
mautrix_xmpp_logins{state="stopped"} 0
# HELP mautrix_xmpp_unacked_stanzas Outbound stanzas waiting for a stream management (XEP-0198) ack.
# TYPE mautrix_xmpp_unacked_stanzas gauge
mautrix_xmpp_unacked_stanzas 4
`
	if err := testutil.GatherAndCompare(m.reg, strings.NewReader(want), "mautrix_xmpp_logins", "mautrix_xmpp_unacked_stanzas"); err != nil {
		t.Error(err)
	}
}

func get(t *testing.T, s *Server, path string) (int, string) {
	t.Helper()
	resp, err := http.Get("http://" + s.Addr().String() + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func startServer(t *testing.T, pprof bool) *Server {
	t.Helper()
	s, err := Listen("127.0.0.1:0", New("1.2.3", "abc"), pprof, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	s.Serve()
	t.Cleanup(s.Close)
	return s
}

func TestServerRoutes(t *testing.T) {
	s := startServer(t, false)
	if code, _ := get(t, s, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d", code)
	}
	if code, _ := get(t, s, "/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("/readyz before ready = %d", code)
	}
	s.SetReady(true)
	if code, _ := get(t, s, "/readyz"); code != http.StatusOK {
		t.Errorf("/readyz after ready = %d", code)
	}
	code, body := get(t, s, "/metrics")
	if code != http.StatusOK || !strings.Contains(body, `mautrix_xmpp_build_info{commit="abc",version="1.2.3"} 1`) || !strings.Contains(body, "go_goroutines") {
		t.Errorf("/metrics = %d:\n%s", code, body)
	}
	// mautrix registers pprof on http.DefaultServeMux; it must not leak here.
	if code, _ := get(t, s, "/debug/pprof/"); code != http.StatusNotFound {
		t.Errorf("/debug/pprof/ with pprof disabled = %d", code)
	}
}

func TestServerPprof(t *testing.T) {
	s := startServer(t, true)
	if code, _ := get(t, s, "/debug/pprof/"); code != http.StatusOK {
		t.Errorf("/debug/pprof/ = %d", code)
	}
	if code, _ := get(t, s, "/debug/pprof/heap?debug=1"); code != http.StatusOK {
		t.Errorf("/debug/pprof/heap = %d", code)
	}
}
