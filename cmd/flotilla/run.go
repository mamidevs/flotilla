package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	kitslog "github.com/italypaleale/go-kit/slog"
	"github.com/spf13/pflag"

	"github.com/mamidevs/flotilla/internal/admin"
	"github.com/mamidevs/flotilla/internal/auth"
	"github.com/mamidevs/flotilla/internal/config"
	"github.com/mamidevs/flotilla/internal/dispatcher"
	"github.com/mamidevs/flotilla/internal/pool"
	"github.com/mamidevs/flotilla/internal/worker"
)

type runFlags struct {
	configPath string

	// Single-node compatibility flags (mirror upstream tailsocks).
	exitNode    string
	socksAddr   string
	hostname    string
	stateDir    string
	authKey     string
	useOAuth2   bool
	allowLAN    bool
	loginServer string
	localDNS    bool

	ephemeral    bool
	ephemeralSet bool

	logLevel  string
	logFormat string
}

func parseRunFlags(args []string) *runFlags {
	fs := pflag.NewFlagSet("run", pflag.ContinueOnError)
	f := &runFlags{}

	fs.StringVarP(&f.configPath, "config", "c", "", "Path to flotilla.yaml. Required for pool mode.")

	fs.StringVarP(&f.exitNode, "exit-node", "x", "", "Single-node mode: exit node IP or MagicDNS name.")
	fs.StringVarP(&f.socksAddr, "socks-addr", "a", config.DefaultSocksAddr, "Single-node mode: SOCKS5 listen address.")
	fs.StringVarP(&f.hostname, "hostname", "n", "flotilla", "Single-node mode: Tailscale hostname.")
	fs.StringVarP(&f.stateDir, "state-dir", "s", "./tsnet-state", "Single-node mode: tsnet state dir.")
	fs.StringVarP(&f.authKey, "authkey", "k", "", "Optional Tailscale auth key (or TS_AUTHKEY).")
	fs.BoolVarP(&f.useOAuth2, "oauth2", "o", false, "Authenticate via OAuth2 client credentials.")
	fs.BoolVarP(&f.allowLAN, "exit-node-allow-lan-access", "l", false, "Allow LAN access while using exit node.")
	fs.StringVar(&f.loginServer, "login-server", "", "Optional control server URL (Headscale).")
	fs.BoolVar(&f.localDNS, "local-dns", false, "Skip Tailscale MagicDNS resolver.")

	fs.BoolVarP(&f.ephemeral, "ephemeral", "e", false, "Make tsnet nodes ephemeral.")

	fs.StringVar(&f.logLevel, "log-level", "", "Log level (debug|info|warn|error). Overrides config.")
	fs.StringVar(&f.logFormat, "log-format", "", "Log format (auto|tint|json). Overrides config.")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if fs.Changed("ephemeral") {
		f.ephemeralSet = true
	}
	return f
}

func runMain(args []string) {
	f := parseRunFlags(args)
	setLogger(f.logFormat)

	if f.configPath == "" && f.exitNode == "" {
		fmt.Fprintln(os.Stderr, "flotilla run: either --config FILE or --exit-node X is required")
		os.Exit(2)
	}

	cfg, err := buildConfig(f)
	if err != nil {
		kitslog.FatalError(slog.Default(), "failed to build config", err)
	}

	if f.logLevel != "" {
		cfg.Log.Level = f.logLevel
	}
	if f.logFormat != "" {
		cfg.Log.Format = f.logFormat
	}
	setLogger(cfg.Log.Format)

	ctx := signalCtx(context.Background())

	workers, err := startWorkers(ctx, cfg)
	if err != nil {
		kitslog.FatalError(slog.Default(), "worker startup failed", err)
	}
	defer func() {
		for _, w := range workers {
			_ = w.Close()
		}
	}()

	strategy, err := pool.NewStrategy(cfg.Dispatch.Strategy)
	if err != nil {
		kitslog.FatalError(slog.Default(), "build strategy", err)
	}
	p := pool.New(workers, strategy, cfg.Dispatch.HealthCheck)
	p.StartHealthChecks(ctx)
	defer p.Stop()

	startDispatchersAndAdmin(ctx, cfg, p)
}

