package marketdata

import (
	"bytes"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/fix/field"
	"github.com/quynv/mock-fixser/fix/fix44/marketdataincrementalrefresh"
	"github.com/quynv/mock-fixser/fix/fix44/marketdatarequest"
	"github.com/quynv/mock-fixser/fix/fix44/marketdatarequestreject"
	"github.com/quynv/mock-fixser/fix/fix44/marketdatasnapshotfullrefresh"
	"github.com/quynv/mock-fixser/internal/clock"
	"github.com/quynv/mock-fixser/internal/config"
)

var sess = quickfix.SessionID{BeginString: "FIX.4.4", SenderCompID: "MOCKSRV", TargetCompID: "CLIENT1"}

type fakeSource map[string]Quote

func (f fakeSource) Last(s string) (Quote, config.Symbol, bool) {
	q, ok := f[s]
	return q, config.Symbol{}, ok
}

func quote(symbol string, bid float64, seq uint64) Quote {
	return Quote{
		Symbol: symbol, Bid: bid, Ask: bid + 0.02, Seq: seq, Decimals: 2,
		Bids: []Level{{bid, 100}, {bid - 0.01, 200}},
		Asks: []Level{{bid + 0.02, 100}, {bid + 0.03, 200}},
	}
}

type sentMsg struct {
	msgType string
	msg     *quickfix.Message
}

// capture records sent messages, optionally blocking on gate first (and
// saying so on blocked).
type capture struct {
	out     chan sentMsg
	gate    chan struct{}
	blocked chan struct{}
}

func newCapture() *capture { return &capture{out: make(chan sentMsg, 1000)} }

func (c *capture) send(m *quickfix.Message, _ quickfix.SessionID) error {
	if c.gate != nil {
		if c.blocked != nil {
			select {
			case c.blocked <- struct{}{}:
			default:
			}
		}
		<-c.gate
	}
	// Round-trip through the wire format, as a client would see it.
	m.Header.SetField(8, quickfix.FIXString("FIX.4.4"))
	parsed := quickfix.NewMessage()
	if err := quickfix.ParseMessage(parsed, bytes.NewBufferString(m.String())); err != nil {
		return err
	}
	mt, _ := parsed.MsgType()
	c.out <- sentMsg{mt, parsed}
	return nil
}

func (c *capture) next(t *testing.T) sentMsg {
	t.Helper()
	select {
	case m := <-c.out:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("no message sent")
	}
	return sentMsg{}
}

func (c *capture) none(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case m := <-c.out:
		t.Fatalf("unexpected %s: %s", m.msgType, m.msg)
	case <-time.After(wait):
	}
}

func testCfg(b config.Batching) *config.Config {
	return &config.Config{MarketData: config.MarketData{Batching: b}}
}

var noBatching = config.Batching{MaxSymbolsPerMsg: 20, MaxEntriesPerMsg: 200, FlushIntervalMs: 50, Scope: "per_request"}

func newPub(t *testing.T, src fakeSource, b config.Batching) (*Publisher, *capture) {
	t.Helper()
	c := newCapture()
	p := NewPublisher(src, testCfg(b), clock.Real{}, slog.New(slog.DiscardHandler), c.send)
	t.Cleanup(p.Close)
	return p, c
}

func request(id string, typ enum.SubscriptionRequestType, depth int, symbols ...string) marketdatarequest.MarketDataRequest {
	m := marketdatarequest.New(field.NewMDReqID(id), field.NewSubscriptionRequestType(typ), field.NewMarketDepth(depth))
	types := marketdatarequest.NewNoMDEntryTypesRepeatingGroup()
	types.Add().SetMDEntryType(enum.MDEntryType_BID)
	types.Add().SetMDEntryType(enum.MDEntryType_OFFER)
	m.SetNoMDEntryTypes(types)
	syms := marketdatarequest.NewNoRelatedSymRepeatingGroup()
	for _, s := range symbols {
		syms.Add().SetSymbol(s)
	}
	m.SetNoRelatedSym(syms)
	// Parse it back so the handler reads it like a received message.
	m.Header.SetField(8, quickfix.FIXString("FIX.4.4"))
	parsed := quickfix.NewMessage()
	if err := quickfix.ParseMessage(parsed, bytes.NewBufferString(m.ToMessage().String())); err != nil {
		panic(err)
	}
	return marketdatarequest.FromMessage(parsed)
}

