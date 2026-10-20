package xmppx

import (
	"context"
	"errors"
	"net"
	"testing"
)

type fakeResolver map[string][]*net.SRV

func (f fakeResolver) LookupSRV(_ context.Context, service, proto, name string) (string, []*net.SRV, error) {
	srvs, ok := f[service]
	if !ok {
		return "", nil, errors.New("no such host")
	}
	return "", srvs, nil
}

func endpointsString(eps []endpoint) []string {
	out := make([]string, len(eps))
	for i, e := range eps {
		out[i] = e.String()
	}
	return out
}

func TestResolveEndpoints(t *testing.T) {
	tests := []struct {
		name string
		r    fakeResolver
		want []string
	}{
		{"no SRV", fakeResolver{}, []string{"example.org:5222"}},
		{"starttls only", fakeResolver{"xmpp-client": {{Target: "xmpp.example.org.", Port: 5222, Priority: 5}}}, []string{"xmpp.example.org:5222"}},
		{"direct TLS preferred on equal priority", fakeResolver{
			"xmpp-client":  {{Target: "a.example.org.", Port: 5222, Priority: 5}},
			"xmpps-client": {{Target: "b.example.org.", Port: 443, Priority: 5}},
		}, []string{"tls://b.example.org:443", "a.example.org:5222"}},
		{"priority wins", fakeResolver{
			"xmpp-client":  {{Target: "a.example.org.", Port: 5222, Priority: 1}},
			"xmpps-client": {{Target: "b.example.org.", Port: 5223, Priority: 10}},
		}, []string{"a.example.org:5222", "tls://b.example.org:5223"}},
		{"service disabled", fakeResolver{"xmpp-client": {{Target: ".", Port: 0}}}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := endpointsString(resolveEndpoints(context.Background(), tt.r, "example.org"))
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestParseServerOverride(t *testing.T) {
	for in, want := range map[string]string{
		"xmpp.example.org":           "xmpp.example.org:5222",
		"xmpp.example.org:5269":      "xmpp.example.org:5269",
		"tls://xmpp.example.org":     "tls://xmpp.example.org:5223",
		"tls://xmpp.example.org:443": "tls://xmpp.example.org:443",
		"xmpp://xmpp.example.org":    "xmpp.example.org:5222",
	} {
		ep, err := ParseServerOverride(in)
		if err != nil || ep.String() != want {
			t.Errorf("ParseServerOverride(%q) = %v, %v; want %s", in, ep, err, want)
		}
	}
	for _, in := range []string{"", "https://x", "x:abc", ":5222"} {
		if _, err := ParseServerOverride(in); err == nil {
			t.Errorf("ParseServerOverride(%q) should fail", in)
		}
	}
}

func TestTLSConfigFor(t *testing.T) {
	cfg := tlsConfigFor(nil, "example.org")
	if cfg.ServerName != "example.org" || cfg.InsecureSkipVerify || cfg.MinVersion == 0 {
		t.Errorf("tls config = %+v", cfg)
	}
}

func TestMakeDialer(t *testing.T) {
	if _, err := makeDialer(""); err != nil {
		t.Error(err)
	}
	if _, err := makeDialer("socks5://127.0.0.1:1080"); err != nil {
		t.Error(err)
	}
	if _, err := makeDialer("http://127.0.0.1:3128"); err == nil {
		t.Error("http proxies are not supported for XMPP")
	}
}
