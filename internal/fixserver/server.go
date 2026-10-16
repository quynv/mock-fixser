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
	// static holds the sessions registered at construction; started tells
	// Stop whether the acceptors run (quickfixgo's Stop hangs otherwise).
	static  []quickfix.SessionID
	started bool

	// allowed holds the client sessions of the running config.
	allowed atomic.Pointer[map[quickfix.SessionID]bool]

	mu    sync.Mutex
	conns map[quickfix.SessionID]net.Conn

	tap atomic.Pointer[Tap]

	// Session faults of the running config, and their state per logon.
	faults      atomic.Pointer[map[quickfix.SessionID]*config.SessionFaults]
	faultStates map[quickfix.SessionID]*faultState
	flows       map[quickfix.SessionID]*flow
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
		faultStates: map[quickfix.SessionID]*faultState{},
		flows:       map[quickfix.SessionID]*flow{},
	}
	s.setAllowed(cfg)

	s.SetTap(nil)
	s.app.checkLogon = s.checkLogon
	s.app.onLogon = append(s.app.onLogon, s.startFaults)
	s.app.onLogout = append(s.app.onLogout, s.stopFaults, func(id quickfix.SessionID) { s.flowFor(id).reset() })
	logs := slogLogFactory{log: log, tap: &s.tap}
	cs, err := clientSettings(cfg)
	if err != nil {
		return nil, err
	}
	if s.client, err = quickfix.NewAcceptor(s.app, s.stores, cs, logs); err != nil {
		return nil, err
	}
	s.client.SetConnectionValidator(validatorFunc(s.validate))
	s.client.SetNewListenerCallback(newFaultListener)
	for id := range cs.SessionSettings() {
		s.static = append(s.static, id)
	}

	if cfg.Server.Monitor.Enabled {
		ms, err := monitorSettings(cfg.Server.FIX, cfg.Server.Monitor)
		if err != nil {
			s.unregisterStatic()
			return nil, err
		}
		if s.monitor, err = quickfix.NewAcceptor(s.app, newStoreCache(quickfix.NewMemoryStoreFactory()), ms, logs); err != nil {
			s.unregisterStatic()
			return nil, err
		}
		// The monitor session needs the write reports for flow control too.
		s.monitor.SetNewListenerCallback(newFaultListener)
		s.monitor.SetConnectionValidator(validatorFunc(func(c net.Conn, id quickfix.SessionID) error {
			if fc, ok := c.(*faultConn); ok {
				fc.flow.Store(s.flowFor(id))
			}
			return nil
		}))
		for id := range ms.SessionSettings() {
			s.static = append(s.static, id)
		}
	}
	return s, nil
}

// SetTap installs the raw message tap (the monitor mirror).
// The fault simulation always observes the traffic first.
func (s *Server) SetTap(t Tap) {
	all := Tap(func(id quickfix.SessionID, dir byte, raw []byte) {
		s.observe(id, dir, raw)
		if t != nil {
			t(id, dir, raw)
		}
	})
	s.tap.Store(&all)
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
			s.monitor = nil
			s.unregisterStatic()
			return err
		}
	}
	s.started = true
	return nil
}

// Stop logs out every session and closes the listeners. Without a prior
// Start it only releases the sessions registered by New.
func (s *Server) Stop() {
	if !s.started {
		s.unregisterStatic()
		return
	}
	s.started = false
	s.client.Stop()
	if s.monitor != nil {
		s.monitor.Stop()
	}
}

func (s *Server) unregisterStatic() {
	for _, id := range s.static {
		_ = quickfix.UnregisterSession(id)
	}
}

// OnLogout registers fn to run when any session logs out or disconnects.
// Register hooks before Start.
func (s *Server) OnLogout(fn func(quickfix.SessionID)) {
	s.app.mu.Lock()
	defer s.app.mu.Unlock()
	s.app.onLogout = append(s.app.onLogout, fn)
}

// SessionInfo is the admin view of a session.
type SessionInfo struct {
	ID            string                `json:"id"`
	Kind          string                `json:"kind"` // client or monitor
	Version       string                `json:"version"`
	SenderCompID  string                `json:"sender_comp_id"`
	TargetCompID  string                `json:"target_comp_id"`
	LoggedOn      bool                  `json:"logged_on"`
	NextSenderSeq int                   `json:"next_sender_seq,omitempty"`
	NextTargetSeq int                   `json:"next_target_seq,omitempty"`
	Remote        string                `json:"remote,omitempty"`
	Faults        *config.SessionFaults `json:"faults,omitempty"`
}