func handle(t *testing.T, p *Publisher, m marketdatarequest.MarketDataRequest) {
	t.Helper()
	if err := p.HandleRequest(m, sess); err != nil {
		t.Fatalf("HandleRequest: %v", err)
	}
}

// incEntries returns "symbol:type:pos:px" for each X entry.
func incEntries(t *testing.T, m sentMsg) (mdReqID string, entries []string) {
	t.Helper()
	if m.msgType != "X" {
		t.Fatalf("got %s, want X", m.msgType)
	}
	x := marketdataincrementalrefresh.FromMessage(m.msg)
	mdReqID, _ = x.GetMDReqID()
	g, err := x.GetNoMDEntries()
	if err != nil {
		t.Fatal(err)
	}
	for i := range g.Len() {
		e := g.Get(i)
		sym, _ := e.GetSymbol()
		typ, _ := e.GetMDEntryType()
		pos, _ := e.GetMDEntryPositionNo()
		px, _ := e.GetMDEntryPx()
		act, _ := e.GetMDUpdateAction()
		entries = append(entries, fmt.Sprintf("%s:%s:%d:%s:%s", sym, typ, pos, px.StringFixed(2), act))
	}
	return mdReqID, entries
}

func TestSubscribeSendsSnapshotThenIncrementals(t *testing.T) {
	src := fakeSource{"AAPL": quote("AAPL", 230, 1)}
	p, c := newPub(t, src, noBatching)
	handle(t, p, request("r1", enum.SubscriptionRequestType_SNAPSHOTUPDATE, 0, "AAPL"))

	w := c.next(t)
	if w.msgType != "W" {
		t.Fatalf("first message %s, want W", w.msgType)
	}
	snap := marketdatasnapshotfullrefresh.FromMessage(w.msg)
	if id, _ := snap.GetMDReqID(); id != "r1" {
		t.Errorf("W MDReqID = %q", id)
	}
	if g, _ := snap.GetNoMDEntries(); g.Len() != 4 {
		t.Errorf("W entries = %d, want 4 (2 levels x bid/offer)", g.Len())
	}

	p.OnQuote(quote("AAPL", 230.05, 2))
	id, entries := incEntries(t, c.next(t))
	want := []string{"AAPL:0:1:230.05:1", "AAPL:0:2:230.04:1", "AAPL:1:1:230.07:1", "AAPL:1:2:230.08:1"}
	if id != "r1" || fmt.Sprint(entries) != fmt.Sprint(want) {
		t.Fatalf("X %s %v, want r1 %v", id, entries, want)
	}

	// Unsubscribe: no response, no more ticks.
	handle(t, p, request("r1", enum.SubscriptionRequestType_UNSUBSCRIBE, 0, "AAPL"))
	p.OnQuote(quote("AAPL", 230.10, 3))
	c.none(t, 100*time.Millisecond)
}

func TestTopOfBookDepth(t *testing.T) {
	p, c := newPub(t, fakeSource{"AAPL": quote("AAPL", 230, 1)}, noBatching)
	handle(t, p, request("r1", enum.SubscriptionRequestType_SNAPSHOTUPDATE, 1, "AAPL"))
	c.next(t)
	p.OnQuote(quote("AAPL", 230.05, 2))
	if _, e := incEntries(t, c.next(t)); len(e) != 2 {
		t.Fatalf("MarketDepth=1 entries = %v, want bid and offer only", e)
	}
}

func TestSnapshotOnlyDoesNotRegister(t *testing.T) {
	p, c := newPub(t, fakeSource{"AAPL": quote("AAPL", 230, 1)}, noBatching)
	handle(t, p, request("s1", enum.SubscriptionRequestType_SNAPSHOT, 0, "AAPL"))
	if m := c.next(t); m.msgType != "W" {
		t.Fatalf("got %s, want W", m.msgType)
	}
	p.OnQuote(quote("AAPL", 230.05, 2))
	c.none(t, 100*time.Millisecond)
}

