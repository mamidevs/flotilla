package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mamidevs/flotilla/internal/metrics"
	"github.com/mamidevs/flotilla/internal/pool"
)

// HTTPDispatcher is a minimal HTTP CONNECT + plain HTTP forward proxy.
type HTTPDispatcher struct {
	pool         *pool.Pool
	strategyName string
}

// NewHTTP builds the HTTP CONNECT dispatcher. The returned http.Handler is
// not yet listening — caller wraps it in http.Server.
func NewHTTP(p *pool.Pool, strategyName string) (*HTTPDispatcher, http.Handler) {
	d := &HTTPDispatcher{pool: p, strategyName: strategyName}
	return d, d
}

func (d *HTTPDispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		d.handleConnect(w, r)
		return
	}
	d.handleForward(w, r)
}

func (d *HTTPDispatcher) handleConnect(w http.ResponseWriter, r *http.Request) {
	start := metrics.Now()
	hint := HintFromHTTP(r)

	target := r.URL.Host
	if target == "" {
		target = r.Host
	}
	if target == "" {
		http.Error(w, "missing target host", http.StatusBadRequest)
		return
	}

	worker, err := d.pool.Pick(r.Context(), hint)
	if err != nil {
		metrics.ObservePick(d.strategyName, "", err)
		metrics.ObserveRequest("", "http_connect", "no_worker", start)
		http.Error(w, fmt.Sprintf("pool: %v", err), http.StatusBadGateway)
		return
	}
	metrics.ObservePick(d.strategyName, worker.Name(), nil)

	dialCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	upstream, err := worker.Dial(dialCtx, "tcp", target)
	if err != nil {
		metrics.ObserveRequest(worker.Name(), "http_connect", "dial_error", start)
		http.Error(w, fmt.Sprintf("dial %s via %s: %v", target, worker.Name(), err), http.StatusBadGateway)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, bufrw, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		http.Error(w, fmt.Sprintf("hijack: %v", err), http.StatusInternalServerError)
		return
	}

	// 200 OK to the client, then bidirectional copy.
	_, err = fmt.Fprintf(clientConn, "HTTP/1.1 200 OK\r\nX-Flotilla-Node: %s\r\n\r\n", worker.Name())
	if err != nil {
		_ = clientConn.Close()
		_ = upstream.Close()
		metrics.ObserveRequest(worker.Name(), "http_connect", "write_error", start)
		return
	}
	if err := bufrw.Flush(); err != nil {
		_ = clientConn.Close()
		_ = upstream.Close()
		return
	}

	metrics.ObserveRequest(worker.Name(), "http_connect", "ok", start)
	go pipe(upstream, clientConn)
	pipe(clientConn, upstream)
}

func (d *HTTPDispatcher) handleForward(w http.ResponseWriter, r *http.Request) {
	if !r.URL.IsAbs() {
		http.Error(w, "this endpoint speaks HTTP proxy — use an absolute URL or CONNECT", http.StatusBadRequest)
		return
	}
	start := metrics.Now()
	hint := HintFromHTTP(r)

	worker, err := d.pool.Pick(r.Context(), hint)
	if err != nil {
		metrics.ObservePick(d.strategyName, "", err)
		metrics.ObserveRequest("", "http", "no_worker", start)
		http.Error(w, fmt.Sprintf("pool: %v", err), http.StatusBadGateway)
		return
	}
	metrics.ObservePick(d.strategyName, worker.Name(), nil)

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return worker.Dial(ctx, network, addr)
		},
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	defer transport.CloseIdleConnections()

	// Reuse the request, but reset the URL so the upstream host is target.
	upstreamReq := r.Clone(r.Context())
	upstreamReq.RequestURI = ""

	resp, err := transport.RoundTrip(upstreamReq)
	if err != nil {
		metrics.ObserveRequest(worker.Name(), "http", "upstream_error", start)
		http.Error(w, fmt.Sprintf("upstream %s: %v", worker.Name(), err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close() //nolint:errcheck

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Flotilla-Node", worker.Name())
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	metrics.ObserveRequest(worker.Name(), "http", "ok", start)
}

func pipe(dst, src net.Conn) {
	defer func() { _ = dst.Close() }()
	defer func() { _ = src.Close() }()
	_, err := io.Copy(dst, src)
	if err != nil && !errors.Is(err, io.EOF) && !isClosedConnError(err) {
		slog.Debug("pipe ended", "error", err)
	}
}

func isClosedConnError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "use of closed network connection") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "connection reset")
}
