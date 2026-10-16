package order

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"
	"github.com/shopspring/decimal"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/fix/field"
	"github.com/quynv/mock-fixser/fix/fix44/newordersingle"
	"github.com/quynv/mock-fixser/fix/fix44/ordercancelreplacerequest"
	"github.com/quynv/mock-fixser/fix/fix44/ordercancelrequest"
	"github.com/quynv/mock-fixser/fix/fix44/orderstatusrequest"
	"github.com/quynv/mock-fixser/fix/tag"
	"github.com/quynv/mock-fixser/internal/clock"
	"github.com/quynv/mock-fixser/internal/config"
	"github.com/quynv/mock-fixser/internal/marketdata"
	"github.com/quynv/mock-fixser/internal/sched"
)

var (
	t0      = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	client1 = quickfix.SessionID{BeginString: "FIX.4.4", SenderCompID: "MOCKSRV", TargetCompID: "CLIENT1"}
	monitor = quickfix.SessionID{BeginString: "FIX.4.4", SenderCompID: "MOCKSRV", TargetCompID: "UI_MONITOR"}
)

type fakeQuotes map[string]struct {
	q   marketdata.Quote
	cfg config.Symbol
}

func (f fakeQuotes) Last(s string) (marketdata.Quote, config.Symbol, bool) {
	v, ok := f[s]
	return v.q, v.cfg, ok
}

func aaplQuotes(sl *config.Slippage) fakeQuotes {
	return fakeQuotes{"AAPL": {
		q:   marketdata.Quote{Symbol: "AAPL", Bid: 229.99, Ask: 230.01},
		cfg: config.Symbol{Symbol: "AAPL", TickSize: 0.01, Slippage: sl, Seed: new(int64(42))},
	}}
}

type out struct {
	session quickfix.SessionID
	m       *quickfix.Message
}

type harness struct {
	t     *testing.T
	clk   *clock.Fake
	sched *sched.Scheduler
	e     *Engine
	out   chan out
}

const engineCfg = `
server:
  fix:
    sessions: [ { sender_comp_id: MOCKSRV, target_comp_id: CLIENT1 } ]
  monitor: { enabled: true, drop_copy: %v }
symbols:
  - { symbol: AAPL, initial_price: 230, min: 200, max: 260, tick_size: 0.01, spread: 0.02, speed: { interval_ms: 100 } }
`

func newHarness(t *testing.T, rules string, quotes fakeQuotes, dropCopy bool) *harness {
	t.Helper()
	cfg, err := config.ParseWith([]byte(strings.Replace(engineCfg, "%v", map[bool]string{true: "true", false: "false"}[dropCopy], 1)+rules), Validate)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, clk: clock.NewFake(t0), out: make(chan out, 1000)}
	s := sched.New(h.clk)
	h.sched = s
	send := func(m *quickfix.Message, id quickfix.SessionID) error {
		h.out <- out{id, wire(m)}
		return nil
	}
	if h.e, err = NewEngine(cfg, quotes, s, slog.New(slog.DiscardHandler), send); err != nil {
		t.Fatal(err)
	}
	// No Run goroutine: advance steps the scheduler synchronously so every
	// task runs at exactly its due time, in a deterministic order.
	t.Cleanup(h.e.Close)
	return h
}

// wire round-trips a message through the FIX wire format.
func wire(m *quickfix.Message) *quickfix.Message {
	m.Header.SetField(tag.BeginString, quickfix.FIXString("FIX.4.4"))
	p := quickfix.NewMessage()
	if err := quickfix.ParseMessage(p, bytes.NewBufferString(m.String())); err != nil {
		panic(err)
	}
	return p
}

func (h *harness) handle(m quickfix.Messagable) {
	h.t.Helper()
	msg := wire(m.ToMessage())
	if err := h.e.handle(msg, client1); err != nil {
		h.t.Fatalf("handler error: %v", err)
	}
	h.sched.RunDue() // steps with no delay
}

// handle dispatches like the router would.
func (e *Engine) handle(m *quickfix.Message, id quickfix.SessionID) quickfix.MessageRejectError {
	r := quickfix.NewMessageRouter()
	e.Route(r)
	return r.Route(m, id)
}

