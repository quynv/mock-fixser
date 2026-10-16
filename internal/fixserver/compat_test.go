package fixserver

// Phase 0 compatibility checks for quickfixgo (design doc, section 14.1):
// the acceptor binds to a specific host, and the custom U1 MirrorLog message
// round-trips between two sessions in the same process with dictionary
// validation enabled.

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"
	"github.com/quickfixgo/quickfix/config"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/fix/field"
	"github.com/quynv/mock-fixser/fix/fix44/mirrorlog"
)

const dictPath = "../../spec/FIX44.xml"

type testApp struct {
	*quickfix.MessageRouter
	loggedOn chan quickfix.SessionID
}

func newTestApp() *testApp {
	return &testApp{MessageRouter: quickfix.NewMessageRouter(), loggedOn: make(chan quickfix.SessionID, 1)}
}

func (a *testApp) OnCreate(quickfix.SessionID)                       {}
func (a *testApp) OnLogon(id quickfix.SessionID)                     { a.loggedOn <- id }
func (a *testApp) OnLogout(quickfix.SessionID)                       {}
func (a *testApp) ToAdmin(*quickfix.Message, quickfix.SessionID)     {}
func (a *testApp) ToApp(*quickfix.Message, quickfix.SessionID) error { return nil }
func (a *testApp) FromAdmin(*quickfix.Message, quickfix.SessionID) quickfix.MessageRejectError {
	return nil
}
func (a *testApp) FromApp(m *quickfix.Message, id quickfix.SessionID) quickfix.MessageRejectError {
	return a.Route(m, id)
}

func sessionSettings(sender, target string) *quickfix.SessionSettings {
	s := quickfix.NewSessionSettings()
	s.Set(config.BeginString, quickfix.BeginStringFIX44)
	s.Set(config.SenderCompID, sender)
	s.Set(config.TargetCompID, target)
	s.Set(config.DataDictionary, dictPath)
	s.Set(config.HeartBtInt, "30")
	return s
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestMonitorAcceptorBindsLoopbackAndCarriesU1(t *testing.T) {
	port := freePort(t)

	// Monitor acceptor: MOCKSRV -> UI_MONITOR on 127.0.0.1 only.
	accSettings := quickfix.NewSettings()
	accSettings.GlobalSettings().Set(config.SocketAcceptHost, "127.0.0.1")
	accSettings.GlobalSettings().Set(config.SocketAcceptPort, strconv.Itoa(port))
	if _, err := accSettings.AddSession(sessionSettings("MOCKSRV", "UI_MONITOR")); err != nil {
		t.Fatal(err)
	}
	server := newTestApp()
	acc, err := quickfix.NewAcceptor(server, quickfix.NewMemoryStoreFactory(), accSettings, quickfix.NewNullLogFactory())
	if err != nil {
		t.Fatal(err)
	}
	if err := acc.Start(); err != nil {
		t.Fatal(err)
	}
	defer acc.Stop()

	// The listener must not accept connections on a non-loopback address.
	if ip := nonLoopbackIP(); ip != "" {
		if c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), time.Second); err == nil {
			c.Close()
			t.Fatalf("monitor acceptor reachable on %s", ip)
		}
	}

	// Gateway initiator: UI_MONITOR -> MOCKSRV over loopback.
	iniSettings := quickfix.NewSettings()
	gw := sessionSettings("UI_MONITOR", "MOCKSRV")
	gw.Set(config.SocketConnectHost, "127.0.0.1")
	gw.Set(config.SocketConnectPort, strconv.Itoa(port))
	gw.Set(config.ReconnectInterval, "1")
	if _, err := iniSettings.AddSession(gw); err != nil {
		t.Fatal(err)
	}
	client := newTestApp()
	received := make(chan mirrorlog.MirrorLog, 1)
	client.AddRoute(mirrorlog.Route(func(m mirrorlog.MirrorLog, _ quickfix.SessionID) quickfix.MessageRejectError {
		received <- m
		return nil
	}))
	ini, err := quickfix.NewInitiator(client, quickfix.NewMemoryStoreFactory(), iniSettings, quickfix.NewNullLogFactory())
	if err != nil {
		t.Fatal(err)
	}
	if err := ini.Start(); err != nil {
		t.Fatal(err)
	}
	defer ini.Stop()

	var serverSession quickfix.SessionID
	select {
	case serverSession = <-server.loggedOn:
	case <-time.After(5 * time.Second):
		t.Fatal("UI_MONITOR did not log on")
	}

	raw := "8=FIX.4.4|9=5|35=0|10=000|"
	at := time.Date(2026, 10, 16, 1, 2, 3, 456789000, time.UTC)
	msg := mirrorlog.New(
		field.NewMirrorSessionID("FIX.4.4:MOCKSRV->CLIENT1"),
		field.NewMirrorDirection(enum.MirrorDirection_INBOUND),
		field.NewMirrorRawMsg(raw),
		field.NewMirrorTimeWithPrecision(at, quickfix.Micros),
	)
	if err := quickfix.SendToTarget(msg, serverSession); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-received:
		if v, err := got.GetMirrorRawMsg(); err != nil || v != raw {
			t.Errorf("MirrorRawMsg = %q, %v; want %q", v, err, raw)
		}
		if v, err := got.GetMirrorDirection(); err != nil || v != enum.MirrorDirection_INBOUND {
			t.Errorf("MirrorDirection = %q, %v", v, err)
		}
		if v, err := got.GetMirrorTime(); err != nil || !v.Equal(at) {
			t.Errorf("MirrorTime = %v, %v; want %v", v, err, at)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("U1 not received")
	}
}

func nonLoopbackIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	return ""
}
