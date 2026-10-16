package order

import (
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quickfixgo/quickfix"
	"github.com/shopspring/decimal"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/fix/tag"
	"github.com/quynv/mock-fixser/internal/config"
	"github.com/quynv/mock-fixser/internal/marketdata"
	"github.com/quynv/mock-fixser/internal/metrics"
	"github.com/quynv/mock-fixser/internal/sched"
)

// Quotes gives the current market for fills (the market data engine).
type Quotes interface {
	Last(symbol string) (marketdata.Quote, config.Symbol, bool)
}

// Engine processes D, F, G and H. Each new order is checked, matched
// against the rules once, and its action chain is put on the scheduler.
// Rules and the drop copy target can be swapped at runtime; orders in
// flight keep the chain they were given (section 9.3).
type Engine struct {
	log    *slog.Logger
	sched  *sched.Scheduler
	quotes Quotes
	send   Sender

	rules    atomic.Pointer[RuleSet]
	dropCopy atomic.Pointer[quickfix.SessionID]

	mu       sync.Mutex
	book     *book
	orderSeq uint64
	execSeq  uint64
	massSeq  uint64
	outboxes map[quickfix.SessionID]*outbox
}

func NewEngine(cfg *config.Config, quotes Quotes, s *sched.Scheduler, log *slog.Logger, send Sender) (*Engine, error) {
	if send == nil {
		send = func(m *quickfix.Message, id quickfix.SessionID) error { return quickfix.SendToTarget(m, id) }
	}
	e := &Engine{
		log: log, sched: s, quotes: quotes, send: send,
		book:     newBook(DefaultRetention),
		outboxes: map[quickfix.SessionID]*outbox{},
	}
	if err := e.SetConfig(cfg); err != nil {
		return nil, err
	}
	return e, nil
}

// SetConfig swaps in new rules and drop copy settings.
func (e *Engine) SetConfig(cfg *config.Config) error {
	rs, errs := Compile(cfg)
	if len(errs) > 0 {
		return &config.ValidationError{Errors: errs}
	}
	e.rules.Store(rs)

	mon := cfg.Server.Monitor
	if mon.Enabled && mon.DropCopy {
		id := quickfix.SessionID{BeginString: cfg.Server.FIX.Version, SenderCompID: mon.SenderCompID, TargetCompID: mon.TargetCompID}
		e.dropCopy.Store(&id)
	} else {
		e.dropCopy.Store(nil)
	}
	return nil
}

// Route registers the order handlers on a router. Each handler's time is
// observed in mockfix_order_latency_seconds.
func (e *Engine) Route(r *quickfix.MessageRouter) {
	for msgType, h := range map[string]quickfix.MessageRoute{
		"D": e.handleNew, "F": e.handleCancel, "G": e.handleReplace, "H": e.handleStatus,
		"E": e.handleNewList, "K": e.handleListCancel, "M": e.handleListStatusRequest, "q": e.handleMassCancel,
	} {
		obs := metrics.OrderLatency.WithLabelValues(msgType)
		r.AddRoute(quickfix.BeginStringFIX44, msgType, func(m *quickfix.Message, id quickfix.SessionID) quickfix.MessageRejectError {
			start := time.Now()
			defer func() { obs.Observe(time.Since(start).Seconds()) }()
			return h(m, id)
		})
	}
}

// Close stops the session outboxes.
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, b := range e.outboxes {
		b.close()
	}
	e.outboxes = map[quickfix.SessionID]*outbox{}
}

// Orders returns copies of the orders of a session (all sessions if id is
// zero), for tests and the admin API.
func (e *Engine) Orders(id quickfix.SessionID) []Order {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Order
	for _, o := range e.book.orders {
		if id == (quickfix.SessionID{}) || o.Session == id {
			c := *o
			c.Fills = append([]Fill(nil), o.Fills...)
			out = append(out, c)
		}
	}
	return out
}

// fieldGetter is a message body, header or repeating group entry.
type fieldGetter interface {
	GetString(quickfix.Tag) (string, quickfix.MessageRejectError)
}

// incoming wraps a received message (or one NewOrderList entry) for field
// access.
type incoming struct {
	body, header fieldGetter
}

func msgFields(m *quickfix.Message) incoming { return incoming{body: m.Body, header: m.Header} }

