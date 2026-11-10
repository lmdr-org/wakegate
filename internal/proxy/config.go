// Package proxy runs on a machine that is always on. It listens in place of the
// services of a computer that sleeps, wakes that computer when a request needs it, and
// relays the request once the service answers.
package proxy

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/lmdr-org/wakegate/internal/rules"
)

// Config is /etc/wakegate/proxy.yaml.
type Config struct {
	Target   Target      `yaml:"target"`
	Admin    AdminConfig `yaml:"admin"`
	Services []*Service  `yaml:"services"`
	// MaxBodyMB: request bodies up to this size are kept in memory, so a request can be
	// sent again if the computer went to sleep under it. Larger bodies are streamed and
	// sent once.
	MaxBodyMB int    `yaml:"max_body_mb"`
	StateDir  string `yaml:"state_dir"`
}

// Target is the computer that sleeps.
type Target struct {
	Name string `yaml:"name"` // shown on the admin page; the host by default
	Host string `yaml:"host"`
	MAC  string `yaml:"mac"`
	// Broadcast lists where the magic packet goes: "192.168.1.255" or
	// "192.168.1.255:7". 255.255.255.255 by default.
	Broadcast []string `yaml:"broadcast"`
	// WakeCommand replaces the magic packet: a program and its arguments, for a proxy
	// that cannot reach the target's network directly.
	WakeCommand []string `yaml:"wake_command"`
	// Agent is the URL of the agent on the target. Without it, the proxy only checks
	// that Probe accepts connections, and the target manages its own sleep.
	Agent          string        `yaml:"agent"`
	AgentTokenFile string        `yaml:"agent_token_file"`
	Probe          string        `yaml:"probe"`
	BootTimeout    time.Duration `yaml:"boot_timeout"`
	// ShutdownTimeout bounds the wait for a target that is going to sleep, before it is
	// woken again.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// AdminConfig is the optional admin page.
type AdminConfig struct {
	Listen string `yaml:"listen"` // empty: no admin page
}

// Service is one port the proxy listens on.
type Service struct {
	Name     string `yaml:"name"`
	Listen   string `yaml:"listen"`
	Upstream string `yaml:"upstream"` // a port on the target, or host:port
	// TCP relays raw connections (SSH, rsync...) instead of HTTP requests. Every
	// connection wakes the target and holds it awake until it closes.
	TCP bool `yaml:"tcp"`
	// WakeOn lists the requests that wake the target and hold it awake; all requests
	// when empty. NeverWake lists requests that never do, even if WakeOn matches them.
	WakeOn    []string `yaml:"wake_on"`
	NeverWake []string `yaml:"never_wake"`
	// WaitingPage answers a browser with a page that reloads until the service is up,
	// instead of holding the request. On by default.
	WaitingPage *bool `yaml:"waiting_page"`
	// Health is a path that answers 2xx once the service is ready, checked after the
	// target wakes up. Without it, the proxy retries the request until it goes through.
	Health       string        `yaml:"health"`
	StartTimeout time.Duration `yaml:"start_timeout"`

	wakeOn, neverWake rules.Set
	addr              string // upstream host:port
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

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
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("proxy config: %w", err)
	}
	if err := c.complete(); err != nil {
		return nil, fmt.Errorf("proxy config: %w", err)
	}
	return &c, nil
}

func (c *Config) complete() error {
	t := &c.Target
	if t.Host == "" {
		return fmt.Errorf("target.host is required")
	}
	if t.Name == "" {
		t.Name = t.Host
	}
	if len(t.WakeCommand) == 0 {
		if _, err := net.ParseMAC(t.MAC); err != nil {
			return fmt.Errorf("target.mac %q: a MAC address such as 04:d9:f5:cb:75:23 (or set target.wake_command)", t.MAC)
		}
	}
	if len(t.Broadcast) == 0 {
		t.Broadcast = []string{"255.255.255.255"}
	}
	for _, b := range t.Broadcast {
		host := b
		if h, port, err := net.SplitHostPort(b); err == nil {
			if _, err := strconv.ParseUint(port, 10, 16); err != nil {
				return fmt.Errorf("target.broadcast %q: bad port", b)
			}
			host = h
		}
		if net.ParseIP(host).To4() == nil {
			return fmt.Errorf("target.broadcast %q: an IPv4 address", b)
		}
	}
	if t.Agent != "" {
		u, err := url.Parse(t.Agent)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("target.agent %q: a URL such as http://%s:9777", t.Agent, t.Host)
		}
		t.Agent = strings.TrimSuffix(t.Agent, "/")
		if t.AgentTokenFile == "" {
			t.AgentTokenFile = "/etc/wakegate/agent.token"
		}
	}
	if t.BootTimeout == 0 {
		t.BootTimeout = 3 * time.Minute
	}
	if t.ShutdownTimeout == 0 {
		t.ShutdownTimeout = 3 * time.Minute
	}
	if t.BootTimeout < 10*time.Second || t.ShutdownTimeout < 10*time.Second {
		return fmt.Errorf("target.boot_timeout and target.shutdown_timeout: at least 10s")
	}
	if c.MaxBodyMB == 0 {
		c.MaxBodyMB = 16
	}
	if c.MaxBodyMB < 0 {
		return fmt.Errorf("max_body_mb: 0 or more")
	}
	if c.StateDir == "" {
		c.StateDir = "/var/lib/wakegate"
	}
	if c.Admin.Listen != "" {
		listen, err := normalizeListen(c.Admin.Listen)
		if err != nil {
			return fmt.Errorf("admin.listen: %w", err)
		}
		c.Admin.Listen = listen
	}
	if len(c.Services) == 0 {
		return fmt.Errorf("services: at least one")
	}
	names, listens := map[string]bool{}, map[string]string{c.Admin.Listen: "admin"}
	for i, s := range c.Services {
		if s == nil {
			return fmt.Errorf("services[%d]: empty", i)
		}
		if err := s.complete(t.Host); err != nil {
			return fmt.Errorf("service %q: %w", s.Name, err)
		}
		if names[s.Name] {
			return fmt.Errorf("service %q: the name is used twice", s.Name)
		}
		names[s.Name] = true
		if other, taken := listens[s.Listen]; taken && s.Listen != "" {
			return fmt.Errorf("service %q: listen %s is already used by %s", s.Name, s.Listen, other)
		}
		listens[s.Listen] = s.Name
	}
	if t.Agent == "" && t.Probe == "" {
		t.Probe = c.Services[0].addr
	}
	if t.Probe != "" {
		if _, _, err := net.SplitHostPort(t.Probe); err != nil {
			return fmt.Errorf("target.probe %q: host:port", t.Probe)
		}
	}
	return nil
}