func buildConfig(f *runFlags) (*config.Config, error) {
	if f.configPath != "" {
		return config.Load(f.configPath)
	}

	// Single-node mode — synthesize a Config from CLI flags.
	cfg := &config.Config{
		Listen: config.ListenConfig{Socks: f.socksAddr},
		Auth: config.AuthConfig{
			LoginServer: f.loginServer,
		},
		Nodes: []config.NodeConfig{{
			Name:      f.hostname,
			ExitNode:  f.exitNode,
			StateDir:  f.stateDir,
			SocksAddr: f.socksAddr,
			AllowLAN:  f.allowLAN,
			LocalDNS:  f.localDNS,
		}},
	}
	if f.useOAuth2 {
		cfg.Auth.Method = config.AuthMethodOAuth2
	} else {
		cfg.Auth.Method = config.AuthMethodAuthKey
		cfg.Auth.AuthKey = f.authKey
	}
	if f.ephemeralSet {
		v := f.ephemeral
		cfg.Auth.Ephemeral = &v
	}
	cfg.Listen.HTTP = ""  // disable in single-node mode by default
	cfg.Listen.Admin = "" // disable admin in single-node mode by default
	cfg.ApplyDefaults()
	// In single-node mode the user expects only SOCKS5 on the configured port,
	// no admin server, no HTTP. We restore those defaults to "" after defaults.
	cfg.Listen.HTTP = ""
	cfg.Listen.Admin = ""
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func startWorkers(ctx context.Context, cfg *config.Config) ([]*worker.Worker, error) {
	workerCfgs := cfg.ToWorkerConfigs()

	authKeyMinter, err := authKeyMinterFor(cfg)
	if err != nil {
		return nil, err
	}

	workers := make([]*worker.Worker, 0, len(workerCfgs))
	for i := range workerCfgs {
		wc := workerCfgs[i]

		if err := os.MkdirAll(wc.StateDir, 0o700); err != nil {
			return nil, fmt.Errorf("worker %q: create state dir: %w", wc.Name, err)
		}

		key, err := authKeyMinter(ctx, wc.Ephemeral)
		if err != nil {
			return nil, fmt.Errorf("worker %q: auth: %w", wc.Name, err)
		}
		wc.AuthKey = key

		w, err := worker.New(wc)
		if err != nil {
			return nil, err
		}
		if err := w.Start(ctx); err != nil {
			return nil, fmt.Errorf("worker %q: start: %w", wc.Name, err)
		}
		workers = append(workers, w)
	}
	return workers, nil
}

// authKeyMinterFor returns a closure that produces a per-worker auth key.
// Static-key mode reuses the same key for every worker. OAuth2 mode mints
// a fresh ephemeral key per call (which is the only way to register multiple
// independent tsnet nodes against a single OAuth2 client).
func authKeyMinterFor(cfg *config.Config) (func(ctx context.Context, ephemeral bool) (string, error), error) {
	switch cfg.Auth.Method {
	case config.AuthMethodAuthKey:
		key := strings.TrimSpace(cfg.Auth.AuthKey)
		if key == "" {
			key = auth.AuthKeyFromEnv()
		}
		return func(_ context.Context, _ bool) (string, error) { return key, nil }, nil

	case config.AuthMethodOAuth2:
		// Either TS_OAUTH_ACCESS_TOKEN+TS_OAUTH_TAG env vars OR a creds file.
		if tok, tag := auth.OAuthAccessTokenFromEnv(); tok != "" {
			creds := &auth.OAuth2Credentials{Tag: tag}
			return func(ctx context.Context, ephemeral bool) (string, error) {
				return creds.CreateAuthKey(ctx, tok, ephemeral)
			}, nil
		}
		path := expandHome(cfg.Auth.OAuth2Credentials)
		if path == "" {
			defaultPath, err := auth.DefaultCredentialsPath()
			if err != nil {
				return nil, err
			}
			path = defaultPath
		}
		creds, err := auth.LoadOAuth2Credentials(path)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, ephemeral bool) (string, error) {
			return creds.GetAuthToken(ctx, ephemeral)
		}, nil

	default:
		return nil, fmt.Errorf("unknown auth method: %q", cfg.Auth.Method)
	}
}

