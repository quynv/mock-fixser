package order

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/fix/fix44/newordersingle"
	"github.com/quynv/mock-fixser/fix/tag"
	"github.com/quynv/mock-fixser/internal/config"
	"github.com/quynv/mock-fixser/internal/marketdata"
)

// limitOrder: AAPL is quoted 229.99 / 230.01 by aaplQuotes.
func limitOrder(id string, side enum.Side, qty, price string, ordType enum.OrdType, stop, tif string) newordersingle.NewOrderSingle {
	m := nos(id, side, qty, ordType)
	if price != "" {
		m.SetPrice(decimal.RequireFromString(price), 2)
	}
	if stop != "" {
		m.SetStopPx(decimal.RequireFromString(stop), 2)
	}
	if tif != "" {
		m.SetTimeInForce(enum.TimeInForce(tif))
	}
	return m
}

// moveMarket changes the AAPL quote and lets the engine see the tick.
func (h *harness) moveMarket(q fakeQuotes, bid, ask float64) {
	v := q["AAPL"]
	v.q.Bid, v.q.Ask = bid, ask
	q["AAPL"] = v
	h.e.OnQuote(marketdata.Quote{Symbol: "AAPL", Bid: bid, Ask: ask})
}

func TestLimitMarketableFillsClampedToLimit(t *testing.T) {
	q := aaplQuotes(&config.Slippage{Type: "fixed", Ticks: 5})
	h := newHarness(t, "default_action: [ { type: fill } ]\n", q, false)
	// Marketable: ask 230.01 <= 230.03; ask + 5 ticks = 230.06 is clamped to the limit.
	h.handle(limitOrder("L1", enum.Side_BUY, "10", "230.03", enum.OrdType_LIMIT, "", ""))
	h.expect(ExecType, "F", LastPx, "230.03", tag.Price, "230.03")
	h.handle(limitOrder("L2", enum.Side_SELL, "10", "229.97", enum.OrdType_LIMIT, "", ""))
	h.expect(ExecType, "F", LastPx, "229.97") // 229.99 - 5 ticks = 229.94, clamped
}

func TestLimitWaitsForTheMarket(t *testing.T) {
	q := aaplQuotes(nil)
	h := newHarness(t, "default_action: [ { type: ack }, { type: fill } ]\n", q, false)
	h.handle(limitOrder("L1", enum.Side_BUY, "10", "229.50", enum.OrdType_LIMIT, "", ""))
	h.expect(ExecType, "0")
	h.none() // ask 230.01 is above the limit
	h.moveMarket(q, 229.70, 229.72)
	h.none()
	h.moveMarket(q, 229.47, 229.49)
	h.expect(ExecType, "F", LastPx, "229.49", OrdStatus, "2")
}

func TestLimitImmediateAndFixedModes(t *testing.T) {
	h := newHarness(t, `order_rules:
  - { name: at-limit, priority: 1, match: { ord_type: LIMIT, side: BUY }, actions: [ { type: fill, price_mode: limit_immediate } ] }
default_action: [ { type: fill } ]
`, aaplQuotes(nil), false)
	h.handle(limitOrder("L1", enum.Side_BUY, "10", "200", enum.OrdType_LIMIT, "", "")) // far from the market
	h.expect(ExecType, "F", LastPx, "200")
}

func TestStopOrders(t *testing.T) {
	q := aaplQuotes(nil)
	h := newHarness(t, "default_action: [ { type: ack }, { type: fill } ]\n", q, false)
	// Buy stop at 230.50: waits until the ask reaches it, then fills at the market.
	h.handle(limitOrder("S1", enum.Side_BUY, "10", "", enum.OrdType_STOP, "230.50", ""))
	h.expect(ExecType, "0", tag.StopPx, "230.5")
	h.moveMarket(q, 230.20, 230.22)
	h.none()
	h.moveMarket(q, 230.58, 230.60)
	h.expect(ExecType, "F", LastPx, "230.6")

	// Sell stop limit: triggers at bid <= 229.00, then needs bid >= 228.90.
	h.handle(limitOrder("SL1", enum.Side_SELL, "10", "228.90", enum.OrdType("4"), "229.00", ""))
	h.expect(ExecType, "0")
	h.moveMarket(q, 228.80, 228.82) // triggered, but below the limit
	h.none()
	h.moveMarket(q, 228.95, 228.97)
	h.expect(ExecType, "F", LastPx, "228.95")
}

