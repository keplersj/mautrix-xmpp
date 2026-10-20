package xmppx

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/net/proxy"
	"mellium.im/xmpp/jid"
)

// endpoint is one way to reach the server.
type endpoint struct {
	addr      string // host:port
	directTLS bool   // XEP-0368 implicit TLS rather than STARTTLS
}

func (e endpoint) String() string {
	if e.directTLS {
		return "tls://" + e.addr
	}
	return e.addr
}

// ParseServerOverride parses a per-login server override: "host", "host:port"
// (STARTTLS) or "tls://host:port" (direct TLS).
func ParseServerOverride(s string) (endpoint, error) {
	s = strings.TrimSpace(s)
	direct := false
	switch {
	case strings.HasPrefix(s, "tls://"):
		direct = true
		s = strings.TrimPrefix(s, "tls://")
	case strings.HasPrefix(s, "xmpp://"), strings.HasPrefix(s, "tcp://"):
		s = s[strings.Index(s, "://")+3:]
	case strings.Contains(s, "://"):
		return endpoint{}, fmt.Errorf("unsupported scheme in server address %q (use host:port or tls://host:port)", s)
	}
	if s == "" {
		return endpoint{}, errors.New("empty server address")
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		// No port given.
		host = s
		if direct {
			port = "5223"
		} else {
			port = "5222"
		}
	}
	if host == "" {
		return endpoint{}, fmt.Errorf("missing host in server address %q", s)
	}
	if p, err := strconv.Atoi(port); err != nil || p <= 0 || p > 65535 {
		return endpoint{}, fmt.Errorf("invalid port in server address %q", s)
	}
	return endpoint{addr: net.JoinHostPort(host, port), directTLS: direct}, nil
}

type srvLookuper interface {
	LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
}

// resolveEndpoints finds the candidate endpoints for a domain using SRV
// records (RFC 6120 §3.2.1 and XEP-0368), falling back to domain:5222.
func resolveEndpoints(ctx context.Context, r srvLookuper, domain string) []endpoint {
	type cand struct {
		endpoint
		prio, weight uint16
	}
	var cands []cand
	disabled := map[bool]bool{}
	for _, direct := range []bool{true, false} {
		service := "xmpp-client"
		if direct {
			service = "xmpps-client"
		}
		_, srvs, err := r.LookupSRV(ctx, service, "tcp", domain)
		if err != nil {
			continue
		}
		for _, srv := range srvs {
			if srv.Target == "." {
				// RFC 2782: service explicitly not available.
				disabled[direct] = true
				continue
			}
			cands = append(cands, cand{
				endpoint: endpoint{addr: net.JoinHostPort(strings.TrimSuffix(srv.Target, "."), strconv.Itoa(int(srv.Port))), directTLS: direct},
				prio:     srv.Priority,
				weight:   srv.Weight,
			})
		}
	}
	// Lower priority first. On ties prefer direct TLS (XEP-0368 §3), then the
	// higher weight. Weighted random selection isn't worth it for one client.
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].prio != cands[j].prio {
			return cands[i].prio < cands[j].prio
		}
		if cands[i].directTLS != cands[j].directTLS {
			return cands[i].directTLS
		}
		return cands[i].weight > cands[j].weight
	})
	out := make([]endpoint, 0, len(cands)+1)
	for _, c := range cands {
		out = append(out, c.endpoint)
	}
	if len(out) == 0 && !disabled[false] {
		out = append(out, endpoint{addr: net.JoinHostPort(domain, "5222")})
	}
	return out
}

// contextDialer is the subset of proxy.ContextDialer and net.Dialer we use.
type contextDialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

func makeDialer(proxyURL string) (contextDialer, error) {
	base := &net.Dialer{}
	if proxyURL == "" {
		return base, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %w", err)
	}
	switch u.Scheme {
	case "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q (only socks5:// and socks5h:// are supported for XMPP connections)", u.Scheme)
	}
	d, err := proxy.FromURL(u, base)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %w", err)
	}
	cd, ok := d.(contextDialer)
	if !ok {
		return nil, errors.New("proxy dialer does not support contexts")
	}
	return cd, nil
}

// tlsConfigFor returns the TLS config for connecting to the given XMPP domain.
// Certificates are always verified against the JID's domain (RFC 7590 §3.2),
// never against the SRV target.
func tlsConfigFor(base *tls.Config, domain string) *tls.Config {
	var cfg *tls.Config
	if base != nil {
		cfg = base.Clone()
	} else {
		cfg = &tls.Config{}
	}
	cfg.ServerName = domain
	cfg.InsecureSkipVerify = false
	if cfg.MinVersion < tls.VersionTLS12 {
		cfg.MinVersion = tls.VersionTLS12
	}
	return cfg
}

// dial connects to the first reachable endpoint. For direct TLS endpoints the
// returned connection has already completed the TLS handshake.
func dial(ctx context.Context, d contextDialer, endpoints []endpoint, j jid.JID, tlsBase *tls.Config) (net.Conn, endpoint, error) {
	if len(endpoints) == 0 {
		return nil, endpoint{}, fmt.Errorf("no XMPP client service found for %s", j.Domainpart())
	}
	var errs []error
	for _, ep := range endpoints {
		conn, err := d.DialContext(ctx, "tcp", ep.addr)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ep, err))
			continue
		}
		if !ep.directTLS {
			return conn, ep, nil
		}
		cfg := tlsConfigFor(tlsBase, j.Domainpart())
		cfg.NextProtos = []string{"xmpp-client"}
		tlsConn := tls.Client(conn, cfg)
		if err = tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			// A certificate problem is not something another endpoint will fix
			// silently, but trying the rest is still correct: report all errors.
			errs = append(errs, fmt.Errorf("%s: %w", ep, err))
			continue
		}
		return tlsConn, ep, nil
	}
	return nil, endpoint{}, &DialError{Errs: errs}
}

// DialError is returned when no endpoint could be reached.
type DialError struct {
	Errs []error
}

func (e *DialError) Error() string {
	parts := make([]string, len(e.Errs))
	for i, err := range e.Errs {
		parts[i] = err.Error()
	}
	return "failed to connect: " + strings.Join(parts, "; ")
}

func (e *DialError) Unwrap() []error {
	return e.Errs
}
