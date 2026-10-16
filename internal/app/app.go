// Package app wires the mockfix components together in the startup and
// shutdown order of design doc section 12.1.
package app

import (
	"context"
	"log/slog"

	"golang.org/x/sync/errgroup"

	"github.com/quynv/mock-fixser/internal/clock"
	"github.com/quynv/mock-fixser/internal/config"
	"github.com/quynv/mock-fixser/internal/fixserver"
	"github.com/quynv/mock-fixser/internal/marketdata"
)

type App struct {
	log    *slog.Logger
	store  *config.Store
	srv    *fixserver.Server
	engine *marketdata.Engine
	pub    *marketdata.Publisher
}

// New loads the config and builds every component without starting them.
// Config errors are a *config.ValidationError.
func New(configPath string, log *slog.Logger) (*App, error) {
	store, err := config.Open(configPath, config.Options{Logger: log})
	if err != nil {
		return nil, err
	}
	cfg := store.Current()
	a := &App{log: log, store: store}

	if a.srv, err = fixserver.New(cfg, log); err != nil {
		return nil, err
	}
	clk := clock.Real{}
	if a.engine, err = marketdata.NewEngine(cfg.Symbols, clk, log, func(q marketdata.Quote) { a.pub.OnQuote(q) }); err != nil {
		return nil, err
	}
	a.pub = marketdata.NewPublisher(a.engine, cfg, clk, log, nil)

	a.pub.Route(a.srv.Router())
	a.srv.OnLogout(a.pub.OnLogout)
	store.Subscribe(a.srv.OnConfigChange)
	store.Subscribe(func(c config.Change) {
		if err := a.engine.ApplySymbols(c.New.Symbols); err != nil {
			a.log.Error("apply symbols failed", "error", err)
		}
		a.pub.SetConfig(c.New)
	})
	return a, nil
}

// Config returns the running config.
func (a *App) Config() *config.Config { return a.store.Current() }

// Run starts the acceptors, the market data engine and the config watcher,
// and shuts them down when ctx ends.
func (a *App) Run(ctx context.Context) error {
	cfg := a.store.Current()
	a.log.Info("config loaded", "version", cfg.Version, "sessions", len(cfg.Server.FIX.Sessions), "symbols", len(cfg.Symbols))
	if err := a.srv.Start(); err != nil {
		return err
	}
	a.log.Info("acceptors started", "client", cfg.Server.FIX.Listen, "monitor", cfg.Server.Monitor.Listen,
		"monitor_enabled", cfg.Server.Monitor.Enabled)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return a.engine.Run(gctx) })
	g.Go(func() error { return a.store.Watch(gctx) })

	<-gctx.Done()
	a.srv.Stop()
	a.pub.Close()
	return g.Wait()
}
