package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/pflag"

	"github.com/mamidevs/flotilla/internal/config"
)

func initMain(args []string) {
	fs := pflag.NewFlagSet("init", pflag.ContinueOnError)
	out := fs.StringP("output", "o", "flotilla.yaml", "Path to write the scaffolded config to.")
	force := fs.BoolP("force", "f", false, "Overwrite the output file if it exists.")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	if _, err := os.Stat(*out); err == nil && !*force {
		slog.Error("Refusing to overwrite existing file (use --force)", "path", *out)
		os.Exit(1)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Error("Stat output failed", "path", *out, "error", err)
		os.Exit(1)
	}

	if err := os.WriteFile(*out, config.ExampleYAML(), 0o600); err != nil {
		slog.Error("Write failed", "path", *out, "error", err)
		os.Exit(1)
	}
	fmt.Printf("Scaffolded %s — edit it, then run:  flotilla run --config %s\n", *out, *out)
}
