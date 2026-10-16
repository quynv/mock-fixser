// Command mockfix runs the mock FIX server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sync/errgroup"

	"github.com/quynv/mock-fixser/internal/config"
	"github.com/quynv/mock-fixser/internal/fixserver"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	configPath := flag.String("config", "configs/mock.yaml", "path to the YAML config file")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	noUI := flag.Bool("no-ui", false, "skip the HTTP server and the FIX gateway (for CI)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "invalid --log-level %q: %v\n", *logLevel, err)
		os.Exit(2)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("mockfix starting", "version", version, "config", *configPath, "no_ui", *noUI)
	if err := run(ctx, *configPath); err != nil {
		var verr *config.ValidationError
		if errors.As(err, &verr) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		slog.Error("mockfix failed", "error", err)
		os.Exit(1)
	}
	slog.Info("mockfix stopped")
}

// run follows the startup and shutdown order of section 12.1. Engines,
// the gateway and HTTP join in later phases.
func run(ctx context.Context, configPath string) error {
	store, err := config.Open(configPath, config.Options{})
	if err != nil {
		return err
	}
	cfg := store.Current()
	slog.Info("config loaded", "version", cfg.Version, "sessions", len(cfg.Server.FIX.Sessions), "symbols", len(cfg.Symbols))

	srv, err := fixserver.New(cfg, slog.Default())
	if err != nil {
		return err
	}
	store.Subscribe(srv.OnConfigChange)
	if err := srv.Start(); err != nil {
		return err
	}
	slog.Info("acceptors started", "client", cfg.Server.FIX.Listen, "monitor", cfg.Server.Monitor.Listen, "monitor_enabled", cfg.Server.Monitor.Enabled)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return store.Watch(gctx) })

	<-gctx.Done()
	srv.Stop()
	return g.Wait()
}
