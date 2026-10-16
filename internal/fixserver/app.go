package fixserver

import (
	"log/slog"
	"sync"

	"github.com/quickfixgo/quickfix"

	"github.com/quynv/mock-fixser/internal/metrics"
)

// App is the quickfix.Application shared by both acceptors. Application
// messages are dispatched by MsgType through the router; types without a
// route get BusinessMessageReject (j) with reason 3 (section 5.2).
type App struct {
	log    *slog.Logger
	router *quickfix.MessageRouter

	mu       sync.Mutex
	logged   map[quickfix.SessionID]bool
	onLogout []func(quickfix.SessionID)
}

func newApp(log *slog.Logger) *App {
	return &App{log: log, router: quickfix.NewMessageRouter(), logged: map[quickfix.SessionID]bool{}}
}

func (a *App) loggedOn(id quickfix.SessionID) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.logged[id]
}

func (a *App) OnCreate(quickfix.SessionID) {}

func (a *App) OnLogon(id quickfix.SessionID) {
	a.mu.Lock()
	a.logged[id] = true
	metrics.SessionsLoggedOn.Set(float64(len(a.logged)))
	a.mu.Unlock()
	a.log.Info("session logon", "session", id.String())
}

func (a *App) OnLogout(id quickfix.SessionID) {
	a.mu.Lock()
	delete(a.logged, id)
	metrics.SessionsLoggedOn.Set(float64(len(a.logged)))
	hooks := a.onLogout
	a.mu.Unlock()
	a.log.Info("session logout", "session", id.String())
	for _, fn := range hooks {
		fn(id)
	}
}

func (a *App) ToAdmin(*quickfix.Message, quickfix.SessionID) {}

func (a *App) ToApp(*quickfix.Message, quickfix.SessionID) error { return nil }

func (a *App) FromAdmin(*quickfix.Message, quickfix.SessionID) quickfix.MessageRejectError {
	return nil
}

func (a *App) FromApp(msg *quickfix.Message, id quickfix.SessionID) quickfix.MessageRejectError {
	return a.router.Route(msg, id)
}