func (in incoming) str(t quickfix.Tag) string {
	v, _ := in.body.GetString(t)
	return v
}

func (in incoming) dec(t quickfix.Tag) (decimal.Decimal, bool) {
	v, rerr := in.body.GetString(t)
	if rerr != nil {
		return decimal.Decimal{}, false
	}
	d, err := decimal.NewFromString(v)
	return d, err == nil
}

func (in incoming) request(msgType string, id quickfix.SessionID) Request {
	r := Request{
		MsgType: msgType,
		CompID:  id.TargetCompID,
		Symbol:  in.str(tag.Symbol),
		Side:    in.str(tag.Side),
		OrdType: in.str(tag.OrdType),
		Account: in.str(tag.Account),
		ClOrdID: in.str(tag.ClOrdID),
		Tag: func(t int) (string, bool) {
			if v, err := in.body.GetString(quickfix.Tag(t)); err == nil {
				return v, true
			}
			if in.header != nil {
				if v, err := in.header.GetString(quickfix.Tag(t)); err == nil {
					return v, true
				}
			}
			return "", false
		},
	}
	r.Qty, _ = in.dec(tag.OrderQty)
	if p, ok := in.dec(tag.Price); ok {
		r.Price = &p
	}
	return r
}

func (e *Engine) handleNew(msg *quickfix.Message, id quickfix.SessionID) quickfix.MessageRejectError {
	in := msgFields(msg)
	req := in.request("D", id)
	now := e.sched.Now()

	e.mu.Lock()
	defer e.mu.Unlock()

	o := &Order{
		Session: id, ClOrdID: req.ClOrdID, Symbol: req.Symbol, Side: req.Side, OrdType: req.OrdType,
		Account: req.Account, OrderQty: req.Qty, LeavesQty: req.Qty, Status: enum.OrdStatus_PENDINGNEW,
		CreatedAt: now,
	}

	// Checks before rules (section 7.1). A duplicate ClOrdID is not stored,
	// so it cannot shadow the original order.
	if e.book.byClOrd(id, req.ClOrdID) != nil {
		o.OrderID = "NONE"
		e.rejectNewLocked(o, enum.OrdRejReason_DUPLICATE, "Duplicate ClOrdID")
		return nil
	}
	e.newOrderIDLocked(o)
	e.book.add(o)
	e.acceptLocked(o, req, e.rules.Load(), now)
	return nil
}

func (e *Engine) newOrderIDLocked(o *Order) {
	e.orderSeq++
	o.seq = e.orderSeq
	o.OrderID = fmt.Sprintf("ORD-%d", e.orderSeq)
}

// acceptLocked runs the per-order checks of section 7.1, then matches the
// rules and schedules the action chain. o is already in the book.
func (e *Engine) acceptLocked(o *Order, req Request, rs *RuleSet, now time.Time) {
	switch _, _, known := e.quotes.Last(req.Symbol); {
	case !req.Qty.IsPositive():
		e.rejectNewLocked(o, enum.OrdRejReason_INCORRECTQUANTITY, "OrderQty must be positive")
		return
	case req.OrdType != string(enum.OrdType_MARKET):
		e.rejectNewLocked(o, enum.OrdRejReason_UNSUPPORDERCHAR, "Only market orders (OrdType=1) are supported")
		return
	case !known:
		e.rejectNewLocked(o, enum.OrdRejReason_UNKNOWNSYM, "Unknown symbol")
		return
	}
	name, steps := rs.Match(req)
	o.Rule = name
	e.scheduleLocked(o, steps, now)
}

func (e *Engine) rejectNewLocked(o *Order, reason enum.OrdRejReason, text string) {
	o.close(enum.OrdStatus_REJECTED)
	if o.OrderID != "NONE" {
		e.book.markClosed(o)
	}
	e.reportLocked(o, enum.ExecType_REJECTED, func(r *erFields) {
		r.ordRejReason = string(reason)
		r.text = text
	})
}

// scheduleLocked puts every step at its cumulative delay. A partial_fill
// with count > 1 becomes count fills, delay_ms apart.
func (e *Engine) scheduleLocked(o *Order, steps []Step, now time.Time) {
	gen := o.chain
	at := now
	for _, st := range steps {
		pieces := 1
		if st.Type == "partial_fill" {
			pieces = st.Count
		}
		for k := range pieces {
			at = at.Add(st.Delay)
			e.sched.At(at, func() { e.runStep(o, gen, st, k) })
		}
	}
}