// advance moves the fake clock forward, stopping at each task's due time
// to run it.
func (h *harness) advance(d time.Duration) {
	end := h.clk.Now().Add(d)
	for {
		next, ok := h.sched.Next()
		if !ok || next.After(end) {
			break
		}
		h.clk.Advance(next.Sub(h.clk.Now()))
		h.sched.RunDue()
	}
	h.clk.Advance(end.Sub(h.clk.Now()))
	h.sched.RunDue()
}

func (h *harness) next() out {
	h.t.Helper()
	select {
	case o := <-h.out:
		return o
	case <-time.After(2 * time.Second):
		h.t.Fatal("no message sent")
	}
	return out{}
}

func (h *harness) none() {
	h.t.Helper()
	select {
	case o := <-h.out:
		h.t.Fatalf("unexpected message: %s", o.m)
	case <-time.After(50 * time.Millisecond):
	}
}

func get(m *quickfix.Message, t quickfix.Tag) string {
	v, err := m.Body.GetString(t)
	if err != nil {
		v, _ = m.Header.GetString(t)
	}
	return v
}

// expect checks tag=value pairs on the next message.
func (h *harness) expect(pairs ...any) *quickfix.Message {
	h.t.Helper()
	o := h.next()
	for i := 0; i < len(pairs); i += 2 {
		tg, want := pairs[i].(quickfix.Tag), pairs[i+1].(string)
		if got := get(o.m, tg); got != want {
			h.t.Fatalf("tag %d = %q, want %q in %s", tg, got, want, o.m)
		}
	}
	return o.m
}

func nos(clOrdID string, side enum.Side, qty string, ordType enum.OrdType) newordersingle.NewOrderSingle {
	m := newordersingle.New(field.NewClOrdID(clOrdID), field.NewSide(side), field.NewTransactTime(t0), field.NewOrdType(ordType))
	m.SetSymbol("AAPL")
	m.SetOrderQty(decimal.RequireFromString(qty), 0)
	return m
}

func cxl(orig, clOrdID string) ordercancelrequest.OrderCancelRequest {
	m := ordercancelrequest.New(field.NewOrigClOrdID(orig), field.NewClOrdID(clOrdID), field.NewSide(enum.Side_BUY), field.NewTransactTime(t0))
	m.SetSymbol("AAPL")
	return m
}

func rpl(orig, clOrdID, qty string, ordType enum.OrdType) ordercancelreplacerequest.OrderCancelReplaceRequest {
	m := ordercancelreplacerequest.New(field.NewOrigClOrdID(orig), field.NewClOrdID(clOrdID), field.NewSide(enum.Side_BUY),
		field.NewTransactTime(t0), field.NewOrdType(ordType))
	m.SetSymbol("AAPL")
	m.SetOrderQty(decimal.RequireFromString(qty), 0)
	return m
}

const (
	ExecType  = tag.ExecType
	OrdStatus = tag.OrdStatus
	CumQty    = tag.CumQty
	LeavesQty = tag.LeavesQty
	LastQty   = tag.LastQty
	LastPx    = tag.LastPx
	AvgPx     = tag.AvgPx
	MsgType   = tag.MsgType
)

func TestDefaultChainAckThenFill(t *testing.T) {
	h := newHarness(t, "default_action: [ { type: ack, delay_ms: 20 }, { type: fill, delay_ms: 100 } ]\n", aaplQuotes(nil), false)
	h.handle(nos("C1", enum.Side_BUY, "100", enum.OrdType_MARKET))
	h.none()
	h.advance(20 * time.Millisecond)
	h.expect(MsgType, "8", ExecType, "0", OrdStatus, "0", LeavesQty, "100", tag.ClOrdID, "C1")
	h.advance(99 * time.Millisecond)
	h.none()
	h.advance(time.Millisecond)
	h.expect(ExecType, "F", OrdStatus, "2", LastQty, "100", LastPx, "230.01", CumQty, "100", LeavesQty, "0", AvgPx, "230.01")

	h.handle(nos("C2", enum.Side_SELL, "5", enum.OrdType_MARKET))
	h.advance(120 * time.Millisecond)
	h.expect(ExecType, "0")
	h.expect(ExecType, "F", LastPx, "229.99")
}

