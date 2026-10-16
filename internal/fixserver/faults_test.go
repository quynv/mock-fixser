package fixserver

import (
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"
	qconfig "github.com/quickfixgo/quickfix/config"

	"github.com/quynv/mock-fixser/fix/field"
	"github.com/quynv/mock-fixser/fix/fix44/news"
	"github.com/quynv/mock-fixser/fix/tag"
)

// faultClient records what a client sees: logons, logouts, admin messages.
type faultClient struct {
	id     quickfix.SessionID
	ini    *quickfix.Initiator
	logon  chan struct{}
	logout chan struct{}
	admin  chan *quickfix.Message
}

func (c *faultClient) OnCreate(quickfix.SessionID)                   {}
func (c *faultClient) OnLogon(quickfix.SessionID)                    { c.logon <- struct{}{} }
func (c *faultClient) OnLogout(quickfix.SessionID)                   { c.logout <- struct{}{} }
func (c *faultClient) ToAdmin(*quickfix.Message, quickfix.SessionID) {}
func (c *faultClient) ToApp(*quickfix.Message, quickfix.SessionID) error {
	return nil
}
func (c *faultClient) FromAdmin(m *quickfix.Message, _ quickfix.SessionID) quickfix.MessageRejectError {
	cp := quickfix.NewMessage()
	m.CopyInto(cp)
	c.admin <- cp
	return nil
}
func (c *faultClient) FromApp(*quickfix.Message, quickfix.SessionID) quickfix.MessageRejectError {
	return nil
}

func dialFault(t *testing.T, port int, sender string, heartBtInt int) *faultClient {
	t.Helper()
	settings := quickfix.NewSettings()
	ss := sessionSettings(sender, "MOCKSRV")
	ss.Set(qconfig.SocketConnectHost, "127.0.0.1")
	ss.Set(qconfig.SocketConnectPort, strconv.Itoa(port))
	ss.Set(qconfig.ReconnectInterval, "30") // no reconnect during a test
	ss.Set(qconfig.HeartBtInt, strconv.Itoa(heartBtInt))
	id, err := settings.AddSession(ss)
	if err != nil {
		t.Fatal(err)
	}
	c := &faultClient{id: id, logon: make(chan struct{}, 4), logout: make(chan struct{}, 4), admin: make(chan *quickfix.Message, 100)}
	ini, err := quickfix.NewInitiator(c, quickfix.NewMemoryStoreFactory(), settings, quickfix.NewNullLogFactory())
	if err != nil {
		t.Fatal(err)
	}
	if err := ini.Start(); err != nil {
		t.Fatal(err)
	}
	c.ini = ini
	t.Cleanup(ini.Stop)
	return c
}

func (c *faultClient) wait(t *testing.T, ch chan struct{}, what string, d time.Duration) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("%s: no %s within %v", c.id.SenderCompID, what, d)
	}
}

// waitAdmin returns the first admin message of msgType.
func (c *faultClient) waitAdmin(t *testing.T, msgType string, d time.Duration) *quickfix.Message {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case m := <-c.admin:
			if mt, _ := m.MsgType(); mt == msgType {
				return m
			}
		case <-deadline:
			t.Fatalf("no %s within %v", msgType, d)
		}
	}
}

// sendNews makes the server answer with BusinessMessageReject (one
// application message out per call).
func (c *faultClient) sendNews(t *testing.T) {
	t.Helper()
	msg := news.New(field.NewHeadline("x"))
	lines := news.NewNoLinesOfTextRepeatingGroup()
	lines.Add().SetText("x")
	msg.SetNoLinesOfText(lines)
	if err := quickfix.SendToTarget(msg, c.id); err != nil {
		t.Fatal(err)
	}
}