func expectReject(t *testing.T, c *capture, reason enum.MDReqRejReason) {
	t.Helper()
	m := c.next(t)
	if m.msgType != "Y" {
		t.Fatalf("got %s, want Y", m.msgType)
	}
	if r, _ := marketdatarequestreject.FromMessage(m.msg).GetMDReqRejReason(); r != reason {
		t.Fatalf("MDReqRejReason = %s, want %s", r, reason)
	}
}

func TestRejects(t *testing.T) {
	src := fakeSource{"AAPL": quote("AAPL", 230, 1)}
	p, c := newPub(t, src, noBatching)

	// Unknown symbol rejects the whole request; AAPL is not registered either.
	handle(t, p, request("r1", enum.SubscriptionRequestType_SNAPSHOTUPDATE, 0, "AAPL", "NOPE"))
	expectReject(t, c, enum.MDReqRejReason_UNKNOWNSYM)
	p.OnQuote(quote("AAPL", 230.05, 2))
	c.none(t, 100*time.Millisecond)

	handle(t, p, request("r2", enum.SubscriptionRequestType_SNAPSHOTUPDATE, 0, "AAPL"))
	c.next(t) // W
	handle(t, p, request("r2", enum.SubscriptionRequestType_SNAPSHOTUPDATE, 0, "AAPL"))
	expectReject(t, c, enum.MDReqRejReason_DUPID)
}

func TestLogoutCancelsSubscriptions(t *testing.T) {
	p, c := newPub(t, fakeSource{"AAPL": quote("AAPL", 230, 1)}, noBatching)
	handle(t, p, request("r1", enum.SubscriptionRequestType_SNAPSHOTUPDATE, 0, "AAPL"))
	c.next(t)
	p.OnLogout(sess)
	p.OnQuote(quote("AAPL", 230.05, 2))
	c.none(t, 100*time.Millisecond)

	// The same MDReqID can be used again after logon.
	handle(t, p, request("r1", enum.SubscriptionRequestType_SNAPSHOTUPDATE, 0, "AAPL"))
	if m := c.next(t); m.msgType != "W" {
		t.Fatalf("got %s, want W", m.msgType)
	}
}

