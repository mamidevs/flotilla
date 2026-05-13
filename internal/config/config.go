// Package config parses Flotilla's YAML config + CLI flags and validates
// the result into a single Config struct that callers can rely on.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mamidevs/flotilla/internal/worker"
)

// AuthMethod tells the worker how to obtain a Tailscale auth key.
type AuthMethod string

const (
	AuthMethodAuthKey AuthMethod = "authkey"
	AuthMethodOAuth2  AuthMethod = "oauth2"
)

// DispatchStrategy controls how the front-end dispatcher picks a worker.
type DispatchStrategy string

const (
	StrategyRoundRobin  DispatchStrategy = "round_robin"
	StrategyRandom      DispatchStrategy = "random"
	StrategySticky      DispatchStrategy = "sticky"
	StrategyLeastActive DispatchStrategy = "least_active"
	StrategyTagged      DispatchStrategy = "tagged"
)

// Config is the top-level configuration for a Flotilla daemon.
type Config struct {
	Listen   ListenConfig   `yaml:"listen"`
	Log      LogConfig      `yaml:"log"`
	Dispatch DispatchConfig `yaml:"dispatch"`
	Auth     AuthConfig     `yaml:"auth"`
	Nodes    []NodeConfig   `yaml:"nodes"`
}

// ListenConfig holds the dispatcher + admin listen addresses.
type ListenConfig struct {
	Socks string `yaml:"socks"` // dispatcher SOCKS5
	HTTP  string `yaml:"http"`  // dispatcher HTTP CONNECT
	Admin string `yaml:"admin"` // admin / metrics / health
}

// LogConfig controls slog handler setup.
type LogConfig struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // tint | json | auto
}

// DispatchConfig controls how requests are dispatched across nodes.
type DispatchConfig struct {
	Strategy    DispatchStrategy  `yaml:"strategy"`
	StickyKey   string            `yaml:"sticky_key"`
	HealthCheck HealthCheckConfig `yaml:"health_check"`
}

// HealthCheckConfig controls the background worker health probe.
type HealthCheckConfig struct {
	Enabled        bool          `yaml:"enabled"`
	Interval       time.Duration `yaml:"interval"`
	SkipUnhealthy  bool          `yaml:"skip_unhealthy"`
	ProbeURL       string        `yaml:"probe_url"`
	ProbeTimeout   time.Duration `yaml:"probe_timeout"`
	EgressIPLookup bool          `yaml:"egress_ip_lookup"`
}

// AuthConfig describes how each worker authenticates to the Tailnet.
type AuthConfig struct {
	Method            AuthMethod `yaml:"method"`
	AuthKey           string     `yaml:"authkey"`
	OAuth2Credentials string     `yaml:"oauth2_credentials"`
	Ephemeral         *bool      `yaml:"ephemeral"`
	LoginServer       string     `yaml:"login_server"`
}

// NodeConfig is one entry in the pool — backs exactly one Worker.
type NodeConfig struct {
	Name      string   `yaml:"name"`
	ExitNode  string   `yaml:"exit_node"`
	StateDir  string   `yaml:"state_dir"`
	SocksAddr string   `yaml:"socks_addr"` // optional direct per-node SOCKS5
	AllowLAN  bool     `yaml:"allow_lan"`
	LocalDNS  bool     `yaml:"local_dns"`
	Tags      []string `yaml:"tags"`
}

// Default values mirrored from upstream tailsocks where relevant.
const (
	DefaultSocksAddr     = "127.0.0.1:5040"
	DefaultHTTPAddr      = "127.0.0.1:5050"
	DefaultAdminAddr     = "127.0.0.1:8080"
	DefaultStateDirBase  = "./flotilla-state"
	DefaultProbeURL      = "https://api.ipify.org"
	DefaultProbeTimeout  = 10 * time.Second
	DefaultProbeInterval = 30 * time.Second
)

// Load reads a YAML config file, applies defaults, and validates the result.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %q: %w", path, err)
	}
	return &c, nil
}

// ApplyDefaults fills in unset fields with sensible defaults.
func (c *Config) ApplyDefaults() {
	if c.Listen.Socks == "" {
		c.Listen.Socks = DefaultSocksAddr
	}
	if c.Listen.HTTP == "" {
		c.Listen.HTTP = DefaultHTTPAddr
	}
	if c.Listen.Admin == "" {
		c.Listen.Admin = DefaultAdminAddr
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "auto"
	}
	if c.Dispatch.Strategy == "" {
		c.Dispatch.Strategy = StrategyRoundRobin
	}
	if c.Dispatch.StickyKey == "" {
		c.Dispatch.StickyKey = "client_ip"
	}
	if c.Dispatch.HealthCheck.Interval == 0 {
		c.Dispatch.HealthCheck.Interval = DefaultProbeInterval
	}
	if c.Dispatch.HealthCheck.ProbeTimeout == 0 {
		c.Dispatch.HealthCheck.ProbeTimeout = DefaultProbeTimeout
	}
	if c.Dispatch.HealthCheck.ProbeURL == "" {
		c.Dispatch.HealthCheck.ProbeURL = DefaultProbeURL
	}
	if !c.Dispatch.HealthCheck.Enabled && c.Dispatch.HealthCheck.Interval > 0 {
		// keep zero-default explicit unless user opted in
	}
	if c.Auth.Method == "" {
		if strings.TrimSpace(c.Auth.OAuth2Credentials) != "" {
			c.Auth.Method = AuthMethodOAuth2
		} else {
			c.Auth.Method = AuthMethodAuthKey
		}
	}

	for i := range c.Nodes {
		n := &c.Nodes[i]
		if n.StateDir == "" {
			n.StateDir = filepath.Join(DefaultStateDirBase, n.Name)
		}
	}
}

