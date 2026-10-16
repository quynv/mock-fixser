package fixserver

import (
	"fmt"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"
	qconfig "github.com/quickfixgo/quickfix/config"

	"github.com/quynv/mock-fixser/fix/field"
	"github.com/quynv/mock-fixser/fix/fix44/businessmessagereject"
	"github.com/quynv/mock-fixser/fix/fix44/news"
	"github.com/quynv/mock-fixser/internal/config"
)

func testConfig(t *testing.T, clientPort, monitorPort int, targets ...string) *config.Config {
	t.Helper()
	y := fmt.Sprintf(`
server:
  fix:
    listen: 127.0.0.1:%d
    data_dictionary: %s
    sessions:
`, clientPort, dictPath)
	for _, tg := range targets {
		y += fmt.Sprintf("      - { sender_comp_id: MOCKSRV, target_comp_id: %s }\n", tg)
	}
	y += fmt.Sprintf(`  monitor:
    enabled: true
    listen: 127.0.0.1:%d
symbols: []
`, monitorPort)
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func startServer(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	s, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	return s
}

// testClient is an initiator that records logons, logouts and app messages.
type testClient struct {
	*testApp
	ini      *quickfix.Initiator
	loggedOf chan quickfix.SessionID
	appMsgs  chan *quickfix.Message
	id       quickfix.SessionID
}

type clientApp struct {
	*testApp
	logout chan quickfix.SessionID
	msgs   chan *quickfix.Message
}

func (a *clientApp) OnLogout(id quickfix.SessionID) { a.logout <- id }
func (a *clientApp) FromApp(m *quickfix.Message, _ quickfix.SessionID) quickfix.MessageRejectError {
	a.msgs <- m
	return nil
}

func dialClient(t *testing.T, port int, sender string, stores quickfix.MessageStoreFactory) *testClient {
	t.Helper()
	settings := quickfix.NewSettings()
	ss := sessionSettings(sender, "MOCKSRV")
	ss.Set(qconfig.SocketConnectHost, "127.0.0.1")
	ss.Set(qconfig.SocketConnectPort, strconv.Itoa(port))
	ss.Set(qconfig.ReconnectInterval, "1")
	id, err := settings.AddSession(ss)
	if err != nil {
		t.Fatal(err)
	}
	app := &clientApp{testApp: newTestApp(), logout: make(chan quickfix.SessionID, 10), msgs: make(chan *quickfix.Message, 10)}
	app.loggedOn = make(chan quickfix.SessionID, 10)
	if stores == nil {
		stores = quickfix.NewMemoryStoreFactory()
	}
	ini, err := quickfix.NewInitiator(app, stores, settings, quickfix.NewNullLogFactory())
	if err != nil {
		t.Fatal(err)
	}
	if err := ini.Start(); err != nil {
		t.Fatal(err)
	}
	c := &testClient{testApp: app.testApp, ini: ini, loggedOf: app.logout, appMsgs: app.msgs, id: id}
	t.Cleanup(ini.Stop)
	return c
}

func (c *testClient) waitLogon(t *testing.T) {
	t.Helper()
	select {
	case <-c.loggedOn:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not log on", c.id.SenderCompID)
	}
}

func (c *testClient) expectNoLogon(t *testing.T) {
	t.Helper()
	select {
	case <-c.loggedOn:
		t.Fatalf("%s logged on but should be refused", c.id.SenderCompID)
	case <-time.After(1500 * time.Millisecond):
	}
}

func TestClientSessionsFollowConfig(t *testing.T) {
	cp, mp := freePort(t), freePort(t)
	srv := startServer(t, testConfig(t, cp, mp, "CLIENT1"))

	dialClient(t, cp, "CLIENT1", nil).waitLogon(t)
	dialClient(t, cp, "CLIENT9", nil).expectNoLogon(t)
	// The monitor CompID is not accepted on the client port.
	dialClient(t, cp, "UI_MONITOR", nil).expectNoLogon(t)

	if !srv.LoggedOn(quickfix.SessionID{BeginString: "FIX.4.4", SenderCompID: "MOCKSRV", TargetCompID: "CLIENT1"}) {
		t.Error("server does not report CLIENT1 as logged on")
	}
}

func TestMonitorSessionOnLoopback(t *testing.T) {
	cp, mp := freePort(t), freePort(t)
	startServer(t, testConfig(t, cp, mp, "CLIENT1"))
	dialClient(t, mp, "UI_MONITOR", nil).waitLogon(t)
	// Client sessions are not served on the monitor port.
	dialClient(t, mp, "CLIENT1", nil).expectNoLogon(t)
}

func TestUnsupportedMessageGetsBusinessReject(t *testing.T) {
	cp, mp := freePort(t), freePort(t)
	startServer(t, testConfig(t, cp, mp, "CLIENT1"))
	c := dialClient(t, cp, "CLIENT1", nil)
	c.waitLogon(t)

	msg := news.New(field.NewHeadline("hello"))
	lines := news.NewNoLinesOfTextRepeatingGroup()
	lines.Add().SetText("body")
	msg.SetNoLinesOfText(lines)
	if err := quickfix.SendToTarget(msg, c.id); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-c.appMsgs:
		rej := businessmessagereject.FromMessage(m)
		reason, err := rej.GetBusinessRejectReason()
		if err != nil || reason != "3" {
			t.Fatalf("BusinessRejectReason = %v, %v; want 3", reason, err)
		}
		if mt, _ := rej.GetRefMsgType(); mt != "B" {
			t.Errorf("RefMsgType = %v, want B", mt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no BusinessMessageReject")
	}
}

// A reconnecting client sees the server continue its sequence numbers:
// two connections give server Logon(1), Logout(2), Logon(3), Logout(4).
func TestReconnectKeepsSequenceNumbers(t *testing.T) {
	cp, mp := freePort(t), freePort(t)
	startServer(t, testConfig(t, cp, mp, "CLIENT1"))
	clientStores := newStoreCache(quickfix.NewMemoryStoreFactory())

	for range 2 {
		c := dialClient(t, cp, "CLIENT1", clientStores)
		c.waitLogon(t)
		c.ini.Stop() // returns after the session goroutines end
	}

	st, _ := clientStores.Create(quickfix.SessionID{BeginString: "FIX.4.4", SenderCompID: "CLIENT1", TargetCompID: "MOCKSRV"})
	if got := st.NextTargetMsgSeqNum(); got != 5 {
		t.Fatalf("client next expected seq from server = %d, want 5", got)
	}
}

func TestHotAddAndRemoveSessions(t *testing.T) {
	cp, mp := freePort(t), freePort(t)
	oldCfg := testConfig(t, cp, mp, "CLIENT1")
	srv := startServer(t, oldCfg)

	c1 := dialClient(t, cp, "CLIENT1", nil)
	c1.waitLogon(t)
	c2 := dialClient(t, cp, "CLIENT2", nil)
	c2.expectNoLogon(t)

	newCfg := testConfig(t, cp, mp, "CLIENT2")
	srv.OnConfigChange(config.Change{Old: oldCfg, New: newCfg, Diff: config.Compare(oldCfg, newCfg)})

	select {
	case <-c1.loggedOf:
	case <-time.After(5 * time.Second):
		t.Fatal("removed session CLIENT1 was not logged out")
	}
	c2.waitLogon(t)
	c1.expectNoLogon(t)
}