// Sessions lists the configured client sessions and the monitor session.
func (s *Server) Sessions(cfg *config.Config) []SessionInfo {
	var out []SessionInfo
	add := func(kind string, id quickfix.SessionID, faults *config.SessionFaults) {
		info := SessionInfo{ID: id.String(), Kind: kind, Version: id.BeginString, SenderCompID: id.SenderCompID,
			TargetCompID: id.TargetCompID, LoggedOn: s.app.loggedOn(id), Faults: faults}
		if id.BeginString == quickfix.BeginStringFIXT11 {
			info.Version = "FIX.5.0SP2"
		}
		if info.LoggedOn {
			info.NextSenderSeq, _ = quickfix.GetExpectedSenderNum(id)
			info.NextTargetSeq, _ = quickfix.GetExpectedTargetNum(id)
			if kind == "client" {
				if addr, ok := s.client.RemoteAddr(id); ok {
					info.Remote = addr.String()
				}
			}
		}
		out = append(out, info)
	}
	for _, cs := range cfg.Server.FIX.Sessions {
		add("client", SessionIDOf(cfg, cs), cs.Faults)
	}
	if m := cfg.Server.Monitor; m.Enabled {
		add("monitor", sessionID(s.beginString, config.SessionKey{SenderCompID: m.SenderCompID, TargetCompID: m.TargetCompID}), nil)
	}
	return out
}

// ErrNotLoggedOn is returned by Logout for a session that is not logged on.
var ErrNotLoggedOn = errors.New("session not logged on")

// Logout sends Logout to a logged-on client session and closes its
// connection after a grace period. The session may log on again.
func (s *Server) Logout(id quickfix.SessionID, text string) error {
	if !s.app.loggedOn(id) {
		return ErrNotLoggedOn
	}
	msg := logout.New()
	msg.Set(field.NewText(text))
	if err := quickfix.SendToTarget(msg, id); err != nil {
		return err
	}
	s.mu.Lock()
	conn := s.conns[id]
	delete(s.conns, id)
	s.mu.Unlock()
	if conn != nil {
		time.AfterFunc(removedLogoutGrace, func() { _ = conn.Close() })
	}
	return nil
}

// LoggedOn reports whether a session is currently logged on.
func (s *Server) LoggedOn(id quickfix.SessionID) bool { return s.app.loggedOn(id) }

// OnConfigChange admits added client sessions and logs out removed ones.
// Listen address and dictionary changes need a restart (section 9.3).
func (s *Server) OnConfigChange(c config.Change) {
	s.setAllowed(c.New)
	removed := map[config.SessionKey]bool{}
	for _, k := range c.Diff.SessionsRemoved {
		removed[k] = true
	}
	for _, cs := range c.Old.Server.FIX.Sessions {
		if removed[cs.Key()] {
			s.removeSession(SessionIDOf(c.Old, cs))
		}
	}
	added := map[config.SessionKey]bool{}
	for _, k := range c.Diff.SessionsAdded {
		added[k] = true
	}
	for _, cs := range c.New.Server.FIX.Sessions {
		if added[cs.Key()] {
			s.log.Info("session added", "session", SessionIDOf(c.New, cs).String())
		}
	}
}

func (s *Server) setAllowed(cfg *config.Config) {
	sessions := cfg.Server.FIX.Sessions
	m := make(map[quickfix.SessionID]bool, len(sessions))
	f := make(map[quickfix.SessionID]*config.SessionFaults, len(sessions))
	for _, cs := range sessions {
		id := SessionIDOf(cfg, cs)
		m[id] = true
		if cs.Faults != nil {
			f[id] = cs.Faults
		}
	}
	s.allowed.Store(&m)
	s.faults.Store(&f)
}

func (s *Server) validate(conn net.Conn, id quickfix.SessionID) error {
	if !(*s.allowed.Load())[id] {
		s.log.Warn("logon refused: session not configured", "session", id.String(), "remote", conn.RemoteAddr().String())
		return errSessionNotConfigured
	}
	// A second connection for a logged-on session is refused by quickfixgo
	// later; keep the live connection on record.
	if !s.app.loggedOn(id) {
		if fc, ok := conn.(*faultConn); ok {
			fc.flow.Store(s.flowFor(id))
		}
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
