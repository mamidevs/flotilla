// Package worker wraps a single tsnet.Server pinned to one Tailscale exit node.
// Multiple workers can be created inside the same process — each is a fully
// independent ephemeral Tailscale node with its own state dir.
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"

	"github.com/mamidevs/flotilla/internal/resolver"
)

// Config is the per-worker configuration.
type Config struct {
	Name        string   // Display name + Tailscale hostname (must be unique).
	ExitNode    string   // IP (100.x.y.z) or MagicDNS base name (required).
	StateDir    string   // tsnet state directory. Required and must be unique per worker.
	AuthKey     string   // Optional. If empty, tsnet falls back to interactive auth.
	Ephemeral   bool     // Ephemeral node — auto-cleaned on disconnect.
	LoginServer string   // Optional control plane URL (Headscale).
	AllowLAN    bool     // ExitNodeAllowLANAccess flag.
	LocalDNS    bool     // If true, do not use Tailscale's MagicDNS resolver.
	Tags        []string // Free-form tags for tagged dispatch strategy (e.g. "region:tr").
}

// Status is a snapshot of the worker state, safe for concurrent read.
type Status struct {
	Name         string    `json:"name"`
	ExitNode     string    `json:"exit_node"`
	Tags         []string  `json:"tags,omitempty"`
	Running      bool      `json:"running"`
	Healthy      bool      `json:"healthy"`
	Active       int64     `json:"active_connections"`
	DNSName      string    `json:"dns_name,omitempty"`
	TailscaleIPs []string  `json:"tailscale_ips,omitempty"`
	EgressIP     string    `json:"egress_ip,omitempty"`
	LastChecked  time.Time `json:"last_checked,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
}

// Worker is a tsnet-backed proxy worker pinned to a single Tailscale exit node.
type Worker struct {
	cfg    Config
	server *tsnet.Server
	lc     *local.Client
	rslv   *resolver.TailscaleResolver

	active atomic.Int64

	mu          sync.RWMutex
	running     bool
	healthy     bool
	egressIP    string
	lastChecked time.Time
	lastError   error
}

// New creates a worker but does NOT bring tsnet up yet.
// Call Start to actually connect to the Tailnet.
func New(cfg Config) (*Worker, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	w := &Worker{cfg: cfg}
	w.server = &tsnet.Server{
		AuthKey:   cfg.AuthKey,
		Dir:       cfg.StateDir,
		Hostname:  cfg.Name,
		Ephemeral: cfg.Ephemeral,
		Logf: func(format string, args ...any) {
			slog.Info(fmt.Sprintf(format, args...), slog.String("scope", "tsnet"), slog.String("worker", cfg.Name))
		},
		ControlURL: cfg.LoginServer,
	}
	return w, nil
}

func validateConfig(c Config) error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("worker name required")
	}
	if strings.TrimSpace(c.ExitNode) == "" {
		return fmt.Errorf("worker %q: exit_node required", c.Name)
	}
	if strings.TrimSpace(c.StateDir) == "" {
		return fmt.Errorf("worker %q: state_dir required", c.Name)
	}
	return nil
}

// Start brings tsnet up, applies exit-node prefs, and prepares the DNS resolver.
// It blocks until the node is registered and prefs are applied.
func (w *Worker) Start(ctx context.Context) error {
	slog.Info("Starting worker", "worker", w.cfg.Name, "exit_node", w.cfg.ExitNode)

	_, err := w.server.Up(ctx)
	if err != nil {
		w.markUnhealthy(err)
		return fmt.Errorf("tsnet up: %w", err)
	}

	lc, err := w.server.LocalClient()
	if err != nil {
		w.markUnhealthy(err)
		return fmt.Errorf("LocalClient: %w", err)
	}
	w.lc = lc

	st, err := lc.Status(ctx)
	if err != nil {
		w.markUnhealthy(err)
		return fmt.Errorf("tailscale not running/authorized: %w", err)
	}
	slog.Info("Tailscale up", "worker", w.cfg.Name, "dns_name", st.Self.DNSName, "tailscale_ips", st.Self.TailscaleIPs)

	if err := w.setExitNodePrefs(ctx, st); err != nil {
		w.markUnhealthy(err)
		return fmt.Errorf("set exit node prefs: %w", err)
	}
	slog.Info("Exit node configured", "worker", w.cfg.Name, "exit_node", w.cfg.ExitNode)

	if !w.cfg.LocalDNS && st.CurrentTailnet != nil && st.CurrentTailnet.MagicDNSEnabled {
		w.rslv = resolver.New(lc, st.CurrentTailnet.MagicDNSSuffix)
	}

	w.mu.Lock()
	w.running = true
	w.healthy = true
	w.lastError = nil
	w.mu.Unlock()

	return nil
}

func (w *Worker) setExitNodePrefs(ctx context.Context, st *ipnstate.Status) error {
	p, err := w.lc.GetPrefs(ctx)
	if err != nil {
		return fmt.Errorf("GetPrefs: %w", err)
	}

	np := p.Clone()
	np.WantRunning = true
	np.ExitNodeAllowLANAccess = w.cfg.AllowLAN
	np.ClearExitNode()

	if err := np.SetExitNodeIP(w.cfg.ExitNode, st); err != nil {
		return fmt.Errorf("SetExitNodeIP(%q): %w", w.cfg.ExitNode, err)
	}

	mp := &ipn.MaskedPrefs{
		Prefs:                     *np,
		WantRunningSet:            true,
		ExitNodeIPSet:             true,
		ExitNodeIDSet:             true,
		ExitNodeAllowLANAccessSet: true,
	}

	if _, err := w.lc.EditPrefs(ctx, mp); err != nil {
		return fmt.Errorf("EditPrefs: %w", err)
	}
	return nil
}

// Dial proxies a TCP/UDP connection via this worker's tsnet stack.
// The remote peer will see the worker's exit-node public IP.
func (w *Worker) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	w.active.Add(1)
	conn, err := w.server.Dial(ctx, network, addr)
	if err != nil {
		w.active.Add(-1)
		return nil, err
	}
	return &trackedConn{Conn: conn, w: w}, nil
}

// Resolver returns the worker's Tailscale MagicDNS resolver, if any.
// May be nil when --local-dns is set or MagicDNS is disabled in the Tailnet.
func (w *Worker) Resolver() *resolver.TailscaleResolver {
	return w.rslv
}

// Name returns the worker's display name.
func (w *Worker) Name() string { return w.cfg.Name }

// Tags returns the worker's tag list (read-only).
func (w *Worker) Tags() []string { return w.cfg.Tags }

// ExitNode returns the configured exit node selector.
func (w *Worker) ExitNode() string { return w.cfg.ExitNode }

// Healthy reports whether the worker is currently healthy.
func (w *Worker) Healthy() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.healthy
}

// ActiveConns returns the count of currently in-flight connections.
func (w *Worker) ActiveConns() int64 { return w.active.Load() }

// MarkHealthy / MarkUnhealthy are exported so the Pool's health-checker can
// update worker state without re-implementing the lock dance.
func (w *Worker) MarkHealthy(egressIP string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.healthy = true
	w.lastError = nil
	w.lastChecked = time.Now()
	if egressIP != "" {
		w.egressIP = egressIP
	}
}

func (w *Worker) MarkUnhealthy(err error) {
	w.markUnhealthy(err)
}

func (w *Worker) markUnhealthy(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.healthy = false
	w.lastError = err
	w.lastChecked = time.Now()
}

// Snapshot returns a copy of the worker's current public-facing state.
func (w *Worker) Snapshot() Status {
	w.mu.RLock()
	defer w.mu.RUnlock()

	s := Status{
		Name:        w.cfg.Name,
		ExitNode:    w.cfg.ExitNode,
		Tags:        w.cfg.Tags,
		Running:     w.running,
		Healthy:     w.healthy,
		Active:      w.active.Load(),
		EgressIP:    w.egressIP,
		LastChecked: w.lastChecked,
	}
	if w.lastError != nil {
		s.LastError = w.lastError.Error()
	}
	if w.lc != nil {
		if st, err := w.lc.Status(context.Background()); err == nil && st.Self != nil {
			s.DNSName = strings.TrimSuffix(st.Self.DNSName, ".")
			ips := make([]string, 0, len(st.Self.TailscaleIPs))
			for _, ip := range st.Self.TailscaleIPs {
				ips = append(ips, ip.String())
			}
			s.TailscaleIPs = ips
		}
	}
	return s
}

// LocalClient returns the underlying Tailscale local client.
// Callers should treat this as read-only — direct prefs mutation will
// fight with the worker's own state machine.
func (w *Worker) LocalClient() *local.Client { return w.lc }

// TailscaleIPs returns the worker's own 100.x addresses (best-effort).
func (w *Worker) TailscaleIPs(ctx context.Context) ([]netip.Addr, error) {
	if w.lc == nil {
		return nil, fmt.Errorf("worker %q not started", w.cfg.Name)
	}
	st, err := w.lc.Status(ctx)
	if err != nil {
		return nil, err
	}
	return st.Self.TailscaleIPs, nil
}

// Close brings the worker's tsnet server down. The worker becomes unusable.
func (w *Worker) Close() error {
	w.mu.Lock()
	w.running = false
	w.mu.Unlock()
	if w.server != nil {
		return w.server.Close()
	}
	return nil
}

// trackedConn decrements the active-connection counter exactly once on Close.
type trackedConn struct {
	net.Conn
	w      *Worker
	closed atomic.Bool
}

func (t *trackedConn) Close() error {
	if t.closed.CompareAndSwap(false, true) {
		t.w.active.Add(-1)
	}
	return t.Conn.Close()
}
