// Command mockfix runs the mock FIX server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
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
	// Config loading, acceptors, engines and HTTP are wired in from phase 1 (design doc, section 12.1).
	<-ctx.Done()
	slog.Info("mockfix stopped")
}