func TestPartialFillsAndInvariants(t *testing.T) {
	h := newHarness(t, `default_action:
  - { type: ack }
  - { type: partial_fill, ratio: 0.5, count: 3, delay_ms: 10 }
  - { type: fill, delay_ms: 10 }
`, aaplQuotes(nil), false)
	h.handle(nos("C1", enum.Side_BUY, "100", enum.OrdType_MARKET))
	h.expect(ExecType, "0")
	h.advance(40 * time.Millisecond)
	h.expect(ExecType, "F", OrdStatus, "1", LastQty, "16", CumQty, "16", LeavesQty, "84")
	h.expect(LastQty, "16", CumQty, "32")
	h.expect(LastQty, "18", CumQty, "50", LeavesQty, "50")
	h.expect(OrdStatus, "2", LastQty, "50", CumQty, "100", LeavesQty, "0")

	o := h.e.Orders(client1)[0]
	sum := decimal.Zero
	for _, f := range o.Fills {
		sum = sum.Add(f.Qty)
	}
	if !sum.Equal(o.CumQty) || !o.CumQty.Add(o.LeavesQty).Equal(o.OrderQty) {
		t.Fatalf("invariants broken: %+v", o)
	}
}

func TestChecksBeforeRules(t *testing.T) {
	h := newHarness(t, "", aaplQuotes(nil), false)
	h.handle(nos("L1", enum.Side_BUY, "10", enum.OrdType_LIMIT))
	h.expect(ExecType, "8", OrdStatus, "8", tag.OrdRejReason, "11")

	unknown := nos("U1", enum.Side_BUY, "10", enum.OrdType_MARKET)
	unknown.SetSymbol("NOPE")
	h.handle(unknown)
	h.expect(ExecType, "8", tag.OrdRejReason, "1")

	h.handle(nos("C1", enum.Side_BUY, "10", enum.OrdType_MARKET))
	h.expect(ExecType, "0")
	h.expect(ExecType, "F", tag.ClOrdID, "C1")
	h.handle(nos("C1", enum.Side_BUY, "10", enum.OrdType_MARKET))
	h.expect(ExecType, "8", tag.OrdRejReason, "6", tag.OrderID, "NONE")
	if o := h.e.Orders(client1); len(o) != 3 {
		t.Fatalf("stored %d orders, want 3 (the duplicate is not stored)", len(o))
	}
}

func TestRuleActions(t *testing.T) {
	h := newHarness(t, `order_rules:
  - { name: big, priority: 1, match: { qty: { gt: 1000 } }, actions: [ { type: reject, reason: 3, text: "Order exceeds limit" } ] }
  - { name: timeout, priority: 2, match: { cl_ord_id: { regex: "^TIMEOUT-" } }, actions: [ { type: ignore } ] }
  - { name: bmr, priority: 3, match: { cl_ord_id: BMR }, actions: [ { type: business_reject, reason: 4, text: "No" } ] }
  - { name: exp, priority: 4, match: { cl_ord_id: EXP }, actions: [ { type: ack }, { type: expire, delay_ms: 5 } ] }
`, aaplQuotes(nil), false)
	h.handle(nos("B1", enum.Side_BUY, "5000", enum.OrdType_MARKET))
	h.expect(ExecType, "8", tag.OrdRejReason, "3", tag.Text, "Order exceeds limit")

	h.handle(nos("TIMEOUT-1", enum.Side_BUY, "5", enum.OrdType_MARKET))
	h.advance(time.Hour)
	h.none()

	h.handle(nos("BMR", enum.Side_BUY, "5", enum.OrdType_MARKET))
	h.expect(MsgType, "j", tag.RefMsgType, "D", tag.BusinessRejectReason, "4", tag.BusinessRejectRefID, "BMR")
	h.handle(nos("BMR", enum.Side_BUY, "5", enum.OrdType_MARKET)) // never accepted, so not a duplicate
	h.expect(MsgType, "j")

	h.handle(nos("EXP", enum.Side_BUY, "5", enum.OrdType_MARKET))
	h.expect(ExecType, "0")
	h.advance(5 * time.Millisecond)
	h.expect(ExecType, "C", OrdStatus, "C", LeavesQty, "0")
}

