package dispatcher

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/armon/go-socks5"

	"github.com/mamidevs/flotilla/internal/metrics"
	"github.com/mamidevs/flotilla/internal/pool"
)

// SOCKSDispatcher is a front-end SOCKS5 server backed by a pool.
type SOCKSDispatcher struct {
	pool *pool.Pool
}

type socksHintKey struct{}

// NewSOCKS builds a SOCKS5 front-end. The returned server is not yet listening.
//
// Hint passthrough: the SOCKS5 RFC1929 username field is used as a Flotilla
// hint string. Examples (curl):
//
//	curl --socks5 user:pass@127.0.0.1:5040 …
//	  where user = "node=tr-1"            → pin tr-1
//	  where user = "tags=region:tr"       → tag filter
//	  where user = "tr-1"                 → if it's a known worker, treat as pin
//
// Anonymous SOCKS5 still works — the pool's configured strategy is used.
func NewSOCKS(p *pool.Pool, strategyName string) (*SOCKSDispatcher, *socks5.Server, error) {
	d := &SOCKSDispatcher{pool: p}

	cfg := &socks5.Config{
		AuthMethods: []socks5.Authenticator{
			socks5.NoAuthAuthenticator{},
			socks5.UserPassAuthenticator{Credentials: acceptAllCreds{}},
		},
		// Rules.Allow runs after auth and before Dial. We use it to stash the
		// authenticated username + client addr into the dial context.
		Rules: socksRules{p: p},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			hint, _ := ctx.Value(socksHintKey{}).(pool.Hint)
			start := metrics.Now()
			w, err := p.Pick(ctx, hint)
			if err != nil {
				metrics.ObservePick(strategyName, "", err)
				metrics.ObserveRequest("", "socks5", "no_worker", start)
				return nil, fmt.Errorf("pool pick: %w", err)
			}
			metrics.ObservePick(strategyName, w.Name(), nil)

			conn, err := w.Dial(ctx, network, addr)
			if err != nil {
				metrics.ObserveRequest(w.Name(), "socks5", "dial_error", start)
				return nil, fmt.Errorf("worker %q dial %s: %w", w.Name(), addr, err)
			}
			metrics.ObserveRequest(w.Name(), "socks5", "ok", start)
			return conn, nil
		},
		Logger: slog.NewLogLogger(
			slog.Default().With(slog.String("scope", "socks")).Handler(),
			slog.LevelInfo,
		),
	}

	srv, err := socks5.New(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("build socks5 server: %w", err)
	}
	return d, srv, nil
}

// socksRules pulls the authenticated SOCKS5 username + client addr out of
// the request and propagates them as a pool.Hint via context.
type socksRules struct{ p *pool.Pool }

func (s socksRules) Allow(ctx context.Context, req *socks5.Request) (context.Context, bool) {
	h := pool.Hint{}
	if req.AuthContext != nil && req.AuthContext.Payload != nil {
		if u := strings.TrimSpace(req.AuthContext.Payload["Username"]); u != "" {
			h = HintFromSocksUser(u, func(name string) bool { return s.p.ByName(name) != nil })
		}
	}
	if h.ClientID == "" && req.RemoteAddr != nil && req.RemoteAddr.IP != nil {
		h.ClientID = req.RemoteAddr.IP.String()
	}
	ctx = context.WithValue(ctx, socksHintKey{}, h)
	return ctx, true
}

// acceptAllCreds accepts every (user, pass) — the username is treated as a
// hint string, not a secret. Operators wanting real auth should put a reverse
// proxy or an IP allow-list in front, or run separate Flotilla processes.
type acceptAllCreds struct{}

func (acceptAllCreds) Valid(_, _ string) bool { return true }
