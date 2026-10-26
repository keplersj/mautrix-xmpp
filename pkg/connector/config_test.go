package connector

import (
	"testing"
	"time"

	up "go.mau.fi/util/configupgrade"
	"gopkg.in/yaml.v3"
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
	if cfg.Metrics.Enabled || cfg.Metrics.Pprof || cfg.Metrics.Listen != "127.0.0.1:8001" {
		t.Errorf("example config must keep metrics off and on localhost: %+v", cfg.Metrics)
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
		"metrics listen":      func(c *Config) { c.Metrics.Enabled = true; c.Metrics.Listen = "8001" },
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

func TestConfigUpgradeKeepsMetrics(t *testing.T) {
	old := []byte("resource_prefix: custom\nmetrics:\n    enabled: true\n    listen: 0.0.0.0:9000\n    pprof: true\n")
	upgrader := &up.StructUpgrader{SimpleUpgrader: up.SimpleUpgrader(upgradeConfig), Base: ExampleConfig}
	out, err := up.DoBytes(old, upgrader)
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err = yaml.Unmarshal(out, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ResourcePrefix != "custom" || !cfg.Metrics.Enabled || !cfg.Metrics.Pprof || cfg.Metrics.Listen != "0.0.0.0:9000" {
		t.Errorf("upgrade lost settings: %+v", cfg.Metrics)
	}
	if cfg.MAM.PageSize != 50 {
		t.Errorf("upgrade did not fill defaults: page_size = %d", cfg.MAM.PageSize)
	}
}
