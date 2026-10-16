package fixserver

import (
	"fmt"
	"net"

	"github.com/quickfixgo/quickfix"
	"github.com/quickfixgo/quickfix/config"

	mfconfig "github.com/quynv/mock-fixser/internal/config"
)

// listenerPlaceholderTarget names a static session that is never allowed to
// log on. quickfixgo opens listeners only for configured sessions, while
// mockfix accepts client sessions dynamically so they can be added and
// removed without restarting the acceptor.
const listenerPlaceholderTarget = "__MOCKFIX_LISTENER__"

// clientSettings builds the client acceptor settings (section 5.1). Client
// sessions are dynamic: the ConnectionValidator admits only CompIDs that
// are in the running config.
func clientSettings(fix mfconfig.FIXServer) (*quickfix.Settings, error) {
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