func (s *Service) complete(host string) error {
	if !namePattern.MatchString(s.Name) {
		return fmt.Errorf("name: lower-case letters, digits, - and _")
	}
	listen, err := normalizeListen(s.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	s.Listen = listen
	s.addr, err = upstreamAddr(s.Upstream, host)
	if err != nil {
		return fmt.Errorf("upstream: %w", err)
	}
	if s.TCP && (len(s.WakeOn) > 0 || len(s.NeverWake) > 0 || s.WaitingPage != nil || s.Health != "") {
		return fmt.Errorf("wake_on, never_wake, waiting_page and health only apply to HTTP services")
	}
	if s.wakeOn, err = rules.ParseSet(s.WakeOn); err != nil {
		return fmt.Errorf("wake_on: %w", err)
	}
	if s.neverWake, err = rules.ParseSet(s.NeverWake); err != nil {
		return fmt.Errorf("never_wake: %w", err)
	}
	if s.WaitingPage == nil {
		yes := !s.TCP
		s.WaitingPage = &yes
	}
	if s.Health != "" && !strings.HasPrefix(s.Health, "/") {
		return fmt.Errorf("health %q: a path such as /health", s.Health)
	}
	if s.StartTimeout == 0 {
		s.StartTimeout = time.Minute
	}
	if s.StartTimeout < time.Second {
		return fmt.Errorf("start_timeout: at least 1s")
	}
	return nil
}

// Wakes reports whether a request may wake the target and holds it awake.
func (s *Service) Wakes(method, path string) bool {
	if s.neverWake.Match(method, path) {
		return false
	}
	return len(s.wakeOn) == 0 || s.wakeOn.Match(method, path)
}

// Addr is the upstream host:port.
func (s *Service) Addr() string { return s.addr }

// normalizeListen accepts "8080", ":8080" or "127.0.0.1:8080".
func normalizeListen(v string) (string, error) {
	if v == "" {
		return "", fmt.Errorf("required, such as \":8080\"")
	}
	if !strings.Contains(v, ":") {
		v = ":" + v
	}
	_, port, err := net.SplitHostPort(v)
	if err != nil {
		return "", err
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return "", fmt.Errorf("%q: bad port", v)
	}
	return v, nil
}

// upstreamAddr accepts "11434" or ":11434" (a port on the target) or "host:port".
func upstreamAddr(v, host string) (string, error) {
	if v == "" {
		return "", fmt.Errorf("required: a port on the target, or host:port")
	}
	if !strings.Contains(v, ":") {
		v = ":" + v
	}
	h, port, err := net.SplitHostPort(v)
	if err != nil {
		return "", err
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return "", fmt.Errorf("%q: bad port", v)
	}
	if h == "" {
		h = host
	}
	return net.JoinHostPort(h, port), nil
}

// ReadAgentToken reads the agent token from WAKEGATE_AGENT_TOKEN, or else from the file.
func (c *Config) ReadAgentToken() (string, error) {
	token := os.Getenv("WAKEGATE_AGENT_TOKEN")
	if token == "" {
		raw, err := os.ReadFile(c.Target.AgentTokenFile)
		if err != nil {
			return "", fmt.Errorf("agent token: %w (copy the agent's token file, or set WAKEGATE_AGENT_TOKEN)", err)
		}
		token = strings.TrimSpace(string(raw))
	}
	if len(token) < 16 {
		return "", fmt.Errorf("agent token: at least 16 characters")
	}
	return token, nil
}