func TestCancelMidChain(t *testing.T) {
	h := newHarness(t, "default_action: [ { type: ack }, { type: fill, delay_ms: 1000 } ]\n", aaplQuotes(nil), false)
	h.handle(nos("C1", enum.Side_BUY, "100", enum.OrdType_MARKET))
	h.expect(ExecType, "0")
	h.handle(cxl("C1", "C2"))
	h.expect(ExecType, "4", OrdStatus, "4", tag.ClOrdID, "C2", tag.OrigClOrdID, "C1", LeavesQty, "0")
	h.advance(2 * time.Second)
	h.none() // the pending fill was dropped

	h.handle(cxl("C2", "C3")) // closed
	h.expect(MsgType, "9", tag.CxlRejReason, "1", tag.CxlRejResponseTo, "1", OrdStatus, "4")
	h.handle(cxl("NOPE", "C4"))
	h.expect(MsgType, "9", tag.CxlRejReason, "1", tag.OrderID, "NONE")
}

func TestCancelAndReplaceRules(t *testing.T) {
	h := newHarness(t, `order_rules:
  - { name: no-cancel, priority: 1, message_type: F, match: { cl_ord_id: NOCXL }, actions: [ { type: reject, reason: 0, text: "Too late" } ] }
  - { name: slow-cancel, priority: 2, message_type: F, actions: [ { type: ack, delay_ms: 500 } ] }
  - { name: slow-replace, priority: 3, message_type: G, actions: [ { type: ack, delay_ms: 200 } ] }
default_action: [ { type: ack } ]
`, aaplQuotes(nil), false)
	h.handle(nos("C1", enum.Side_BUY, "100", enum.OrdType_MARKET))
	h.expect(ExecType, "0")

	h.handle(cxl("C1", "NOCXL"))
	h.expect(MsgType, "9", tag.CxlRejReason, "0", tag.Text, "Too late")

	h.handle(rpl("C1", "C2", "300", enum.OrdType_MARKET))
	h.none()
	h.advance(200 * time.Millisecond)
	h.expect(ExecType, "5", tag.ClOrdID, "C2", tag.OrderQty, "300", LeavesQty, "300")

	h.handle(cxl("C2", "C3"))
	h.advance(499 * time.Millisecond)
	h.none()
	h.advance(time.Millisecond)
	h.expect(ExecType, "4", tag.ClOrdID, "C3")
}

func TestReplaceMidChain(t *testing.T) {
	h := newHarness(t, `default_action:
  - { type: ack }
  - { type: partial_fill, qty: 40, delay_ms: 10 }
  - { type: fill, delay_ms: 10 }
`, aaplQuotes(nil), false)
	h.handle(nos("C1", enum.Side_BUY, "100", enum.OrdType_MARKET))
	h.expect(ExecType, "0")
	h.advance(10 * time.Millisecond)
	h.expect(LastQty, "40", CumQty, "40")

	h.handle(rpl("C1", "C2", "40", enum.OrdType_MARKET))
	h.expect(MsgType, "9", tag.CxlRejReason, "2", tag.CxlRejResponseTo, "2")
	h.handle(rpl("C1", "C2", "200", enum.OrdType_LIMIT))
	h.expect(MsgType, "9", tag.CxlRejReason, "2")

	h.handle(rpl("C1", "C2", "200", enum.OrdType_MARKET))
	h.expect(ExecType, "5", OrdStatus, "1", tag.OrderQty, "200", LeavesQty, "160", CumQty, "40")
	h.advance(10 * time.Millisecond)
	h.expect(ExecType, "F", LastQty, "160", CumQty, "200", OrdStatus, "2", tag.ClOrdID, "C2")
}

func TestOrderStatus(t *testing.T) {
	h := newHarness(t, "default_action: [ { type: ack } ]\n", aaplQuotes(nil), false)
	h.handle(nos("C1", enum.Side_BUY, "100", enum.OrdType_MARKET))
	h.expect(ExecType, "0")
	st := orderstatusrequest.New(field.NewClOrdID("C1"), field.NewSide(enum.Side_BUY))
	st.SetSymbol("AAPL")
	h.handle(st)
	h.expect(ExecType, "I", OrdStatus, "0", LeavesQty, "100", tag.OrderID, "ORD-1")

	st = orderstatusrequest.New(field.NewClOrdID("NOPE"), field.NewSide(enum.Side_BUY))
	st.SetSymbol("AAPL")
	h.handle(st)
	h.expect(ExecType, "I", OrdStatus, "8", tag.Text, "Unknown order")
}