func TestSlowSessionIsConflated(t *testing.T) {
	src := fakeSource{"AAPL": quote("AAPL", 230, 1)}
	p, c := newPub(t, src, noBatching)
	c.gate = make(chan struct{})
	c.blocked = make(chan struct{}, 1)
	handle(t, p, request("r1", enum.SubscriptionRequestType_SNAPSHOTUPDATE, 1, "AAPL"))
	<-c.blocked // the writer is stuck sending W

	// The writer is stuck sending W; OnQuote must not block meanwhile.
	done := make(chan struct{})
	go func() {
		for i := range 100 {
			p.OnQuote(quote("AAPL", 230+float64(i)/100, uint64(i+2)))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("OnQuote blocked on a slow session")
	}
	close(c.gate)

	if m := c.next(t); m.msgType != "W" {
		t.Fatalf("got %s, want W", m.msgType)
	}
	_, e := incEntries(t, c.next(t))
	if e[0] != "AAPL:0:1:230.99:1" {
		t.Fatalf("after conflation got %v, want only the latest tick (230.99)", e)
	}
	c.none(t, 100*time.Millisecond)
	if n := p.Conflated(sess); n != 99 {
		t.Fatalf("conflated = %d, want 99", n)
	}
}

func batching(scope string) config.Batching {
	return config.Batching{Enabled: true, Scope: scope, FlushIntervalMs: 50, MaxSymbolsPerMsg: 20,
		MaxEntriesPerMsg: 200, Conflate: true, InitialSnapshot: "per_symbol_w"}
}

func threeSymbols() fakeSource {
	return fakeSource{"A": quote("A", 10, 1), "B": quote("B", 20, 1), "C": quote("C", 30, 1)}
}

func TestBatchingPerRequest(t *testing.T) {
	p, c := newPub(t, threeSymbols(), batching("per_request"))
	handle(t, p, request("ab", enum.SubscriptionRequestType_SNAPSHOTUPDATE, 1, "A", "B"))
	handle(t, p, request("c", enum.SubscriptionRequestType_SNAPSHOTUPDATE, 1, "C"))
	for range 3 {
		if m := c.next(t); m.msgType != "W" {
			t.Fatalf("initial %s, want W per symbol", m.msgType)
		}
	}
	p.OnQuote(quote("A", 10.01, 2))
	p.OnQuote(quote("C", 30.01, 2))
	p.OnQuote(quote("B", 20.01, 2))

	id, e := incEntries(t, c.next(t))
	if id != "ab" || len(e) != 4 || e[0][:2] != "A:" || e[2][:2] != "B:" {
		t.Fatalf("first X %s %v, want ab with A and B", id, e)
	}
	id, e = incEntries(t, c.next(t))
	if id != "c" || len(e) != 2 {
		t.Fatalf("second X %s %v, want c with C", id, e)
	}
}

func TestBatchingPerSessionAndLimits(t *testing.T) {
	b := batching("per_session")
	b.MaxSymbolsPerMsg = 2
	p, c := newPub(t, threeSymbols(), b)
	handle(t, p, request("ab", enum.SubscriptionRequestType_SNAPSHOTUPDATE, 1, "A", "B"))
	handle(t, p, request("c", enum.SubscriptionRequestType_SNAPSHOTUPDATE, 1, "C"))
	for range 3 {
		c.next(t)
	}
	p.OnQuote(quote("A", 10.01, 2))
	p.OnQuote(quote("B", 20.01, 2))
	p.OnQuote(quote("C", 30.01, 2))

	id, e := incEntries(t, c.next(t))
	if id != "" || len(e) != 4 {
		t.Fatalf("first X MDReqID=%q %v, want no 262 and 2 symbols", id, e)
	}
	_, e = incEntries(t, c.next(t))
	if len(e) != 2 || e[0][:2] != "C:" {
		t.Fatalf("second X %v, want C", e)
	}
}

func TestBatchingWithoutConflateKeepsEveryTick(t *testing.T) {
	b := batching("per_request")
	b.Conflate = false
	p, c := newPub(t, threeSymbols(), b)
	handle(t, p, request("a", enum.SubscriptionRequestType_SNAPSHOTUPDATE, 1, "A"))
	c.next(t)
	p.OnQuote(quote("A", 10.01, 2))
	p.OnQuote(quote("A", 10.02, 3))
	_, e := incEntries(t, c.next(t))
	want := []string{"A:0:1:10.01:1", "A:1:1:10.03:1", "A:0:1:10.02:1", "A:1:1:10.04:1"}
	if fmt.Sprint(e) != fmt.Sprint(want) {
		t.Fatalf("entries %v, want %v", e, want)
	}
}

func TestBatchedInitialSnapshot(t *testing.T) {
	b := batching("per_request")
	b.InitialSnapshot = "batched_x"
	p, c := newPub(t, threeSymbols(), b)
	handle(t, p, request("abc", enum.SubscriptionRequestType_SNAPSHOTUPDATE, 1, "A", "B", "C"))
	id, e := incEntries(t, c.next(t))
	if id != "abc" || len(e) != 6 || e[0] != "A:0:1:10.00:0" {
		t.Fatalf("initial X %s %v, want 3 symbols with MDUpdateAction=0", id, e)
	}
}

func TestSessionBatchingOverride(t *testing.T) {
	cfg := testCfg(batching("per_session"))
	off := false
	cfg.Server.FIX.Sessions = []config.Session{{SenderCompID: "MOCKSRV", TargetCompID: "CLIENT1",
		MarketData: &config.SessionMarketData{Batching: &config.BatchingOverride{Enabled: &off}}}}
	c := newCapture()
	p := NewPublisher(threeSymbols(), cfg, clock.Real{}, slog.New(slog.DiscardHandler), c.send)
	defer p.Close()
	if b := p.batchingFor(sess); b.Enabled || b.Scope != "per_session" {
		t.Fatalf("CLIENT1 batching = %+v, want disabled with global scope", b)
	}
	other := sess
	other.TargetCompID = "CLIENT2"
	if !p.batchingFor(other).Enabled {
		t.Fatal("CLIENT2 should use the global batching")
	}
}
