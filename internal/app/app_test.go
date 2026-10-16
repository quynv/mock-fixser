package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"
	qconfig "github.com/quickfixgo/quickfix/config"
	"github.com/shopspring/decimal"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/fix/field"
	"github.com/quynv/mock-fixser/fix/fix44/marketdataincrementalrefresh"
	"github.com/quynv/mock-fixser/fix/fix44/marketdatarequest"
	"github.com/quynv/mock-fixser/fix/fix44/marketdatarequestreject"
	"github.com/quynv/mock-fixser/fix/fix44/marketdatasnapshotfullrefresh"
)

func repoPath(rel string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "../..", rel)
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

const symbolsYAML = `symbols:
  - { symbol: EURUSD, initial_price: 1.0850, min: 1.07, max: 1.10, tick_size: 0.00001, spread: 0.0001,
      model: gbm, volatility: 0.0002, seed: 42, depth_levels: 2, speed: { interval_ms: 20 } }
  - { symbol: AAPL, initial_price: 230, min: 200, max: 260, tick_size: 0.01, spread: 0.02,
      seed: 7, speed: { interval_ms: 20 } }
`

func writeConfig(t *testing.T, path string, port int, symbols string) {
	t.Helper()
	y := fmt.Sprintf(`server:
  fix:
    listen: 127.0.0.1:%d
    data_dictionary: %s
    sessions:
      - { sender_comp_id: MOCKSRV, target_comp_id: CLIENT1 }
  monitor:
    enabled: false
%s`, port, repoPath("spec/FIX44.xml"), symbols)
	if err := os.WriteFile(path, []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
}

type client struct {
	id     quickfix.SessionID
	logon  chan struct{}
	msgs   chan *quickfix.Message
	router *quickfix.MessageRouter
}

func (c *client) OnCreate(quickfix.SessionID)                   {}
func (c *client) OnLogon(quickfix.SessionID)                    { c.logon <- struct{}{} }
func (c *client) OnLogout(quickfix.SessionID)                   {}
func (c *client) ToAdmin(*quickfix.Message, quickfix.SessionID) {}
func (c *client) ToApp(*quickfix.Message, quickfix.SessionID) error {
	return nil
}
func (c *client) FromAdmin(*quickfix.Message, quickfix.SessionID) quickfix.MessageRejectError {
	return nil
}
func (c *client) FromApp(m *quickfix.Message, _ quickfix.SessionID) quickfix.MessageRejectError {
	c.msgs <- m
	return nil
}

func dial(t *testing.T, port int) *client {
	t.Helper()
	settings := quickfix.NewSettings()
	ss := quickfix.NewSessionSettings()
	ss.Set(qconfig.BeginString, "FIX.4.4")
	ss.Set(qconfig.SenderCompID, "CLIENT1")
	ss.Set(qconfig.TargetCompID, "MOCKSRV")
	ss.Set(qconfig.DataDictionary, repoPath("spec/FIX44.xml"))
	ss.Set(qconfig.HeartBtInt, "30")
	ss.Set(qconfig.SocketConnectHost, "127.0.0.1")
	ss.Set(qconfig.SocketConnectPort, strconv.Itoa(port))
	ss.Set(qconfig.ReconnectInterval, "1")
	id, err := settings.AddSession(ss)
	if err != nil {
		t.Fatal(err)
	}
	c := &client{id: id, logon: make(chan struct{}, 1), msgs: make(chan *quickfix.Message, 10000)}
	ini, err := quickfix.NewInitiator(c, quickfix.NewMemoryStoreFactory(), settings, quickfix.NewNullLogFactory())
	if err != nil {
		t.Fatal(err)
	}
	if err := ini.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ini.Stop)
	select {
	case <-c.logon:
	case <-time.After(5 * time.Second):
		t.Fatal("no logon")
	}
	return c
}