func TestIOCAndFOK(t *testing.T) {
	q := aaplQuotes(nil)
	h := newHarness(t, `order_rules:
  - name: partial
    priority: 1
    match: { cl_ord_id: { regex: "^P" } }
    actions: [ { type: ack }, { type: partial_fill, ratio: 0.4 }, { type: fill, delay_ms: 100 } ]
default_action: [ { type: ack }, { type: fill } ]
`, q, false)
	// IOC, not marketable: canceled at once.
	h.handle(limitOrder("I1", enum.Side_BUY, "10", "229", enum.OrdType_LIMIT, "", "3"))
	h.expect(ExecType, "0")
	h.expect(ExecType, "4", tag.Text, "IOC: not executable at the market", tag.TimeInForce, "3")

	// IOC partial then the market moves away: the rest is canceled, not left waiting.
	h.handle(limitOrder("P1", enum.Side_BUY, "100", "230.01", enum.OrdType_LIMIT, "", "3"))
	h.expect(ExecType, "0")
	h.expect(ExecType, "F", CumQty, "40")
	h.moveMarket(q, 230.49, 230.51)
	h.advance(100 * time.Millisecond)
	h.expect(ExecType, "4", CumQty, "40", LeavesQty, "0")

	// FOK: a partial_fill rule becomes all or none.
	h.moveMarket(q, 229.99, 230.01)
	h.handle(limitOrder("P2", enum.Side_BUY, "100", "230.01", enum.OrdType_LIMIT, "", "4"))
	h.expect(ExecType, "0")
	h.expect(ExecType, "F", CumQty, "100", OrdStatus, "2")
	h.handle(limitOrder("F2", enum.Side_BUY, "100", "229", enum.OrdType_LIMIT, "", "4"))
	h.expect(ExecType, "0")
	h.expect(ExecType, "4", CumQty, "0")

	// Unsupported time in force.
	h.handle(limitOrder("G1", enum.Side_BUY, "1", "229", enum.OrdType_LIMIT, "", "6"))
	h.expect(ExecType, "8", tag.OrdRejReason, "11")
}

func TestReplaceLimitPrice(t *testing.T) {
	q := aaplQuotes(nil)
	h := newHarness(t, "default_action: [ { type: ack }, { type: fill } ]\n", q, false)
	h.handle(limitOrder("L1", enum.Side_BUY, "10", "229", enum.OrdType_LIMIT, "", ""))
	h.expect(ExecType, "0")
	h.none()

	r := rpl("L1", "L2", "10", enum.OrdType_LIMIT)
	r.SetPrice(decimal.RequireFromString("230.05"), 2)
	h.handle(r)
	h.expect(ExecType, "5", tag.Price, "230.05")
	h.moveMarket(q, 229.99, 230.01) // any tick retries the waiting fill
	h.expect(ExecType, "F", LastPx, "230.01", tag.ClOrdID, "L2")

	// The OrdType cannot change, and a limit replace needs a Price.
	h.handle(limitOrder("L3", enum.Side_BUY, "10", "200", enum.OrdType_LIMIT, "", ""))
	h.expect(ExecType, "0")
	h.handle(rpl("L3", "L4", "10", enum.OrdType_MARKET))
	h.expect(MsgType, "9", tag.Text, "OrdType cannot change")
	h.handle(rpl("L3", "L5", "10", enum.OrdType_LIMIT))
	h.expect(MsgType, "9", tag.Text, "Price is required for limit orders")
}

func TestRulesMatchOrdTypeAndTimeInForce(t *testing.T) {
	h := newHarness(t, `order_rules:
  - { name: limits, priority: 1, match: { ord_type: [LIMIT, STOP_LIMIT], time_in_force: GTC }, actions: [ { type: reject, text: gtc-limit } ] }
  - { name: day, priority: 2, match: { time_in_force: DAY }, actions: [ { type: reject, text: day } ] }
`, aaplQuotes(nil), false)
	h.handle(limitOrder("A", enum.Side_BUY, "1", "200", enum.OrdType_LIMIT, "", "1"))
	h.expect(tag.Text, "gtc-limit")
	h.handle(nos("B", enum.Side_BUY, "1", enum.OrdType_MARKET)) // no TimeInForce = Day
	h.expect(tag.Text, "day")
}
