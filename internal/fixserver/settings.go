package fixserver

import (
	"fmt"
	"net"

	"github.com/quickfixgo/quickfix"
	"github.com/quickfixgo/quickfix/config"

	"github.com/quynv/mock-fixser/internal/fixver"

	mfconfig "github.com/quynv/mock-fixser/internal/config"
)

// listenerPlaceholderTarget names a static session that is never allowed to
// log on. quickfixgo opens listeners only for configured sessions, while
// mockfix accepts client sessions dynamically so they can be added and
// removed without restarting the acceptor.
const listenerPlaceholderTarget = "__MOCKFIX_LISTENER__"

// clientSettings builds the client acceptor settings (section 5.1). Client
// sessions of the default version are dynamic: the ConnectionValidator
// admits only CompIDs that are in the running config. Sessions of other
// versions need their own dictionaries, so they are static sessions,
// created when the acceptor starts (section 5.3).
func clientSettings(cfg *mfconfig.Config) (*quickfix.Settings, error) {
	fix := cfg.Server.FIX
	host, port, err := net.SplitHostPort(fix.Listen)
	if err != nil {
		return nil, fmt.Errorf("server.fix.listen: %w", err)
	}
	s := quickfix.NewSettings()
	g := s.GlobalSettings()
	g.Set(config.SocketAcceptHost, host)
	g.Set(config.SocketAcceptPort, port)
	g.Set(config.DynamicSessions, "Y")
	g.Set(config.BeginString, fix.Version)
	g.Set(config.DataDictionary, fix.DataDictionary)

	ph := quickfix.NewSessionSettings()
	ph.Set(config.SenderCompID, senderOrDefault(fix))
	ph.Set(config.TargetCompID, listenerPlaceholderTarget)
	if _, err := s.AddSession(ph); err != nil {
		return nil, err
	}

	for _, cs := range fix.Sessions {
		v := cfg.SessionVersion(cs)
		if v == fix.Version {
			continue
		}
		ss := quickfix.NewSessionSettings()
		ss.Set(config.BeginString, fixver.BeginString(v))
		ss.Set(config.SenderCompID, cs.SenderCompID)
		ss.Set(config.TargetCompID, cs.TargetCompID)
		app, transport := fixver.Dictionaries(v, fix.DataDictionary)
		if transport != "" {
			ss.Set(config.TransportDataDictionary, transport)
			ss.Set(config.AppDataDictionary, app)
			ss.Set(config.DefaultApplVerID, v)
		} else {
			ss.Set(config.DataDictionary, app)
		}
		if _, err := s.AddSession(ss); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// monitorSettings builds the loopback-only monitor acceptor settings.
func monitorSettings(fix mfconfig.FIXServer, mon mfconfig.Monitor) (*quickfix.Settings, error) {
	host, port, err := net.SplitHostPort(mon.Listen)
	if err != nil {
		return nil, fmt.Errorf("server.monitor.listen: %w", err)
	}
	s := quickfix.NewSettings()
	g := s.GlobalSettings()
	g.Set(config.SocketAcceptHost, host)
	g.Set(config.SocketAcceptPort, port)
	g.Set(config.BeginString, fix.Version)
	g.Set(config.DataDictionary, fix.DataDictionary)

	ss := quickfix.NewSessionSettings()
	ss.Set(config.SenderCompID, mon.SenderCompID)
	ss.Set(config.TargetCompID, mon.TargetCompID)
	if _, err := s.AddSession(ss); err != nil {
		return nil, err
	}
	return s, nil
}

func senderOrDefault(fix mfconfig.FIXServer) string {
	if len(fix.Sessions) > 0 {
		return fix.Sessions[0].SenderCompID
	}
	return "MOCKSRV"
}

func sessionID(beginString string, k mfconfig.SessionKey) quickfix.SessionID {
	return quickfix.SessionID{BeginString: beginString, SenderCompID: k.SenderCompID, TargetCompID: k.TargetCompID}
}

// SessionIDOf returns the quickfix SessionID of a configured client session.
func SessionIDOf(cfg *mfconfig.Config, cs mfconfig.Session) quickfix.SessionID {
	return sessionID(fixver.BeginString(cfg.SessionVersion(cs)), cs.Key())
}