func (c *client) subscribe(t *testing.T, mdReqID string, symbols ...string) {
	t.Helper()
	m := marketdatarequest.New(field.NewMDReqID(mdReqID),
		field.NewSubscriptionRequestType(enum.SubscriptionRequestType_SNAPSHOTUPDATE), field.NewMarketDepth(0))
	types := marketdatarequest.NewNoMDEntryTypesRepeatingGroup()
	types.Add().SetMDEntryType(enum.MDEntryType_BID)
	types.Add().SetMDEntryType(enum.MDEntryType_OFFER)
	m.SetNoMDEntryTypes(types)
	syms := marketdatarequest.NewNoRelatedSymRepeatingGroup()
	for _, s := range symbols {
		syms.Add().SetSymbol(s)
	}
	m.SetNoRelatedSym(syms)
	if err := quickfix.SendToTarget(m, c.id); err != nil {
		t.Fatal(err)
	}
}

// waitFor returns the first message of msgType for mdReqID, skipping others.
func (c *client) waitFor(t *testing.T, msgType, mdReqID string) *quickfix.Message {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-c.msgs:
			mt, _ := m.MsgType()
			id, _ := m.Body.GetString(262)
			if mt == msgType && id == mdReqID {
				return m
			}
		case <-deadline:
			t.Fatalf("no %s for %s", msgType, mdReqID)
		}
	}
}

func startApp(t *testing.T, path string) {
	t.Helper()
	a, err := New(path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
}

func TestMarketDataOverFIX(t *testing.T) {
	port := freePort(t)
	path := filepath.Join(t.TempDir(), "mock.yaml")
	writeConfig(t, path, port, symbolsYAML)
	startApp(t, path)
	c := dial(t, port)

	c.subscribe(t, "md1", "EURUSD", "AAPL")
	w := marketdatasnapshotfullrefresh.FromMessage(c.waitFor(t, "W", "md1"))
	if sym, _ := w.GetSymbol(); sym != "EURUSD" {
		t.Errorf("first W symbol = %s, want EURUSD", sym)
	}
	if g, _ := w.GetNoMDEntries(); g.Len() != 4 {
		t.Errorf("EURUSD W entries = %d, want 4 (depth_levels 2)", g.Len())
	}

	seen := map[string]bool{}
	for len(seen) < 2 {
		x := marketdataincrementalrefresh.FromMessage(c.waitFor(t, "X", "md1"))
		g, err := x.GetNoMDEntries()
		if err != nil {
			t.Fatal(err)
		}
		e := g.Get(0)
		sym, _ := e.GetSymbol()
		pxv, _ := e.GetMDEntryPx()
		seen[sym] = true
		if sym == "AAPL" && (pxv.LessThan(mustDec("200")) || pxv.GreaterThan(mustDec("260"))) {
			t.Fatalf("AAPL px %s outside [200, 260]", pxv)
		}
	}

	c.subscribe(t, "bad", "NOPE")
	y := marketdatarequestreject.FromMessage(c.waitFor(t, "Y", "bad"))
	if r, _ := y.GetMDReqRejReason(); r != enum.MDReqRejReason_UNKNOWNSYM {
		t.Errorf("MDReqRejReason = %s, want 0", r)
	}

	// Hot reload: a new symbol can be subscribed without a restart.
	writeConfig(t, path, port, symbolsYAML+`  - { symbol: GBPUSD, initial_price: 1.27, min: 1.2, max: 1.4, tick_size: 0.00001,
      spread: 0.0001, speed: { interval_ms: 20 } }
`)
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.subscribe(t, "gbp"+strconv.FormatInt(time.Now().UnixNano(), 36), "GBPUSD")
		m := c.nextMD(t)
		if mt, _ := m.MsgType(); mt == "W" {
			if sym, _ := m.Body.GetString(55); strings.EqualFold(sym, "GBPUSD") {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("GBPUSD not available after config reload")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// nextMD returns the next W or Y, skipping X ticks.
func (c *client) nextMD(t *testing.T) *quickfix.Message {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-c.msgs:
			if mt, _ := m.MsgType(); mt == "W" || mt == "Y" {
				return m
			}
		case <-deadline:
			t.Fatal("no W or Y")
		}
	}
}

func mustDec(s string) decimal.Decimal { return decimal.RequireFromString(s) }
