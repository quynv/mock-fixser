// Package app wires the mockfix components together in the startup and
// shutdown order of design doc section 12.1.
package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/quynv/mock-fixser/internal/admin"
	"github.com/quynv/mock-fixser/internal/clock"
	"github.com/quynv/mock-fixser/internal/config"
	"github.com/quynv/mock-fixser/internal/fixserver"
	"github.com/quynv/mock-fixser/internal/gateway"
	"github.com/quynv/mock-fixser/internal/marketdata"
	"github.com/quynv/mock-fixser/internal/monitor"
	"github.com/quynv/mock-fixser/internal/order"
	"github.com/quynv/mock-fixser/internal/sched"
	"github.com/quynv/mock-fixser/web"
)

type Options struct {
	// NoUI skips the HTTP server and the FIX gateway (for CI).
	NoUI bool
	// Pprof serves /debug/pprof on the HTTP server.
	Pprof bool
}

type App struct {
	log    *slog.Logger
	opts   Options
	store  *config.Store
	srv    *fixserver.Server
	engine *marketdata.Engine
	pub    *marketdata.Publisher
	sched  *sched.Scheduler
	orders *order.Engine
	mirror *monitor.Mirror
	gw     *gateway.Gateway // nil with NoUI or without the monitor session

	httpAddr atomic.Pointer[string]
}

// New loads the config and builds every component without starting them.
// Config errors are a *config.ValidationError.
func New(configPath string, log *slog.Logger, opts Options) (*App, error) {
	store, err := config.Open(configPath, config.Options{Logger: log, Validators: []config.Validator{order.Validate}})
	if err != nil {
		return nil, err
	}
	cfg := store.Current()
	a := &App{log: log, opts: opts, store: store}

	if a.srv, err = fixserver.New(cfg, log); err != nil {
		return nil, err
	}
	clk := clock.Real{}
	// Every tick goes to the subscribers and to orders waiting for the market.
	onQuote := func(q marketdata.Quote) {
		a.pub.OnQuote(q)
		a.orders.OnQuote(q)
	}
	if a.engine, err = marketdata.NewEngine(cfg.Symbols, clk, log, onQuote); err != nil {
		a.srv.Stop()
		return nil, err
	}
	// Sends go through the server's flow control (orders first).
	a.pub = marketdata.NewPublisher(a.engine, cfg, clk, log, a.srv.SendMarketData)
	a.sched = sched.New(clk)
	if a.orders, err = order.NewEngine(cfg, a.engine, a.sched, log, a.srv.SendOrder); err != nil {
		a.srv.Stop()
		return nil, err
	}
	a.mirror = monitor.New(cfg, clk, log, a.srv.SendMarketData)
	if !opts.NoUI && cfg.Server.Monitor.Enabled {
		if a.gw, err = gateway.New(cfg, log); err != nil {
			a.srv.Stop()
			return nil, err
		}
	}

	router := a.srv.Router()
	a.pub.Route(router)
	a.orders.Route(router)
	a.mirror.Route(router)
	a.srv.SetTap(a.mirror.Capture)
	a.srv.OnLogout(a.pub.OnLogout)
	a.srv.OnLogout(a.mirror.OnLogout)

	store.Subscribe(a.srv.OnConfigChange)
	store.Subscribe(func(c config.Change) {
		if err := a.engine.ApplySymbols(c.New.Symbols); err != nil {
			a.log.Error("apply symbols failed", "error", err)
		}
		a.pub.SetConfig(c.New)
		if err := a.orders.SetConfig(c.New); err != nil {
			a.log.Error("apply order rules failed", "error", err)
		}
		a.mirror.SetConfig(c.New)
	})
	return a, nil
}

// Config returns the running config.
func (a *App) Config() *config.Config { return a.store.Current() }

// HTTPAddr returns the HTTP listen address once Run has opened it.
func (a *App) HTTPAddr() string {
	if p := a.httpAddr.Load(); p != nil {
		return *p
	}
	return ""
}

// Run starts the components in the order of section 12.1 and shuts them
// down in reverse when ctx ends: acceptors, gateway initiator, engines and
// config watcher, HTTP.
func (a *App) Run(ctx context.Context) error {
	cfg := a.store.Current()
	a.log.Info("config loaded", "version", cfg.Version, "sessions", len(cfg.Server.FIX.Sessions), "symbols", len(cfg.Symbols))
	if err := a.srv.Start(); err != nil {
		return err
	}
	a.log.Info("acceptors started", "client", cfg.Server.FIX.Listen, "monitor", cfg.Server.Monitor.Listen,
		"monitor_enabled", cfg.Server.Monitor.Enabled)
	if a.gw != nil {
		if err := a.gw.Start(); err != nil {
			a.srv.Stop()
			return err
		}
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return a.engine.Run(gctx) })
	g.Go(func() error { return a.sched.Run(gctx) })
	g.Go(func() error { return a.mirror.Run(gctx) })
	g.Go(func() error { return a.store.Watch(gctx) })

	var httpSrv *http.Server
	if !a.opts.NoUI {
		ln, err := net.Listen("tcp", cfg.Server.HTTP.Listen)
		if err != nil {
			a.shutdown(nil)
			return errors.Join(err, g.Wait())
		}
		addr := ln.Addr().String()
		a.httpAddr.Store(&addr)
		deps := admin.Deps{Store: a.store, Market: a.engine, Quotes: a.pub, Orders: a.orders, Server: a.srv, UI: web.FS(), Pprof: a.opts.Pprof, Log: a.log}
		if a.gw != nil {
			deps.WS = a.gw
		}
		httpSrv = &http.Server{Handler: admin.Handler(deps), ReadHeaderTimeout: 10 * time.Second}
		g.Go(func() error {
			if err := httpSrv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
		a.log.Info("http server started", "listen", ln.Addr().String())
	}

	<-gctx.Done()
	a.shutdown(httpSrv)
	return g.Wait()
}

// shutdown closes HTTP, stops the gateway, logs out the client sessions
// and stops the acceptors.
func (a *App) shutdown(httpSrv *http.Server) {
	if httpSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = httpSrv.Shutdown(ctx)
		cancel()
	}
	if a.gw != nil {
		a.gw.Stop()
	}
	a.srv.Stop()
	a.pub.Close()
	a.orders.Close()
}
