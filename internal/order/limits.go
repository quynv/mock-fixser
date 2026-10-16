package order

import (
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/internal/marketdata"
)

// Limit, stop and time-in-force handling (design doc, sections 7.1 and
// 7.4). A fill step of an order that cannot execute at the current market
// waits for a quote that lets it, unless the order is IOC or FOK.

const (
	ordMarket    = string(enum.OrdType_MARKET)
	ordLimit     = string(enum.OrdType_LIMIT)
	ordStop      = string(enum.OrdType_STOP)
	ordStopLimit = "4"

	tifDay = "0"
	tifGTC = "1"
	tifIOC = "3"
	tifFOK = "4"
)

// checkOrderLocked validates the order type, prices and time in force
// before any rule runs. It returns false after rejecting the order.
func (e *Engine) checkOrderLocked(o *Order) bool {
	switch o.OrdType {
	case ordMarket, ordLimit, ordStop, ordStopLimit:
	default:
		e.rejectNewLocked(o, enum.OrdRejReason_UNSUPPORDERCHAR, "OrdType must be 1 (Market), 2 (Limit), 3 (Stop) or 4 (Stop limit)")
		return false
	}
	switch o.TimeInForce {
	case "", tifDay, tifGTC, tifIOC, tifFOK:
	default:
		e.rejectNewLocked(o, enum.OrdRejReason_UNSUPPORDERCHAR, "TimeInForce must be 0 (Day), 1 (GTC), 3 (IOC) or 4 (FOK)")
		return false
	}
	if hasLimit(o.OrdType) && (o.Price == nil || !o.Price.IsPositive()) {
		e.rejectNewLocked(o, enum.OrdRejReason_OTHER, "Price is required for limit orders")
		return false
	}
	if hasStop(o.OrdType) && (o.StopPx == nil || !o.StopPx.IsPositive()) {
		e.rejectNewLocked(o, enum.OrdRejReason_OTHER, "StopPx is required for stop orders")
		return false
	}
	return true
}

func hasLimit(ordType string) bool { return ordType == ordLimit || ordType == ordStopLimit }
func hasStop(ordType string) bool  { return ordType == ordStop || ordType == ordStopLimit }

// waiter is a fill step waiting for the market.
type waiter struct {
	o     *Order
	gen   uint64
	st    Step
	qty   func() decimal.Decimal
	label string
}

// tryFillLocked runs a fill step: it executes now if the market allows,
// otherwise IOC cancels the rest, FOK cancels the order, and other orders
// wait for a quote that lets the step execute.
func (e *Engine) tryFillLocked(o *Order, gen uint64, st Step, qty func() decimal.Decimal) {
	if o.TimeInForce == tifFOK {
		qty = func() decimal.Decimal { return o.LeavesQty } // all or none
	}
	if e.fillNowLocked(o, st, qty()) {
		return
	}
	switch o.TimeInForce {
	case tifIOC, tifFOK:
		e.cancelRestLocked(o, fmt.Sprintf("%s: not executable at the market", tifName(o.TimeInForce)))
		return
	}
	e.waiting[o.Symbol] = append(e.waiting[o.Symbol], waiter{o: o, gen: gen, st: st, qty: qty})
	e.waiters.Add(1)
}

// fillNowLocked fills qty if the order can execute against the current
// quote; it returns false when the order has to wait.
func (e *Engine) fillNowLocked(o *Order, st Step, qty decimal.Decimal) bool {
	qty = decimal.Min(qty, o.LeavesQty)
	if !qty.IsPositive() {
		return true // nothing left to do
	}
	px, state := e.executionPrice(o, st)
	switch state {
	case priceNone:
		o.close(enum.OrdStatus_CANCELED)
		e.book.markClosed(o)
		e.reportLocked(o, enum.ExecType_CANCELED, func(r *erFields) { r.text = "No market price for " + o.Symbol })
		return true
	case priceWait:
		return false
	}
	e.execFillLocked(o, qty, px)
	return true
}

type priceState int

const (
	priceOK priceState = iota
	priceWait
	priceNone
)

// executionPrice is the fill price for a step, or priceWait while a stop
// is not triggered or a limit is not marketable (section 7.4):
//
//   - fixed: the rule's price;
//   - limit_immediate: the order's limit price, no slippage (market
//     orders fall back to the market price);
//   - market and market_driven: the ask for buys or the bid for sells plus
//     slippage, never worse than the limit; a limit order waits until the
//     market reaches its limit.
func (e *Engine) executionPrice(o *Order, st Step) (decimal.Decimal, priceState) {
	if st.PriceMode == "fixed" {
		return st.Price, priceOK
	}
	q, cfg, ok := e.quotes.Last(o.Symbol)
	if !ok {
		return decimal.Decimal{}, priceNone
	}
	buy := isBuy(o.Side)
	mkt := decimal.NewFromFloat(q.Bid)
	if buy {
		mkt = decimal.NewFromFloat(q.Ask)
	}
	if hasStop(o.OrdType) && !o.Triggered {
		// A buy stop triggers when the ask reaches StopPx, a sell stop when
		// the bid does.
		if (buy && mkt.LessThan(*o.StopPx)) || (!buy && mkt.GreaterThan(*o.StopPx)) {
			return decimal.Decimal{}, priceWait
		}
		o.Triggered = true
	}
	limit := o.Price
	if !hasLimit(o.OrdType) {
		limit = nil
	}
	if st.PriceMode == "limit_immediate" && limit != nil {
		return *limit, priceOK
	}
	if limit != nil && ((buy && mkt.GreaterThan(*limit)) || (!buy && mkt.LessThan(*limit))) {
		return decimal.Decimal{}, priceWait
	}
	px := slip(mkt, o.Side, cfg.Slippage, decimal.NewFromFloat(cfg.TickSize), fillRand(cfg, o))
	if limit != nil {
		if buy {
			px = decimal.Min(px, *limit)
		} else {
			px = decimal.Max(px, *limit)
		}
	}
	return px, priceOK
}

// OnQuote retries the fill steps waiting on a symbol. The market data
// engine calls it for every tick.
func (e *Engine) OnQuote(q marketdata.Quote) {
	if e.waiters.Load() == 0 {
		return // the usual case: no lock per tick
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	ws := e.waiting[q.Symbol]
	if len(ws) == 0 {
		return
	}
	var keep []waiter
	for _, w := range ws {
		if w.o.chain != w.gen || w.o.Closed() {
			continue // canceled, replaced chain, or done
		}
		if !e.fillNowLocked(w.o, w.st, w.qty()) {
			keep = append(keep, w)
		}
	}
	e.waiters.Add(int64(len(keep) - len(ws)))
	if len(keep) == 0 {
		delete(e.waiting, q.Symbol)
	} else {
		e.waiting[q.Symbol] = keep
	}
}

// cancelRestLocked cancels what is left of an order (IOC, FOK).
func (e *Engine) cancelRestLocked(o *Order, text string) {
	if o.Closed() {
		return
	}
	o.close(enum.OrdStatus_CANCELED)
	e.book.markClosed(o)
	e.reportLocked(o, enum.ExecType_CANCELED, func(r *erFields) { r.text = text })
}

func tifName(tif string) string {
	switch tif {
	case tifIOC:
		return "IOC"
	case tifFOK:
		return "FOK"
	case tifGTC:
		return "GTC"
	}
	return "Day"
}