// Validate enforces required fields and uniqueness invariants.
func (c *Config) Validate() error {
	if len(c.Nodes) == 0 {
		return errors.New("at least one node is required")
	}

	switch c.Dispatch.Strategy {
	case StrategyRoundRobin, StrategyRandom, StrategySticky, StrategyLeastActive, StrategyTagged:
	case "":
		return errors.New("dispatch.strategy is required")
	default:
		return fmt.Errorf("unknown dispatch strategy: %q", c.Dispatch.Strategy)
	}

	switch c.Auth.Method {
	case AuthMethodAuthKey, AuthMethodOAuth2:
	default:
		return fmt.Errorf("unknown auth.method: %q", c.Auth.Method)
	}

	seenNames := map[string]bool{}
	seenDirs := map[string]bool{}
	seenAddrs := map[string]bool{}
	for i, n := range c.Nodes {
		if n.Name == "" {
			return fmt.Errorf("node #%d: name is required", i)
		}
		if seenNames[n.Name] {
			return fmt.Errorf("duplicate node name: %q", n.Name)
		}
		seenNames[n.Name] = true

		if n.ExitNode == "" {
			return fmt.Errorf("node %q: exit_node is required", n.Name)
		}

		if n.StateDir == "" {
			return fmt.Errorf("node %q: state_dir is required", n.Name)
		}
		if seenDirs[n.StateDir] {
			return fmt.Errorf("duplicate state_dir: %q", n.StateDir)
		}
		seenDirs[n.StateDir] = true

		if n.SocksAddr != "" {
			if seenAddrs[n.SocksAddr] {
				return fmt.Errorf("duplicate per-node socks_addr: %q", n.SocksAddr)
			}
			seenAddrs[n.SocksAddr] = true
		}
	}

	return nil
}

// ToWorkerConfigs translates the Config's node list into WorkerConfig values.
// AuthKey is left empty here — the caller (run command) is responsible for
// minting per-worker auth keys via OAuth2 or env vars before constructing
// the workers.
func (c *Config) ToWorkerConfigs() []worker.Config {
	ephemeral := true // OAuth2 default
	if c.Auth.Method == AuthMethodAuthKey {
		ephemeral = false
	}
	if c.Auth.Ephemeral != nil {
		ephemeral = *c.Auth.Ephemeral
	}

	out := make([]worker.Config, 0, len(c.Nodes))
	for _, n := range c.Nodes {
		out = append(out, worker.Config{
			Name:        n.Name,
			ExitNode:    n.ExitNode,
			StateDir:    n.StateDir,
			Ephemeral:   ephemeral,
			LoginServer: c.Auth.LoginServer,
			AllowLAN:    n.AllowLAN,
			LocalDNS:    n.LocalDNS,
			Tags:        n.Tags,
		})
	}
	return out
}

// ExampleYAML is the bytes for `flotilla init` to scaffold a config.
func ExampleYAML() []byte {
	return []byte(`# flotilla.example.yaml — fleet of Tailscale exit-node SOCKS5/HTTP proxies.
# Run with:  flotilla run --config flotilla.yaml

listen:
  socks: 127.0.0.1:5040       # dispatcher SOCKS5
  http:  127.0.0.1:5050       # dispatcher HTTP CONNECT
  admin: 127.0.0.1:8080       # health, /ip, /metrics

log:
  level: info                  # debug | info | warn | error
  format: auto                 # auto | tint | json

dispatch:
  strategy: round_robin        # round_robin | random | sticky | least_active | tagged
  sticky_key: client_ip        # used by sticky strategy only
  health_check:
    enabled: true
    interval: 30s
    skip_unhealthy: true
    probe_url: https://api.ipify.org
    probe_timeout: 10s
    egress_ip_lookup: true

# Either:
#   method: authkey     + supply TS_AUTHKEY env var (or per-node creds via Tailscale)
# Or:
#   method: oauth2      + path to a JSON file with {client_id, client_secret, tag}
auth:
  method: oauth2
  oauth2_credentials: ~/.config/flotilla/oauth2.json
  ephemeral: true

nodes:
  - name: tr-1
    exit_node: tr-istanbul-1
    state_dir: ./state/tr-1
    socks_addr: 127.0.0.1:5041
    allow_lan: false
    tags: [region:tr]
  - name: tr-2
    exit_node: tr-istanbul-2
    state_dir: ./state/tr-2
    tags: [region:tr]
  - name: eu-1
    exit_node: eu-amsterdam-1
    state_dir: ./state/eu-1
    tags: [region:eu]
`)
}