func (e *Engine) runStep(o *Order, gen uint64, st Step, piece int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if o.chain != gen || o.Closed() {
		return
	}
	switch st.Type {
	case "ack":
		if o.Status == enum.OrdStatus_PENDINGNEW {
			o.Status = enum.OrdStatus_NEW
		}
		e.reportLocked(o, enum.ExecType_NEW, nil)
	case "fill":
		e.fillLocked(o, st, o.LeavesQty)
	case "partial_fill":
		total := st.Qty
		if total.IsZero() {
			total = o.OrderQty.Mul(st.Ratio)
		}
		e.fillLocked(o, st, splitPiece(total, st.Count, piece, o.OrderQty))
	case "reject":
		o.close(enum.OrdStatus_REJECTED)
		e.book.markClosed(o)
		e.reportLocked(o, enum.ExecType_REJECTED, func(r *erFields) {
			r.ordRejReason = fmt.Sprint(st.Reason)
			r.text = st.Text
		})
	case "cancel":
		o.close(enum.OrdStatus_CANCELED)
		e.book.markClosed(o)
		e.reportLocked(o, enum.ExecType_CANCELED, func(r *erFields) { r.text = st.Text })
	case "expire":
		o.close(enum.OrdStatus_EXPIRED)
		e.book.markClosed(o)
		e.reportLocked(o, enum.ExecType_EXPIRED, func(r *erFields) { r.text = st.Text })
	case "ignore":
	case "business_reject":
		// The order is treated as never accepted.
		o.chain++
		countOrder(o, "business_reject")
		e.book.remove(o)
		e.outboxLocked(o.Session).push(businessReject("D", o.ClOrdID, st.Reason, st.Text))
	}
}

// splitPiece splits total into count parts with the same precision as the
// order quantity; the last part takes the remainder.
func splitPiece(total decimal.Decimal, count, k int, orderQty decimal.Decimal) decimal.Decimal {
	places := max(-orderQty.Exponent(), 0)
	total = total.RoundFloor(places)
	part := total.Div(decimal.NewFromInt(int64(count))).RoundFloor(places)
	if k == count-1 {
		return total.Sub(part.Mul(decimal.NewFromInt(int64(count - 1))))
	}
	return part
}

func (e *Engine) fillLocked(o *Order, st Step, qty decimal.Decimal) {
	qty = decimal.Min(qty, o.LeavesQty)
	if !qty.IsPositive() {
		return
	}
	px, ok := e.fillPrice(o, st)
	if !ok {
		o.close(enum.OrdStatus_CANCELED)
		e.book.markClosed(o)
		e.reportLocked(o, enum.ExecType_CANCELED, func(r *erFields) { r.text = "No market price for " + o.Symbol })
		return
	}
	e.execSeq++
	f := Fill{ExecID: fmt.Sprintf("EXE-%d", e.execSeq), Qty: qty, Px: px, Time: e.sched.Now()}
	if o.Status == enum.OrdStatus_PENDINGNEW {
		o.Status = enum.OrdStatus_NEW
	}
	o.applyFill(f)
	if o.Closed() {
		o.chain++
		e.book.markClosed(o)
	}
	e.reportLocked(o, enum.ExecType_TRADE, func(r *erFields) {
		r.execID = f.ExecID
		r.lastQty, r.lastPx = &f.Qty, &f.Px
	})
}

// fillPrice: market fills at the ask for buys and the bid for sells, plus
// slippage; fixed fills at the rule's price (section 7.4).
func (e *Engine) fillPrice(o *Order, st Step) (decimal.Decimal, bool) {
	if st.PriceMode == "fixed" {
		return st.Price, true
	}
	q, cfg, ok := e.quotes.Last(o.Symbol)
	if !ok {
		return decimal.Decimal{}, false
	}
	px := q.Bid
	if isBuy(o.Side) {
		px = q.Ask
	}
	tick := decimal.NewFromFloat(cfg.TickSize)
	return slip(decimal.NewFromFloat(px), o.Side, cfg.Slippage, tick, fillRand(cfg, o)), true
}

