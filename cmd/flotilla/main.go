package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/italypaleale/go-kit/signals"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"

	"github.com/mamidevs/flotilla/internal/buildinfo"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]

	switch sub {
	case "run":
		runMain(args)
	case "version", "--version", "-v":
		fmt.Printf("%s %s - build: %s\n", buildinfo.AppName, buildinfo.AppVersion, buildinfo.BuildDescription)
	case "init":
		initMain(args)
	case "ip":
		ipMain(args)
	case "nodes":
		nodesMain(args)
	case "doctor":
		doctorMain(args)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %q\n\n", sub)
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Println(`flotilla — a fleet of Tailscale exit-node SOCKS5/HTTP proxies.

Usage:
  flotilla <command> [flags]

Commands:
  run         Start the proxy daemon (use --config or --exit-node).
  init        Scaffold an example flotilla.yaml.
  ip          Print egress IPs of running workers.
  nodes       List worker status as a table.
  doctor      Validate config and tailnet reachability.
  version     Print the build version.

Examples:
  flotilla run --config flotilla.yaml
  flotilla run --exit-node home-server                       # single-node, tailsocks-compat
  flotilla init -o flotilla.yaml
  flotilla ip
  flotilla nodes
  flotilla doctor --config flotilla.yaml

Environment variables honored:
  TS_AUTHKEY                  Static Tailscale auth key.
  TS_OAUTH_ACCESS_TOKEN       Pre-issued OAuth2 token (CI / federated flows).
  TS_OAUTH_TAG                Tag for OAuth2-minted keys.
  FLOTILLA_ADMIN_URL          Override target for ` + "`ip`/`nodes`" + ` subcommands.`)
}

// setLogger mirrors upstream tailsocks' setup: pretty tint for TTY, JSON
// for non-TTY (containers, systemd journals). The "format" config knob can
// override.
func setLogger(format string) {
	useTint := false
	switch strings.ToLower(format) {
	case "tint":
		useTint = true
	case "json":
		useTint = false
	case "auto", "":
		useTint = isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd())
	}
	var handler slog.Handler
	if useTint {
		handler = tint.NewHandler(os.Stderr, nil)
	} else {
		handler = slog.NewJSONHandler(os.Stderr, nil)
	}
	slog.SetDefault(slog.New(handler))
}

// signalCtx returns a context that's canceled on SIGINT/SIGTERM.
// (Re-exported for visibility — keeps the import path explicit.)
var signalCtx = signals.SignalContext
