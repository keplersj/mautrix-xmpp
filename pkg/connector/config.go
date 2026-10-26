package connector

import (
	_ "embed"
	"fmt"
	"net"
	"net/url"
	"time"

	up "go.mau.fi/util/configupgrade"
	"gopkg.in/yaml.v3"
)

//go:embed example-config.yaml
var ExampleConfig string

// Duration is a time.Duration that unmarshals from Go duration strings like
// "30s" or "5m".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// Config is the network-specific config under `network:` in config.yaml.
type Config struct {
	ResourcePrefix string   `yaml:"resource_prefix"`
	ConnectTimeout Duration `yaml:"connect_timeout"`
	Reconnect      struct {
		InitialDelay Duration `yaml:"initial_delay"`
		MaxDelay     Duration `yaml:"max_delay"`
	} `yaml:"reconnect"`
	PingInterval     Duration `yaml:"ping_interval"`
	Proxy            string   `yaml:"proxy"`
	PresencePriority int8     `yaml:"presence_priority"`
	MAM              struct {
		PageSize   int `yaml:"page_size"`
		CatchupMax int `yaml:"catchup_max"`
	} `yaml:"mam"`
	Media struct {
		MaxDownloadSize int64 `yaml:"max_download_size"`
		MaxUploadSize   int64 `yaml:"max_upload_size"`
	} `yaml:"media"`
	SendReadMarkers   bool `yaml:"send_read_markers"`
	SendTyping        bool `yaml:"send_typing"`
	LogMessageContent bool `yaml:"log_message_content"`
	Metrics           struct {
		Enabled bool   `yaml:"enabled"`
		Listen  string `yaml:"listen"`
		Pprof   bool   `yaml:"pprof"`
	} `yaml:"metrics"`
}

// Validate checks the config for values that would make the bridge misbehave.
func (c *Config) Validate() error {
	switch {
	case c.ResourcePrefix == "":
		return fmt.Errorf("network.resource_prefix must not be empty")
	case c.ConnectTimeout <= 0:
		return fmt.Errorf("network.connect_timeout must be positive")
	case c.Reconnect.InitialDelay <= 0:
		return fmt.Errorf("network.reconnect.initial_delay must be positive")
	case c.Reconnect.MaxDelay < c.Reconnect.InitialDelay:
		return fmt.Errorf("network.reconnect.max_delay must be at least initial_delay")
	case c.PingInterval < 0:
		return fmt.Errorf("network.ping_interval must not be negative")
	case c.PresencePriority < 0:
		return fmt.Errorf("network.presence_priority must not be negative, or messages to your bare JID won't reach the bridge")
	case c.MAM.PageSize <= 0 || c.MAM.PageSize > 1000:
		return fmt.Errorf("network.mam.page_size must be between 1 and 1000")
	case c.MAM.CatchupMax < 0:
		return fmt.Errorf("network.mam.catchup_max must not be negative")
	case c.Media.MaxDownloadSize <= 0 || c.Media.MaxUploadSize <= 0:
		return fmt.Errorf("network.media limits must be positive")
	}
	if c.Proxy != "" {
		if err := validateProxyURL(c.Proxy); err != nil {
			return fmt.Errorf("network.proxy: %w", err)
		}
	}
	if c.Metrics.Enabled {
		if _, _, err := net.SplitHostPort(c.Metrics.Listen); err != nil {
			return fmt.Errorf("network.metrics.listen must be host:port: %w", err)
		}
	}
	return nil
}

func validateProxyURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if u.Scheme != "socks5" && u.Scheme != "socks5h" {
		return fmt.Errorf("unsupported scheme %q, only socks5:// and socks5h:// are supported", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("missing host")
	}
	return nil
}

func upgradeConfig(helper up.Helper) {
	helper.Copy(up.Str, "resource_prefix")
	helper.Copy(up.Str, "connect_timeout")
	helper.Copy(up.Str, "reconnect", "initial_delay")
	helper.Copy(up.Str, "reconnect", "max_delay")
	helper.Copy(up.Str, "ping_interval")
	helper.Copy(up.Str, "proxy")
	helper.Copy(up.Int, "presence_priority")
	helper.Copy(up.Int, "mam", "page_size")
	helper.Copy(up.Int, "mam", "catchup_max")
	helper.Copy(up.Int, "media", "max_download_size")
	helper.Copy(up.Int, "media", "max_upload_size")
	helper.Copy(up.Bool, "send_read_markers")
	helper.Copy(up.Bool, "send_typing")
	helper.Copy(up.Bool, "log_message_content")
	helper.Copy(up.Bool, "metrics", "enabled")
	helper.Copy(up.Str, "metrics", "listen")
	helper.Copy(up.Bool, "metrics", "pprof")
}
