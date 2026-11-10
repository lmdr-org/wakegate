// Package agent runs on the computer that sleeps. It tells the proxy whether the
// computer is ready, and puts it to sleep once nothing has needed it for a while.
package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/clauer-org/wakegate/internal/api"
)

// Config is /etc/wakegate/agent.yaml.
type Config struct {
	Listen      string        `yaml:"listen"`
	TokenFile   string        `yaml:"token_file"`
	IdleTimeout time.Duration `yaml:"idle_timeout"`
	Sleep       string        `yaml:"sleep"`
	KeepAwake   KeepAwake     `yaml:"keep_awake"`
	StateDir    string        `yaml:"state_dir"`
}

// KeepAwake is what holds the computer awake besides requests through the proxy.
type KeepAwake struct {
	// Sessions: "all" (any logged-in user), "active" (users logind does not report as
	// idle) or "none".
	Sessions string `yaml:"sessions"`
	// Inhibitors: systemd sleep inhibitors in "block" mode, as taken by
	// systemd-inhibit --what=sleep.
	Inhibitors *bool `yaml:"inhibitors"`
	// Commands run with sh -c; each one that exits 0 means work is in progress.
	Commands []string `yaml:"commands"`
}

// Sessions policies.
const (
	SessionsAll    = "all"
	SessionsActive = "active"
	SessionsNone   = "none"
)

// LoadConfig reads, completes and checks the configuration.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseConfig(raw)
}

// ParseConfig parses a configuration; unknown fields are errors.
func ParseConfig(raw []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	// An empty file leaves every setting at its default.
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("agent config: %w", err)
	}
	if c.Listen == "" {
		c.Listen = ":9777"
	}
	if c.TokenFile == "" {
		c.TokenFile = "/etc/wakegate/agent.token"
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 30 * time.Minute
	}
	if c.Sleep == "" {
		c.Sleep = api.Suspend
	}
	if c.KeepAwake.Sessions == "" {
		c.KeepAwake.Sessions = SessionsAll
	}
	if c.KeepAwake.Inhibitors == nil {
		yes := true
		c.KeepAwake.Inhibitors = &yes
	}
	if c.StateDir == "" {
		c.StateDir = "/var/lib/wakegate"
	}
	if c.IdleTimeout < time.Minute || c.IdleTimeout%time.Minute != 0 || c.IdleTimeout > MaxIdle {
		return nil, fmt.Errorf("agent config: idle_timeout %s: a whole number of minutes, from 1m to 24h", c.IdleTimeout)
	}
	switch c.Sleep {
	case api.Suspend, api.Hibernate, api.PowerOff, api.None:
	default:
		return nil, fmt.Errorf("agent config: sleep %q: suspend, hibernate, poweroff or none", c.Sleep)
	}
	switch c.KeepAwake.Sessions {
	case SessionsAll, SessionsActive, SessionsNone:
	default:
		return nil, fmt.Errorf("agent config: keep_awake.sessions %q: all, active or none", c.KeepAwake.Sessions)
	}
	for _, cmd := range c.KeepAwake.Commands {
		if strings.TrimSpace(cmd) == "" {
			return nil, fmt.Errorf("agent config: keep_awake.commands: empty command")
		}
	}
	return &c, nil
}

// MaxIdle bounds the idle timeout, from the configuration or POST /v1/idle.
const MaxIdle = 24 * time.Hour

// ReadToken reads the token from WAKEGATE_TOKEN, or else from path.
func ReadToken(path string) (string, error) {
	token := os.Getenv("WAKEGATE_TOKEN")
	if token == "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("token: %w (set WAKEGATE_TOKEN or create the file)", err)
		}
		token = strings.TrimSpace(string(raw))
	}
	if len(token) < 16 {
		return "", fmt.Errorf("token: at least 16 characters")
	}
	return token, nil
}
