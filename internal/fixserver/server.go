package fixserver

import (
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quickfixgo/quickfix"

	"github.com/quynv/mock-fixser/fix/field"
	"github.com/quynv/mock-fixser/fix/fix44/logout"
	"github.com/quynv/mock-fixser/internal/config"
)

// removedLogoutGrace is how long a removed session has to answer our
// Logout before its connection is closed.
const removedLogoutGrace = 2 * time.Second

var errSessionNotConfigured = errors.New("session not in config")

// Server runs the client and monitor acceptors (section 5.1).
type Server struct {
	log         *slog.Logger
	app         *App
	beginString string
	stores      *storeCache

	client  *quickfix.Acceptor
	monitor *quickfix.Acceptor

	// allowed holds the client sessions of the running config.
	allowed atomic.Pointer[map[quickfix.SessionID]bool]

	mu    sync.Mutex
	conns map[quickfix.SessionID]net.Conn
}

// New builds the acceptors from cfg. Call Start to open the listeners.
func New(cfg *config.Config, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		log:         log,
		app:         newApp(log),
		beginString: cfg.Server.FIX.Version,
		stores:      newStoreCache(quickfix.NewMemoryStoreFactory()),
		conns:       map[quickfix.SessionID]net.Conn{},
	}
	s.setAllowed(cfg.Server.FIX.Sessions)

	logs := slogLogFactory{log}
	cs, err := clientSettings(cfg.Server.FIX)
	if err != nil {
		return nil, err
	}
	if s.client, err = quickfix.NewAcceptor(s.app, s.stores, cs, logs); err != nil {
		return nil, err
	}
	s.client.SetConnectionValidator(validatorFunc(s.validate))

	if cfg.Server.Monitor.Enabled {
		ms, err := monitorSettings(cfg.Server.FIX, cfg.Server.Monitor)
		if err != nil {
			return nil, err
		}
		if s.monitor, err = quickfix.NewAcceptor(s.app, quickfix.NewMemoryStoreFactory(), ms, logs); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Router lets the engines register MsgType handlers (section 5.2).
func (s *Server) Router() *quickfix.MessageRouter { return s.app.router }

// Start opens both acceptors. If the monitor fails to start, the client
// acceptor is stopped again.
func (s *Server) Start() error {
	if err := s.client.Start(); err != nil {
		return err
	}
	if s.monitor != nil {
		if err := s.monitor.Start(); err != nil {
			s.client.Stop()
			return err
		}
	}
	return nil
}

// Stop logs out every session and closes the listeners.
func (s *Server) Stop() {
	s.client.Stop()
	if s.monitor != nil {
		s.monitor.Stop()
	}
}

// OnLogout registers fn to run when any session logs out or disconnects.
// Register hooks before Start.
func (s *Server) OnLogout(fn func(quickfix.SessionID)) {
	s.app.mu.Lock()
	defer s.app.mu.Unlock()
	s.app.onLogout = append(s.app.onLogout, fn)
}

// LoggedOn reports whether a session is currently logged on.
func (s *Server) LoggedOn(id quickfix.SessionID) bool { return s.app.loggedOn(id) }

// OnConfigChange admits added client sessions and logs out removed ones.
// Listen address and dictionary changes need a restart (section 9.3).
func (s *Server) OnConfigChange(c config.Change) {
	s.setAllowed(c.New.Server.FIX.Sessions)
	for _, k := range c.Diff.SessionsRemoved {
		s.removeSession(sessionID(s.beginString, k))
	}
	for _, k := range c.Diff.SessionsAdded {
		s.log.Info("session added", "session", sessionID(s.beginString, k).String())
	}
}

func (s *Server) setAllowed(sessions []config.Session) {
	m := make(map[quickfix.SessionID]bool, len(sessions))
	for _, cs := range sessions {
		m[sessionID(s.beginString, cs.Key())] = true
	}
	s.allowed.Store(&m)
}

func (s *Server) validate(conn net.Conn, id quickfix.SessionID) error {
	if !(*s.allowed.Load())[id] {
		s.log.Warn("logon refused: session not configured", "session", id.String(), "remote", conn.RemoteAddr().String())
		return errSessionNotConfigured
	}
	// A second connection for a logged-on session is refused by quickfixgo
	// later; keep the live connection on record.
	if !s.app.loggedOn(id) {
		s.mu.Lock()
		s.conns[id] = conn
		s.mu.Unlock()
	}
	return nil
}

func (s *Server) removeSession(id quickfix.SessionID) {
	s.log.Info("session removed", "session", id.String())
	s.stores.drop(id)

	s.mu.Lock()
	conn := s.conns[id]
	delete(s.conns, id)
	s.mu.Unlock()
	if conn == nil {
		return
	}
	if s.app.loggedOn(id) {
		msg := logout.New()
		msg.Set(field.NewText("Session removed from configuration"))
		if err := quickfix.SendToTarget(msg, id); err != nil {
			s.log.Warn("send logout failed", "session", id.String(), "error", err)
		}
	}
	time.AfterFunc(removedLogoutGrace, func() { _ = conn.Close() })
}

type validatorFunc func(net.Conn, quickfix.SessionID) error

func (f validatorFunc) Validate(c net.Conn, id quickfix.SessionID) error { return f(c, id) }