// fillRand is seeded from the symbol seed, the order and its fill count, so
// random slippage is reproducible with a fixed seed.
func fillRand(cfg config.Symbol, o *Order) *rand.Rand {
	var seed uint64
	if cfg.Seed != nil {
		seed = uint64(*cfg.Seed)
	}
	h := fnv.New64a()
	fmt.Fprintf(h, "%s|%s|%d", o.Session, o.ClOrdID, len(o.Fills))
	return rand.New(rand.NewPCG(seed, h.Sum64()))
}

func (e *Engine) handleCancel(msg *quickfix.Message, id quickfix.SessionID) quickfix.MessageRejectError {
	in := msgFields(msg)
	orig, clOrdID := in.str(tag.OrigClOrdID), in.str(tag.ClOrdID)

	e.mu.Lock()
	defer e.mu.Unlock()
	o := e.book.byClOrd(id, orig)
	if o == nil || o.Closed() {
		e.cancelRejectLocked(id, o, orig, clOrdID, enum.CxlRejResponseTo_ORDCXLREQ, enum.CxlRejReason_UNKNOWN, "Unknown or closed order")
		return nil
	}
	if e.book.byClOrd(id, clOrdID) != nil {
		e.cancelRejectLocked(id, o, orig, clOrdID, enum.CxlRejResponseTo_ORDCXLREQ, enum.CxlRejReason_DUPCLORDID, "Duplicate ClOrdID")
		return nil
	}
	req := in.request("F", id)
	req.Symbol, req.Side = o.Symbol, o.Side // F carries them, but match on the order's values
	e.decideLocked(o, req, enum.CxlRejResponseTo_ORDCXLREQ, orig, clOrdID, func() {
		o.ClOrdID, o.OrigClOrdID = clOrdID, orig
		e.book.index(o, clOrdID)
		o.close(enum.OrdStatus_CANCELED)
		e.book.markClosed(o)
		e.reportLocked(o, enum.ExecType_CANCELED, solicited)
	})
	return nil
}

func (e *Engine) handleReplace(msg *quickfix.Message, id quickfix.SessionID) quickfix.MessageRejectError {
	in := msgFields(msg)
	orig, clOrdID := in.str(tag.OrigClOrdID), in.str(tag.ClOrdID)
	newQty, _ := in.dec(tag.OrderQty)

	e.mu.Lock()
	defer e.mu.Unlock()
	o := e.book.byClOrd(id, orig)
	reject := func(reason enum.CxlRejReason, text string) quickfix.MessageRejectError {
		e.cancelRejectLocked(id, o, orig, clOrdID, enum.CxlRejResponseTo_ORDCXLREPREQ, reason, text)
		return nil
	}
	switch {
	case o == nil || o.Closed():
		return reject(enum.CxlRejReason_UNKNOWN, "Unknown or closed order")
	case e.book.byClOrd(id, clOrdID) != nil:
		return reject(enum.CxlRejReason_DUPCLORDID, "Duplicate ClOrdID")
	case in.str(tag.OrdType) != string(enum.OrdType_MARKET):
		return reject(enum.CxlRejReason_BROKEROPT, "Only OrderQty may change; OrdType must stay 1 (Market)")
	case in.str(tag.Symbol) != o.Symbol || in.str(tag.Side) != o.Side:
		return reject(enum.CxlRejReason_BROKEROPT, "Only OrderQty may change; Symbol and Side must match the order")
	case !newQty.GreaterThan(o.CumQty):
		return reject(enum.CxlRejReason_BROKEROPT, "OrderQty must exceed CumQty")
	}
	e.decideLocked(o, in.request("G", id), enum.CxlRejResponseTo_ORDCXLREPREQ, orig, clOrdID, func() {
		if !newQty.GreaterThan(o.CumQty) { // fills arrived while the replace was delayed
			e.cancelRejectLocked(id, o, orig, clOrdID, enum.CxlRejResponseTo_ORDCXLREPREQ, enum.CxlRejReason_TOOLATE, "OrderQty must exceed CumQty")
			return
		}
		o.ClOrdID, o.OrigClOrdID = clOrdID, orig
		e.book.index(o, clOrdID)
		o.OrderQty = newQty
		o.LeavesQty = newQty.Sub(o.CumQty)
		// Remaining steps keep running and read the new quantities.
		e.reportLocked(o, enum.ExecType_REPLACED, solicited)
	})
	return nil
}

