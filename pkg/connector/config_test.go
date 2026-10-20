package connector

import (
	"testing"
	"time"
)

func TestExampleConfigIsValid(t *testing.T) {
	cfg := testConfig(t)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("example config invalid: %v", err)
	}
	if time.Duration(cfg.ConnectTimeout) != 30*time.Second || time.Duration(cfg.Reconnect.MaxDelay) != 5*time.Minute {
		t.Errorf("durations not parsed: %+v", cfg)
	}
	if cfg.Proxy != "" || cfg.LogMessageContent {
		t.Error("example config must not enable proxies or stanza logging")
	}
}

func TestConfigValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"empty resource":      func(c *Config) { c.ResourcePrefix = "" },
		"zero timeout":        func(c *Config) { c.ConnectTimeout = 0 },
		"max below initial":   func(c *Config) { c.Reconnect.MaxDelay = Duration(time.Second) },
		"negative priority":   func(c *Config) { c.PresencePriority = -1 },
		"page size":           func(c *Config) { c.MAM.PageSize = 0 },
		"http proxy":          func(c *Config) { c.Proxy = "http://proxy:3128" },
		"proxy without host":  func(c *Config) { c.Proxy = "socks5://" },
		"zero download limit": func(c *Config) { c.Media.MaxDownloadSize = 0 },
	} {
		cfg := testConfig(t)
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
	cfg := testConfig(t)
	cfg.Proxy = "socks5h://user:pass@proxy:1080"
	if err := cfg.Validate(); err != nil {
		t.Errorf("socks5h proxy rejected: %v", err)
	}
}

func TestResolveProxy(t *testing.T) {
	xc := &XMPPConnector{Config: Config{Proxy: "socks5://default:1080"}}
	if p := xc.resolveProxy(&UserLoginMetadata{}); p != "socks5://default:1080" {
		t.Errorf("default proxy = %q", p)
	}
	if p := xc.resolveProxy(&UserLoginMetadata{Proxy: "socks5://mine:1080"}); p != "socks5://mine:1080" {
		t.Errorf("per-login proxy = %q", p)
	}
}
