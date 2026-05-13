// Package pool keeps a set of workers and hands them out to the dispatcher
// according to a configurable strategy. It also runs a background health
// probe that flips per-worker health state.
package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mamidevs/flotilla/internal/config"
	"github.com/mamidevs/flotilla/internal/worker"
)

// ErrNoHealthyWorker is returned when the dispatcher asks for a worker
// but the pool has nothing usable.
var ErrNoHealthyWorker = errors.New("no healthy worker available")

// Hint is metadata the dispatcher attaches to each request so the strategy
// can make an informed choice (sticky-by-client, tag-filtered, pinned).
type Hint struct {
	ClientID string   // Stable per-client value: src IP, header, SOCKS5 user.
	Tags     []string // Tag filter, e.g. ["region:tr"]. Empty = no filter.
	PinName  string   // If set, pool MUST return this worker or fail.
}

// Pool owns the workers and routes Pick calls through a Strategy.
type Pool struct {
	workers       []*worker.Worker
	byName        map[string]*worker.Worker
	strategy      Strategy
	skipUnhealthy bool

	hc       config.HealthCheckConfig
	stopOnce sync.Once
	stop     chan struct{}
}

// New constructs a Pool. Workers must already be Start()ed by the caller
// (so any auth/tsnet failures surface during `flotilla run` boot, not lazily).
func New(workers []*worker.Worker, strategy Strategy, hc config.HealthCheckConfig) *Pool {
	byName := make(map[string]*worker.Worker, len(workers))
	for _, w := range workers {
		byName[w.Name()] = w
	}
	return &Pool{
		workers:       workers,
		byName:        byName,
		strategy:      strategy,
		skipUnhealthy: hc.SkipUnhealthy,
		hc:            hc,
		stop:          make(chan struct{}),
	}
}

// Workers returns the underlying slice. Callers must treat it as read-only.
func (p *Pool) Workers() []*worker.Worker { return p.workers }

// ByName returns a worker by display name, or nil if unknown.
func (p *Pool) ByName(name string) *worker.Worker { return p.byName[name] }

// Pick selects a worker according to the configured strategy.
func (p *Pool) Pick(ctx context.Context, h Hint) (*worker.Worker, error) {
	if h.PinName != "" {
		w := p.byName[h.PinName]
		if w == nil {
			return nil, fmt.Errorf("pinned worker %q not found", h.PinName)
		}
		if p.skipUnhealthy && !w.Healthy() {
			return nil, fmt.Errorf("pinned worker %q is unhealthy", h.PinName)
		}
		return w, nil
	}

	candidates := p.candidates(h.Tags)
	if len(candidates) == 0 {
		return nil, ErrNoHealthyWorker
	}
	return p.strategy.Choose(ctx, candidates, h)
}

func (p *Pool) candidates(tags []string) []*worker.Worker {
	out := make([]*worker.Worker, 0, len(p.workers))
	for _, w := range p.workers {
		if p.skipUnhealthy && !w.Healthy() {
			continue
		}
		if !matchesTags(w.Tags(), tags) {
			continue
		}
		out = append(out, w)
	}
	return out
}

// matchesTags returns true if `have` contains every `want` tag.
// Empty `want` matches any worker.
func matchesTags(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	set := make(map[string]bool, len(have))
	for _, t := range have {
		set[strings.ToLower(t)] = true
	}
	for _, t := range want {
		if !set[strings.ToLower(t)] {
			return false
		}
	}
	return true
}

// StartHealthChecks spins off a background goroutine that probes each worker
// at the configured interval, flipping its healthy flag accordingly.
func (p *Pool) StartHealthChecks(ctx context.Context) {
	if !p.hc.Enabled || p.hc.Interval <= 0 {
		slog.Info("Health checks disabled")
		return
	}
	go p.healthLoop(ctx)
}

func (p *Pool) healthLoop(ctx context.Context) {
	// Run an immediate probe so /health/nodes is meaningful right after boot.
	p.probeAll(ctx)

	t := time.NewTicker(p.hc.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stop:
			return
		case <-t.C:
			p.probeAll(ctx)
		}
	}
}

func (p *Pool) probeAll(parent context.Context) {
	var wg sync.WaitGroup
	for _, w := range p.workers {
		wg.Add(1)
		go func(w *worker.Worker) {
			defer wg.Done()
			p.probeOne(parent, w)
		}(w)
	}
	wg.Wait()
}

func (p *Pool) probeOne(parent context.Context, w *worker.Worker) {
	ctx, cancel := context.WithTimeout(parent, p.hc.ProbeTimeout)
	defer cancel()

	transport := &http.Transport{
		DialContext:           w.Dial,
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: p.hc.ProbeTimeout,
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: p.hc.ProbeTimeout}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.hc.ProbeURL, nil)
	if err != nil {
		w.MarkUnhealthy(fmt.Errorf("build probe request: %w", err))
		return
	}
	req.Header.Set("User-Agent", "flotilla-healthcheck/1")

	resp, err := client.Do(req)
	if err != nil {
		w.MarkUnhealthy(fmt.Errorf("probe: %w", err))
		return
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode/100 != 2 {
		w.MarkUnhealthy(fmt.Errorf("probe HTTP %d", resp.StatusCode))
		return
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	egressIP := strings.TrimSpace(string(body))
	if !p.hc.EgressIPLookup {
		egressIP = ""
	}
	w.MarkHealthy(egressIP)
}

// Stop signals the health-check goroutine to exit. Safe to call multiple times.
func (p *Pool) Stop() {
	p.stopOnce.Do(func() { close(p.stop) })
}
