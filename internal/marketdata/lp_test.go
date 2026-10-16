package marketdata

import (
	"bytes"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/fix/field"
	"github.com/quynv/mock-fixser/fix/fix44/marketdatarequest"
	"github.com/quynv/mock-fixser/fix/fix44/marketdatasnapshotfullrefresh"
	"github.com/quynv/mock-fixser/fix/fix44/quotecancel"
	"github.com/quynv/mock-fixser/internal/clock"
	"github.com/quynv/mock-fixser/internal/config"
	"log/slog"
)

// bidOnlyFullRefresh is the request of the postprime price-gateway: only
// MDEntryType 0, MDUpdateType 0 (full refresh), MarketDepth 0.
func bidOnlyFullRefresh(id, symbol string) marketdatarequest.MarketDataRequest {
	m := marketdatarequest.New(field.NewMDReqID(id), field.NewSubscriptionRequestType(enum.SubscriptionRequestType_SNAPSHOTUPDATE), field.NewMarketDepth(0))
	m.SetMDUpdateType(enum.MDUpdateType_FULL)
	types := marketdatarequest.NewNoMDEntryTypesRepeatingGroup()
	types.Add().SetMDEntryType(enum.MDEntryType_BID)
	m.SetNoMDEntryTypes(types)
	syms := marketdatarequest.NewNoRelatedSymRepeatingGroup()
	syms.Add().SetSymbol(symbol)
	m.SetNoRelatedSym(syms)
	m.Header.SetField(8, quickfix.FIXString("FIX.4.4"))
	parsed := quickfix.NewMessage()
	if err := quickfix.ParseMessage(parsed, bytes.NewBufferString(m.ToMessage().String())); err != nil {
		panic(err)
	}
	return marketdatarequest.FromMessage(parsed)
}

// snapshotEntries returns MDEntryType:QuoteCondition per W entry.
func snapshotEntries(t *testing.T, m sentMsg) []string {
	t.Helper()
	if m.msgType != "W" {
		t.Fatalf("got %s, want W", m.msgType)
	}
	g, err := marketdatasnapshotfullrefresh.FromMessage(m.msg).GetNoMDEntries()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := range g.Len() {
		e := g.Get(i)
		typ, _ := e.GetMDEntryType()
		qc, _ := e.GetQuoteCondition()
		out = append(out, string(typ)+":"+string(qc))
	}
	return out
}

func TestSendBothSidesAndQuoteCondition(t *testing.T) {
	q := quote("XAUUSD", 2400, 1)
	q.QuoteCondition = "A"
	src := fakeSource{"XAUUSD": q}

	// Default: only what the request lists.
	p, c := newPub(t, src, noBatching)
	handle(t, p, bidOnlyFullRefresh("r1", "XAUUSD"))
	if e := snapshotEntries(t, c.next(t)); len(e) != 2 || e[0] != "0:A" || e[1] != "0:A" {
		t.Fatalf("default entries %v, want bids only", e)
	}

	// send_both_sides: bid and offer, each with 276=A, also on every tick (W).
	cfg := testCfg(noBatching)
	cfg.MarketData.SendBothSides = true
	c2 := newCapture()
	p2 := NewPublisher(src, cfg, clock.Real{}, slog.New(slog.DiscardHandler), c2.send)
	defer p2.Close()
	handle(t, p2, bidOnlyFullRefresh("r1", "XAUUSD"))
	if e := snapshotEntries(t, c2.next(t)); len(e) != 4 || e[0] != "0:A" || e[2] != "1:A" {
		t.Fatalf("send_both_sides entries %v", e)
	}
	p2.OnQuote(q)
	if e := snapshotEntries(t, c2.next(t)); len(e) != 4 {
		t.Fatalf("tick entries %v", e)
	}

	// A session can turn it off again.
	off := false
	cfg.Server.FIX.Sessions = []config.Session{{SenderCompID: "MOCKSRV", TargetCompID: "CLIENT1",
		MarketData: &config.SessionMarketData{SendBothSides: &off}}}
	p2.SetConfig(cfg)
	handle(t, p2, bidOnlyFullRefresh("r2", "XAUUSD"))
	for {
		m := c2.next(t)
		if id, _ := m.msg.Body.GetString(262); id == "r2" {
			if e := snapshotEntries(t, m); len(e) != 2 {
				t.Fatalf("session override entries %v", e)
			}
			break
		}
	}
}

func TestQuoteCancel(t *testing.T) {
	p, c := newPub(t, threeSymbols(), noBatching)
	handle(t, p, request("ab", enum.SubscriptionRequestType_SNAPSHOTUPDATE, 1, "A", "B"))
	handle(t, p, request("c", enum.SubscriptionRequestType_SNAPSHOTUPDATE, 1, "C"))
	for range 3 {
		c.next(t)
	}

	// One symbol: Z with type 1 and the symbol; A stops, B keeps ticking.
	res := p.QuoteCancel([]string{"A"}, nil)
	if len(res.Sessions) != 1 || res.Subscriptions != 0 {
		t.Fatalf("result %+v (the ab subscription keeps B)", res)
	}
	z := c.next(t)
	qc := quotecancel.FromMessage(z.msg)
	typ, _ := qc.GetQuoteCancelType()
	entries, _ := qc.GetNoQuoteEntries()
	if z.msgType != "Z" || typ != enum.QuoteCancelType_CXLSYM || entries.Len() != 1 {
		t.Fatalf("Z %s type %s entries %d", z.msgType, typ, entries.Len())
	}
	if sym, _ := entries.Get(0).GetSymbol(); sym != "A" {
		t.Fatalf("Z symbol %s", sym)
	}
	p.OnQuote(quote("A", 10.5, 2))
	p.OnQuote(quote("B", 20.5, 2))
	if id, e := incEntries(t, c.next(t)); id != "ab" || e[0][:2] != "B:" {
		t.Fatalf("after cancel got %s %v, want B only", id, e)
	}
	c.none(t, 50*time.Millisecond)

	// All quotes: type 4, every subscription gone.
	res = p.QuoteCancel(nil, nil)
	if res.Subscriptions != 2 {
		t.Fatalf("cancel all: %+v", res)
	}
	z = c.next(t)
	if typ, _ := quotecancel.FromMessage(z.msg).GetQuoteCancelType(); typ != enum.QuoteCancelType_CXLALL {
		t.Fatalf("type %s, want 4", typ)
	}
	p.OnQuote(quote("C", 30.5, 3))
	c.none(t, 50*time.Millisecond)

	// Nothing subscribed: nothing sent.
	if res := p.QuoteCancel(nil, nil); len(res.Sessions) != 0 {
		t.Fatalf("empty cancel %+v", res)
	}
	c.none(t, 50*time.Millisecond)
	// Subscribing again works.
	handle(t, p, request("c", enum.SubscriptionRequestType_SNAPSHOTUPDATE, 1, "C"))
	if m := c.next(t); m.msgType != "W" {
		t.Fatalf("resubscribe got %s", m.msgType)
	}
}
