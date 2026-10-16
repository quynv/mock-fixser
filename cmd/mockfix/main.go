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

	"github.com/quynv/mock-fixser/internal/app"
	"github.com/quynv/mock-fixser/internal/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	configPath := flag.String("config", "configs/mock.yaml", "path to the YAML config file")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	noUI := flag.Bool("no-ui", false, "skip the HTTP server and the FIX gateway (for CI)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	pprof := flag.Bool("pprof", false, "serve /debug/pprof on the HTTP server")
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
	if err := run(ctx, *configPath, app.Options{NoUI: *noUI, Pprof: *pprof}); err != nil {
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

func run(ctx context.Context, configPath string, opts app.Options) error {
	a, err := app.New(configPath, slog.Default(), opts)
	if err != nil {
		return err
	}
	return a.Run(ctx)
}
