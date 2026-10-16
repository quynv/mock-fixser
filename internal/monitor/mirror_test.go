package monitor

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"

	"github.com/quynv/mock-fixser/fix/field"
	"github.com/quynv/mock-fixser/fix/fix44/monitorcontrol"
	"github.com/quynv/mock-fixser/fix/tag"
	"github.com/quynv/mock-fixser/internal/clock"
	"github.com/quynv/mock-fixser/internal/config"
)

var (
	client  = quickfix.SessionID{BeginString: "FIX.4.4", SenderCompID: "MOCKSRV", TargetCompID: "CLIENT1"}
	monitor = quickfix.SessionID{BeginString: "FIX.4.4", SenderCompID: "MOCKSRV", TargetCompID: "UI_MONITOR"}
)

func testCfg(buffer int) *config.Config {
	c := &config.Config{}
	c.Server.FIX.Version = "FIX.4.4"
	c.Server.Monitor = config.Monitor{Enabled: true, MirrorLog: true, SenderCompID: "MOCKSRV", TargetCompID: "UI_MONITOR",
		MirrorBuffer: buffer, MirrorExclude: []string{"0"}}
	return c
}

func control(t *testing.T, m *Mirror, id quickfix.SessionID, key, val string) quickfix.MessageRejectError {
	t.Helper()
	msg := monitorcontrol.New(field.NewControlKey(key), field.NewControlValue(val))
	return m.HandleControl(msg, id)
}

const raw = "8=FIX.4.4\x019=5\x0135=D\x0111=C1\x0110=000\x01"

func TestMirrorSendsU1WhenTurnedOn(t *testing.T) {
	out := make(chan *quickfix.Message, 10)
	m := New(testCfg(10), clock.NewFake(time.Date(2026, 10, 16, 1, 2, 3, 456789000, time.UTC)), slog.New(slog.DiscardHandler),
		func(msg *quickfix.Message, id quickfix.SessionID) error {
			if id != monitor {
				t.Errorf("sent to %v", id)
			}
			out <- msg
			return nil
		})
	go m.Run(t.Context())

	m.Capture(client, 'I', []byte(raw)) // off by default
	if err := control(t, m, monitor, "log", "on"); err != nil {
		t.Fatal(err)
	}
	m.Capture(client, 'I', []byte(strings.Replace(raw, "35=D", "35=0", 1))) // heartbeat excluded
	m.Capture(monitor, 'O', []byte(raw))                                    // monitor's own traffic
	m.Capture(client, 'I', []byte(raw))

	select {
	case u1 := <-out:
		mt, _ := u1.MsgType()
		sid, _ := u1.Body.GetString(tag.MirrorSessionID)
		dir, _ := u1.Body.GetString(tag.MirrorDirection)
		body, _ := u1.Body.GetString(tag.MirrorRawMsg)
		if mt != "U1" || sid != "FIX.4.4:MOCKSRV->CLIENT1" || dir != "I" || body != strings.ReplaceAll(raw, "\x01", "|") {
			t.Fatalf("U1 = %s", u1)
		}
		if ts, _ := u1.Body.GetString(tag.MirrorTime); ts != "20261016-01:02:03.456789" {
			t.Errorf("MirrorTime = %s", ts)
		}
	case <-time.After(time.Second):
		t.Fatal("no U1")
	}
	select {
	case u1 := <-out:
		t.Fatalf("unexpected U1 %s", u1)
	case <-time.After(50 * time.Millisecond):
	}

	if err := control(t, m, monitor, "log", "off"); err != nil {
		t.Fatal(err)
	}
	m.Capture(client, 'O', []byte(raw))
	if len(m.ch) != 0 {
		t.Fatal("captured while off")
	}
}

func TestMirrorDropsWhenFull(t *testing.T) {
	m := New(testCfg(2), clock.Real{}, slog.New(slog.DiscardHandler), nil)
	m.deliver.Store(true)
	for range 5 { // no Run: nothing drains
		m.Capture(client, 'I', []byte(raw))
	}
	if m.Dropped() != 3 || len(m.ch) != 2 {
		t.Fatalf("dropped %d, buffered %d", m.Dropped(), len(m.ch))
	}
}

func TestControlRejects(t *testing.T) {
	m := New(testCfg(2), clock.Real{}, slog.New(slog.DiscardHandler), nil)
	if err := control(t, m, client, "log", "on"); err == nil || m.Delivering() {
		t.Fatal("U2 from a client session accepted")
	}
	if err := control(t, m, monitor, "log", "maybe"); err == nil {
		t.Fatal("bad value accepted")
	}
	control(t, m, monitor, "log", "on")
	m.OnLogout(monitor)
	if m.Delivering() {
		t.Fatal("delivery still on after monitor logout")
	}
}