func TestSlippage(t *testing.T) {
	tests := []struct {
		name string
		sl   *config.Slippage
		side enum.Side
		want string
	}{
		{"fixed buy", &config.Slippage{Type: "fixed", Ticks: 2}, enum.Side_BUY, "230.03"},
		{"fixed sell", &config.Slippage{Type: "fixed", Ticks: 2}, enum.Side_SELL, "229.97"},
		{"percent buy", &config.Slippage{Type: "percent", Value: 0.05}, enum.Side_BUY, "230.13"},
		{"percent sell", &config.Slippage{Type: "percent", Value: 0.05}, enum.Side_SELL, "229.87"},
		{"fixed price ignores slippage", &config.Slippage{Type: "fixed", Ticks: 5}, enum.Side_BUY, "1.5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules := "default_action: [ { type: fill } ]\n"
			if strings.Contains(tt.name, "fixed price") {
				rules = "default_action: [ { type: fill, price_mode: fixed, price: 1.5 } ]\n"
			}
			h := newHarness(t, rules, aaplQuotes(tt.sl), false)
			h.handle(nos("C1", tt.side, "10", enum.OrdType_MARKET))
			h.expect(LastPx, tt.want)
		})
	}

	// Random slippage stays within range and repeats with the same seed.
	run := func() []string {
		h := newHarness(t, "default_action: [ { type: fill } ]\n", aaplQuotes(&config.Slippage{Type: "random", MinTicks: 1, MaxTicks: 3}), false)
		var pxs []string
		for _, id := range []string{"A", "B", "C", "D", "E", "F", "G", "H"} {
			h.handle(nos(id, enum.Side_BUY, "1", enum.OrdType_MARKET))
			px := get(h.next().m, LastPx)
			if px < "230.02" || px > "230.04" {
				t.Fatalf("random slippage px %s outside [230.02, 230.04]", px)
			}
			pxs = append(pxs, px)
		}
		return pxs
	}
	if a, b := run(), run(); strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatalf("random slippage not reproducible: %v vs %v", a, b)
	}
}

func TestDropCopy(t *testing.T) {
	h := newHarness(t, "default_action: [ { type: ack } ]\n", aaplQuotes(nil), true)
	h.handle(nos("C1", enum.Side_BUY, "100", enum.OrdType_MARKET))
	got := map[string]out{}
	for range 2 {
		o := h.next()
		got[o.session.TargetCompID] = o
	}
	cp, ok := got["UI_MONITOR"]
	if !ok {
		t.Fatal("no drop copy")
	}
	if v := get(cp.m, tag.MirrorSessionID); v != client1.String() {
		t.Fatalf("5001 = %q, want %q", v, client1.String())
	}
	if get(cp.m, tag.ExecID) != get(got["CLIENT1"].m, tag.ExecID) {
		t.Fatal("drop copy differs from the client report")
	}
	if get(got["CLIENT1"].m, tag.MirrorSessionID) != "" {
		t.Fatal("client report carries 5001")
	}
}

func TestRulesSwapKeepsInFlightChains(t *testing.T) {
	h := newHarness(t, "default_action: [ { type: ack }, { type: fill, delay_ms: 100 } ]\n", aaplQuotes(nil), false)
	h.handle(nos("OLD", enum.Side_BUY, "1", enum.OrdType_MARKET))
	h.expect(ExecType, "0")

	cfg, err := config.ParseWith([]byte(strings.Replace(engineCfg, "%v", "false", 1)+"default_action: [ { type: reject, text: new } ]\n"), Validate)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.e.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	h.handle(nos("NEW", enum.Side_BUY, "1", enum.OrdType_MARKET))
	h.expect(tag.ClOrdID, "NEW", ExecType, "8", tag.Text, "new")
	h.advance(100 * time.Millisecond)
	h.expect(tag.ClOrdID, "OLD", ExecType, "F")
}

func TestBookRetention(t *testing.T) {
	b := newBook(2)
	for _, id := range []string{"1", "2", "3"} {
		o := &Order{OrderID: id, Session: client1, ClOrdID: "C" + id}
		b.add(o)
		b.markClosed(o)
	}
	if b.orders["1"] != nil || b.byClOrd(client1, "C1") != nil || b.orders["3"] == nil {
		t.Fatalf("retention kept %v", b.orders)
	}
}