func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}

func startDispatchersAndAdmin(ctx context.Context, cfg *config.Config, p *pool.Pool) {
	strategyName := string(cfg.Dispatch.Strategy)

	// 1) Front-end SOCKS5
	if cfg.Listen.Socks != "" {
		_, socksSrv, err := dispatcher.NewSOCKS(p, strategyName)
		if err != nil {
			kitslog.FatalError(slog.Default(), "build SOCKS5 dispatcher", err)
		}
		ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Listen.Socks)
		if err != nil {
			kitslog.FatalError(slog.Default(), "listen SOCKS5", err)
		}
		slog.Info("SOCKS5 dispatcher listening", "addr", "socks5://"+cfg.Listen.Socks, "strategy", strategyName)
		go func() {
			if err := socksSrv.Serve(ln); err != nil && ctx.Err() == nil {
				slog.Warn("SOCKS5 dispatcher stopped", "error", err)
			}
		}()
		defer func() { _ = ln.Close() }()
	}

	// 2) Front-end HTTP CONNECT
	if cfg.Listen.HTTP != "" {
		_, h := dispatcher.NewHTTP(p, strategyName)
		go func() {
			srv := &httpServerCompat{Addr: cfg.Listen.HTTP, Handler: h}
			slog.Info("HTTP dispatcher listening", "addr", "http://"+cfg.Listen.HTTP)
			if err := srv.ListenAndServeWithCtx(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("HTTP dispatcher stopped", "error", err)
			}
		}()
	}

	// 3) Per-node direct SOCKS5 (optional, helps with debugging / pinning).
	for _, n := range cfg.Nodes {
		if n.SocksAddr == "" {
			continue
		}
		w := p.ByName(n.Name)
		if w == nil {
			continue
		}
		startDirectSocks(ctx, w, n.SocksAddr, strategyName)
	}

	// 4) Admin server.
	if cfg.Listen.Admin != "" {
		go func() {
			a := admin.New(p, cfg.Dispatch.HealthCheck.ProbeURL)
			slog.Info("Admin server listening", "addr", "http://"+cfg.Listen.Admin)
			if err := a.Serve(ctx, cfg.Listen.Admin); err != nil && ctx.Err() == nil {
				slog.Warn("Admin server stopped", "error", err)
			}
		}()
	}

	<-ctx.Done()
	slog.Info("Shutting down…")
	// Give in-flight requests a brief grace period.
	time.Sleep(200 * time.Millisecond)
}

// startDirectSocks attaches a SOCKS5 listener pinned to a single worker —
// no pool, no strategy, just `worker.Dial`.
func startDirectSocks(ctx context.Context, w *worker.Worker, addr, strategyName string) {
	pinned := pool.New([]*worker.Worker{w}, must(pool.NewStrategy(config.StrategyRoundRobin)), config.HealthCheckConfig{})
	_, srv, err := dispatcher.NewSOCKS(pinned, "pin:"+strategyName)
	if err != nil {
		slog.Warn("Direct SOCKS5 setup failed", "worker", w.Name(), "error", err)
		return
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		slog.Warn("Direct SOCKS5 listen failed", "worker", w.Name(), "addr", addr, "error", err)
		return
	}
	slog.Info("Direct SOCKS5 listening", "worker", w.Name(), "addr", "socks5://"+addr)
	go func() {
		if err := srv.Serve(ln); err != nil && ctx.Err() == nil {
			slog.Warn("Direct SOCKS5 stopped", "worker", w.Name(), "error", err)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
