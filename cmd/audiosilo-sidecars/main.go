// Command audiosilo-sidecars runs the sidecars contributor-tool daemon: a local
// HTTP server with an embedded web UI for turning an audiobook folder into
// character/recap sidecars for meta.audiosilo.app. This is the M0 skeleton
// (auth, secrets, event stream, UI shell); the extraction pipeline lands in
// later milestones.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/kodestar/audiosilo-sidecars/internal/config"
	"github.com/kodestar/audiosilo-sidecars/internal/server"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// Dispatch on the first argument when it is a subcommand; otherwise default
	// to serve, so `audiosilo-sidecars --listen ...` works with no subcommand.
	cmd := "serve"
	rest := args
	if len(args) > 0 && !isFlag(args[0]) {
		cmd = args[0]
		rest = args[1:]
	}

	switch cmd {
	case "serve":
		return runServe(rest)
	case "version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func isFlag(s string) bool { return len(s) > 0 && s[0] == '-' }

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	data := fs.String("data", defaultDataDir(), "data directory (config, auth, secrets)")
	// Default empty = "use the config file's listen value" - a non-empty flag
	// default would silently override config.yaml on every run (server.Run
	// treats any non-empty Listen as an explicit override).
	listen := fs.String("listen", "", "bind address host:port (default "+config.DefaultListen+", overrides config.yaml)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := server.Options{DataDir: *data, Listen: *listen, Version: version, Out: os.Stderr}
	for {
		err := server.Run(ctx, opts)
		if errors.Is(err, server.ErrRestartRequested) && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "[info] restarting daemon to apply configuration")
			continue
		}
		return err
	}
}

// defaultDataDir returns ~/.audiosilo-sidecars, falling back to ./.audiosilo-sidecars
// when the home directory cannot be resolved.
func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".audiosilo-sidecars"
	}
	return filepath.Join(home, ".audiosilo-sidecars")
}

func usage(w *os.File) {
	fmt.Fprint(w, `audiosilo-sidecars - contributor tool for meta.audiosilo.app sidecars

Usage:
  audiosilo-sidecars [serve] [--data DIR] [--listen HOST:PORT]
  audiosilo-sidecars version

Commands:
  serve      Run the daemon and embedded web UI (default).
  version    Print the build version.

Serve flags:
  --data     Data directory for config, auth, and secrets
             (default ~/.audiosilo-sidecars).
  --listen   Bind address (default 127.0.0.1:8090). Loopback by default;
             set 0.0.0.0:PORT only behind a trusted network or proxy.
`)
}
