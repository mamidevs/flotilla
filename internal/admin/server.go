// Package admin exposes Flotilla's HTTP admin API: health, per-node status,
// egress IP probing, and Prometheus metrics scrape endpoint.
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/mamidevs/flotilla/internal/buildinfo"
	"github.com/mamidevs/flotilla/internal/metrics"
	"github.com/mamidevs/flotilla/internal/pool"
	"github.com/mamidevs/flotilla/internal/worker"
)

// Server hosts the admin HTTP endpoints.
type Server struct {
	pool     *pool.Pool
	probeURL string
	cacheTTL time.Duration

	mu        sync.Mutex
	ipCache   map[string]ipCacheEntry
	httpSrv   *http.Server
	scrapeReg prometheus.Gatherer
}

type ipCacheEntry struct {
	IP    string
	Until time.Time
}

// New constructs the admin server. probeURL is used by `/ip` lookups
// when no cached value is available.
func New(p *pool.Pool, probeURL string) *Server {
	if probeURL == "" {
		probeURL = "https://api.ipify.org"
	}
	return &Server{
		pool:      p,
		probeURL:  probeURL,
		cacheTTL:  5 * time.Minute,
		ipCache:   make(map[string]ipCacheEntry),
		scrapeReg: prometheus.DefaultGatherer,
	}
}

// Handler returns the admin HTTP handler (mux).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/health/nodes", s.handleHealthNodes)
	mux.HandleFunc("/ip", s.handleIP)
	mux.Handle("/metrics", promhttp.HandlerFor(s.scrapeReg, promhttp.HandlerOpts{}))
	return mux
}

// Serve starts the admin HTTP server. Blocks until ctx is done or the
// underlying http.Server fails.
func (s *Server) Serve(ctx context.Context, addr string) error {
	s.httpSrv = &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.httpSrv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.httpSrv.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) handleRoot(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "flotilla %s\n", buildinfo.AppVersion)
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "endpoints:")
	fmt.Fprintln(w, "  GET  /health")
	fmt.Fprintln(w, "  GET  /health/nodes")
	fmt.Fprintln(w, "  GET  /ip          # all nodes")
	fmt.Fprintln(w, "  GET  /ip?node=X   # one node")
	fmt.Fprintln(w, "  GET  /metrics     # Prometheus scrape")
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	healthy := 0
	for _, ww := range s.pool.Workers() {
		if ww.Healthy() {
			healthy++
		}
	}
	if healthy == 0 {
		http.Error(w, "no healthy workers", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":          "ok",
		"healthy_workers": healthy,
		"total_workers":   len(s.pool.Workers()),
	})
}

func (s *Server) handleHealthNodes(w http.ResponseWriter, _ *http.Request) {
	out := make([]worker.Status, 0, len(s.pool.Workers()))
	for _, ww := range s.pool.Workers() {
		snap := ww.Snapshot()
		// Refresh metrics gauges alongside the JSON view.
		metrics.SetActiveConnections(snap.Name, snap.Active)
		metrics.SetNodeHealth(snap.Name, snap.Healthy)
		metrics.SetEgressIPInfo(snap.Name, snap.EgressIP)
		out = append(out, snap)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"nodes": out,
	})
}

func (s *Server) handleIP(w http.ResponseWriter, r *http.Request) {
	nodeFilter := strings.TrimSpace(r.URL.Query().Get("node"))
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result := map[string]string{}
	for _, ww := range s.pool.Workers() {
		if nodeFilter != "" && ww.Name() != nodeFilter {
			continue
		}
		ip, err := s.egressIP(ctx, ww)
		if err != nil {
			result[ww.Name()] = "error: " + err.Error()
			continue
		}
		result[ww.Name()] = ip
		metrics.SetEgressIPInfo(ww.Name(), ip)
	}
	if nodeFilter != "" && len(result) == 0 {
		http.Error(w, "unknown node", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func (s *Server) egressIP(ctx context.Context, ww *worker.Worker) (string, error) {
	s.mu.Lock()
	if entry, ok := s.ipCache[ww.Name()]; ok && time.Now().Before(entry.Until) {
		s.mu.Unlock()
		return entry.IP, nil
	}
	s.mu.Unlock()

	transport := &http.Transport{
		DialContext: func(c context.Context, network, addr string) (net.Conn, error) {
			return ww.Dial(c, network, addr)
		},
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.probeURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "flotilla-admin/1")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("probe HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(body))

	s.mu.Lock()
	s.ipCache[ww.Name()] = ipCacheEntry{IP: ip, Until: time.Now().Add(s.cacheTTL)}
	s.mu.Unlock()

	return ip, nil
}