// decideLocked runs the F/G rule: no rule or ack accepts (ack may delay),
// reject answers 9, ignore answers nothing.
func (e *Engine) decideLocked(o *Order, req Request, to enum.CxlRejResponseTo, orig, clOrdID string, accept func()) {
	_, steps := e.rules.Load().Match(req)
	if len(steps) == 0 {
		accept()
		return
	}
	st := steps[0]
	switch st.Type {
	case "ignore":
	case "reject":
		e.cancelRejectLocked(o.Session, o, orig, clOrdID, to, enum.CxlRejReason(fmt.Sprint(st.Reason)), st.Text)
	case "ack":
		if st.Delay == 0 {
			accept()
			return
		}
		e.sched.At(e.sched.Now().Add(st.Delay), func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			if o.Closed() {
				e.cancelRejectLocked(o.Session, o, orig, clOrdID, to, enum.CxlRejReason_TOOLATE, "Order closed before the request was processed")
				return
			}
			accept()
		})
	}
}

func (e *Engine) handleStatus(msg *quickfix.Message, id quickfix.SessionID) quickfix.MessageRejectError {
	in := msgFields(msg)
	e.mu.Lock()
	defer e.mu.Unlock()
	var o *Order
	if oid := in.str(tag.OrderID); oid != "" {
		if c := e.book.orders[oid]; c != nil && c.Session == id {
			o = c
		}
	}
	if o == nil {
		o = e.book.byClOrd(id, in.str(tag.ClOrdID))
	}
	if o == nil {
		unknown := &Order{OrderID: "NONE", Session: id, ClOrdID: in.str(tag.ClOrdID), Symbol: in.str(tag.Symbol),
			Side: in.str(tag.Side), Status: enum.OrdStatus_REJECTED}
		e.reportLocked(unknown, enum.ExecType_ORDERSTATUS, func(r *erFields) { r.solicited, r.text = true, "Unknown order" })
		return nil
	}
	e.reportLocked(o, enum.ExecType_ORDERSTATUS, solicited)
	return nil
}

func (e *Engine) cancelRejectLocked(id quickfix.SessionID, o *Order, orig, clOrdID string, to enum.CxlRejResponseTo, reason enum.CxlRejReason, text string) {
	orderID, status := "NONE", enum.OrdStatus_REJECTED
	if o != nil {
		orderID, status = o.OrderID, o.Status
	}
	e.outboxLocked(id).push(cancelReject(orderID, clOrdID, orig, status, to, reason, text, e.sched.Now()))
}

// solicited marks a report that answers a client request (F, G, H, K, q):
// it is sent even when list orders report through ListStatus only.
func solicited(r *erFields) { r.solicited = true }

// reportLocked sends an ExecutionReport to the order's session and a drop
// copy to the monitor session (section 8.1). For list orders it also feeds
// the list's ListStatus reporting (section 7.6).
func (e *Engine) reportLocked(o *Order, et enum.ExecType, extra func(*erFields)) {
	f := erFields{execType: string(et), now: e.sched.Now()}
	if extra != nil {
		extra(&f)
	}
	if f.execID == "" {
		e.execSeq++
		f.execID = fmt.Sprintf("EXE-%d", e.execSeq)
	}
	var l *List
	if o.ListID != "" {
		l = e.book.list(o.Session, o.ListID)
	}
	if l == nil || f.solicited || l.lists.Report != "list_status" {
		e.deliverLocked(o.Session, executionReport(o, f), true)
	}
	if l != nil {
		e.listChangedLocked(l)
	}
}

// deliverLocked queues a message for a session and, if copy is set and
// drop copy is on, a copy for the monitor session tagged with 5001.
func (e *Engine) deliverLocked(id quickfix.SessionID, msg *quickfix.Message, copy bool) {
	if dc := e.dropCopy.Load(); copy && dc != nil && id != *dc {
		cp := quickfix.NewMessage()
		msg.CopyInto(cp)
		cp.Body.SetField(tag.MirrorSessionID, quickfix.FIXString(id.String()))
		e.outboxLocked(*dc).push(cp)
	}
	e.outboxLocked(id).push(msg)
}

func (e *Engine) outboxLocked(id quickfix.SessionID) *outbox {
	b, ok := e.outboxes[id]
	if !ok {
		b = newOutbox(id, e.send, e.log)
		e.outboxes[id] = b
	}
	return b
}