func TestFaultRejectLogon(t *testing.T) {
	cp, mp := freePort(t), freePort(t)
	startServer(t, testConfig(t, cp, mp, `CLIENT1, faults: { reject_logon: { text: "Try later" } }`))

	// A raw TCP client, to read the exact answer to Logon.
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cp)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	logon := quickfix.NewMessage()
	logon.Header.SetField(tag.BeginString, quickfix.FIXString("FIX.4.4"))
	logon.Header.SetField(tag.MsgType, quickfix.FIXString("A"))
	logon.Header.SetField(tag.SenderCompID, quickfix.FIXString("CLIENT1"))
	logon.Header.SetField(tag.TargetCompID, quickfix.FIXString("MOCKSRV"))
	logon.Header.SetField(tag.MsgSeqNum, quickfix.FIXInt(1))
	logon.Header.SetField(tag.SendingTime, quickfix.FIXUTCTimestamp{Time: time.Now()})
	logon.Body.SetField(tag.EncryptMethod, quickfix.FIXString("0"))
	logon.Body.SetField(tag.HeartBtInt, quickfix.FIXInt(30))
	if _, err := conn.Write([]byte(logon.String())); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reply, _ := io.ReadAll(conn) // the server closes after Logout
	if s := string(reply); !strings.Contains(s, "\x0135=5\x01") || !strings.Contains(s, "\x0158=Try later\x01") {
		t.Fatalf("reply = %q", s)
	}
}

func TestFaultDropAfterMessages(t *testing.T) {
	cp, mp := freePort(t), freePort(t)
	startServer(t, testConfig(t, cp, mp, `CLIENT1, faults: { drop_after: { messages: 5 } }`))
	c := dialFault(t, cp, "CLIENT1", 30)
	c.wait(t, c.logon, "logon", 5*time.Second)
	for range 3 { // 2 in + 2 out per round trip
		c.sendNews(t)
	}
	c.wait(t, c.logout, "disconnect", 5*time.Second)
}

func TestFaultDropAfterSeconds(t *testing.T) {
	cp, mp := freePort(t), freePort(t)
	startServer(t, testConfig(t, cp, mp, `CLIENT1, faults: { drop_after: { seconds: 1 } }`))
	c := dialFault(t, cp, "CLIENT1", 30)
	c.wait(t, c.logon, "logon", 5*time.Second)
	start := time.Now()
	c.wait(t, c.logout, "disconnect", 5*time.Second)
	if d := time.Since(start); d < 800*time.Millisecond {
		t.Fatalf("dropped after %v, want about 1s", d)
	}
}

func TestFaultSeqGapForcesResend(t *testing.T) {
	cp, mp := freePort(t), freePort(t)
	startServer(t, testConfig(t, cp, mp, `CLIENT1, faults: { seq_gap: { every: 2 } }`))
	c := dialFault(t, cp, "CLIENT1", 30)
	c.wait(t, c.logon, "logon", 5*time.Second)
	for range 3 {
		c.sendNews(t)
	}
	// The client sees the gap, asks for a resend and gets a gap fill.
	sr := c.waitAdmin(t, "4", 5*time.Second)
	if gf, _ := sr.Body.GetString(123); gf != "Y" {
		t.Fatalf("SequenceReset GapFillFlag = %q", gf)
	}
	select {
	case <-c.logout:
		t.Fatal("session dropped instead of recovering")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestFaultHeartbeatMiss(t *testing.T) {
	cp, mp := freePort(t), freePort(t)
	startServer(t, testConfig(t, cp, mp, `CLIENT1, faults: { heartbeat_miss: { after_seconds: 0 } }`))
	c := dialFault(t, cp, "CLIENT1", 1)
	c.wait(t, c.logon, "logon", 5*time.Second)
	// No heartbeats and no answer to TestRequest: the client gives up.
	c.wait(t, c.logout, "disconnect", 10*time.Second)
}

func TestTriggerFault(t *testing.T) {
	cp, mp := freePort(t), freePort(t)
	srv := startServer(t, testConfig(t, cp, mp, "CLIENT1"))
	id := quickfix.SessionID{BeginString: "FIX.4.4", SenderCompID: "MOCKSRV", TargetCompID: "CLIENT1"}
	if err := srv.TriggerFault(id, FaultDrop); err != ErrNotLoggedOn {
		t.Fatalf("offline: %v", err)
	}
	c := dialFault(t, cp, "CLIENT1", 30)
	c.wait(t, c.logon, "logon", 5*time.Second)

	if err := srv.TriggerFault(id, "explode"); err != ErrUnknownFault {
		t.Fatalf("unknown kind: %v", err)
	}
	if err := srv.TriggerFault(id, FaultSeqGap); err != nil {
		t.Fatal(err)
	}
	c.sendNews(t)
	c.waitAdmin(t, "4", 5*time.Second)

	if err := srv.TriggerFault(id, FaultDrop); err != nil {
		t.Fatal(err)
	}
	c.wait(t, c.logout, "disconnect", 5*time.Second)
}
