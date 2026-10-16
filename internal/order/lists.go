package order

import (
	"fmt"
	"slices"
	"time"

	"github.com/quickfixgo/quickfix"
	"github.com/shopspring/decimal"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/fix/field"
	"github.com/quynv/mock-fixser/fix/fix44/liststatus"
	"github.com/quynv/mock-fixser/fix/fix44/neworderlist"
	"github.com/quynv/mock-fixser/fix/fix44/ordermasscancelreport"
	"github.com/quynv/mock-fixser/fix/tag"
	"github.com/quynv/mock-fixser/internal/config"
)

// List is a NewOrderList and its orders, in ListSeqNo order.
type List struct {
	ListID  string
	Session quickfix.SessionID
	Orders  []*Order

	lists     config.OrderLists // settings when the list arrived
	dirty     bool              // changed since the last ListStatus
	finalSent bool
	deferred  bool // K in progress: hold ListStatus updates
}

func (l *List) allClosed() bool {
	for _, o := range l.Orders {
		if !o.Closed() {
			return false
		}
	}
	return true
}

func (l *List) orderStatus() enum.ListOrderStatus {
	if l.allClosed() {
		return enum.ListOrderStatus_ALLDONE
	}
	return enum.ListOrderStatus_EXEC
}

// listEntry is one order of an incoming NewOrderList.
type listEntry struct {
	req   Request
	seqNo int
}

func (e *Engine) handleNewList(msg *quickfix.Message, id quickfix.SessionID) quickfix.MessageRejectError {
	rs := e.rules.Load()
	if !rs.Lists.IsEnabled() {
		return quickfix.UnsupportedMessageType()
	}
	nol := neworderlist.FromMessage(msg)
	listID, err := nol.GetListID()
	if err != nil {
		return err
	}
	tot, err := nol.GetTotNoOrders()
	if err != nil {
		return err
	}
	grp, err := nol.GetNoOrders()
	if err != nil {
		return err
	}
	entries := make([]listEntry, grp.Len())
	for i := range grp.Len() {
		g := grp.Get(i)
		req := incoming{body: g.FieldMap}.request("D", id)
		req.InList, req.ListID = true, listID
		req.ListSeqNo, _ = g.GetListSeqNo()
		entries[i] = listEntry{req: req, seqNo: req.ListSeqNo}
	}
	slices.SortStableFunc(entries, func(a, b listEntry) int { return a.seqNo - b.seqNo })
	now := e.sched.Now()

	e.mu.Lock()
	defer e.mu.Unlock()
	reject := func(text string) quickfix.MessageRejectError {
		e.deliverLocked(id, rejectedListStatus(listID, entries, text, now), true)
		return nil
	}

	// Structural checks (section 7.6, step 1): nothing is created on failure.
	switch {
	case len(entries) == 0:
		return reject("NoOrders is empty")
	case len(entries) > rs.Lists.MaxOrdersPerList:
		return reject(fmt.Sprintf("List has %d orders, more than max_orders_per_list %d", len(entries), rs.Lists.MaxOrdersPerList))
	case tot != len(entries):
		return reject(fmt.Sprintf("TotNoOrders %d does not match %d NoOrders entries", tot, len(entries)))
	case e.book.list(id, listID) != nil:
		return reject("ListID " + listID + " already used in this session")
	}
	seen := map[string]bool{}
	for _, en := range entries {
		if seen[en.req.ClOrdID] || e.book.byClOrd(id, en.req.ClOrdID) != nil {
			return reject("Duplicate ClOrdID " + en.req.ClOrdID)
		}
		seen[en.req.ClOrdID] = true
	}

	// List rules (step 2).
	act := rs.MatchList(ListRequest{ListID: listID, CompID: id.TargetCompID, TotNoOrders: len(entries)})
	if act.Type == "reject_list" {
		text := act.Text
		if text == "" {
			text = "Rejected by list rule " + act.Rule
		}
		return reject(text)
	}

	l := &List{ListID: listID, Session: id, lists: rs.Lists}
	for _, en := range entries {
		r := en.req
		o := &Order{
			Session: id, ClOrdID: r.ClOrdID, Symbol: r.Symbol, Side: r.Side, OrdType: r.OrdType, Account: r.Account,
			OrderQty: r.Qty, LeavesQty: r.Qty, Status: enum.OrdStatus_PENDINGNEW, CreatedAt: now,
			ListID: listID, ListSeqNo: en.seqNo,
		}
		e.newOrderIDLocked(o)
		e.book.add(o)
		l.Orders = append(l.Orders, o)
	}
	e.book.lists[clKey{id, listID}] = l

	// Ack (step 3, possibly delayed by delay_ack), then each order goes
	// through the order rules like a D (step 4).
	e.sched.At(now.Add(act.Delay), func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		at := e.sched.Now()
		if rs.Lists.Ack == "list_status" {
			e.deliverLocked(id, listStatusMsg(l, enum.ListStatusType_ACK, enum.ListOrderStatus_RECVFOREXEC, "", at), true)
		}
		for i, o := range l.Orders {
			if !o.Closed() { // a cancel may have arrived before the ack
				e.acceptLocked(o, entries[i].req, rs, at)
			}
		}
		if rs.Lists.Report != "per_order_er" && !l.allClosed() {
			e.scheduleListStatusLocked(l)
		}
	})
	return nil
}

// listChangedLocked records a change of one of the list's orders. Once
// every order is closed, final_list_status sends ListStatusType=5.
func (e *Engine) listChangedLocked(l *List) {
	if l.deferred {
		return
	}
	l.dirty = true
	if l.allClosed() && !l.finalSent && l.lists.SendsFinalStatus() {
		l.finalSent, l.dirty = true, false
		e.deliverLocked(l.Session, listStatusMsg(l, enum.ListStatusType_ALLDONE, enum.ListOrderStatus_ALLDONE, "", e.sched.Now()), true)
	}
}

// scheduleListStatusLocked sends an aggregated ListStatus every
// list_status_interval_ms while something changed, until the list is done.
func (e *Engine) scheduleListStatusLocked(l *List) {
	interval := time.Duration(l.lists.ListStatusIntervalMs) * time.Millisecond
	e.sched.At(e.sched.Now().Add(interval), func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if l.dirty && !l.finalSent {
			l.dirty = false
			e.deliverLocked(l.Session, listStatusMsg(l, enum.ListStatusType_TIMED, l.orderStatus(), "", e.sched.Now()), true)
		}
		if !l.allClosed() {
			e.scheduleListStatusLocked(l)
		}
	})
}

func (e *Engine) handleListCancel(msg *quickfix.Message, id quickfix.SessionID) quickfix.MessageRejectError {
	if !e.rules.Load().Lists.IsEnabled() {
		return quickfix.UnsupportedMessageType()
	}
	listID := msgFields(msg).str(tag.ListID)
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.sched.Now()
	l := e.book.list(id, listID)
	if l == nil {
		e.deliverLocked(id, unknownListStatus(listID, now), true)
		return nil
	}
	// Open orders are canceled; closed orders keep their state.
	l.deferred = true
	for _, o := range l.Orders {
		if !o.Closed() {
			o.close(enum.OrdStatus_CANCELED)
			e.book.markClosed(o)
			e.reportLocked(o, enum.ExecType_CANCELED, func(r *erFields) { r.solicited, r.text = true, "List canceled" })
		}
	}
	l.deferred = false
	e.deliverLocked(id, listStatusMsg(l, enum.ListStatusType_RESP, l.orderStatus(), "", now), true)
	e.listChangedLocked(l)
	return nil
}

func (e *Engine) handleListStatusRequest(msg *quickfix.Message, id quickfix.SessionID) quickfix.MessageRejectError {
	if !e.rules.Load().Lists.IsEnabled() {
		return quickfix.UnsupportedMessageType()
	}
	listID := msgFields(msg).str(tag.ListID)
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.sched.Now()
	if l := e.book.list(id, listID); l != nil {
		e.deliverLocked(id, listStatusMsg(l, enum.ListStatusType_RESP, l.orderStatus(), "", now), true)
	} else {
		e.deliverLocked(id, unknownListStatus(listID, now), true)
	}
	return nil
}

// handleMassCancel cancels the session's open orders, by symbol (1) or all
// (7), and answers with OrderMassCancelReport (r) followed by an
// ExecutionReport per canceled order.
func (e *Engine) handleMassCancel(msg *quickfix.Message, id quickfix.SessionID) quickfix.MessageRejectError {
	in := msgFields(msg)
	clOrdID, typ, symbol := in.str(tag.ClOrdID), enum.MassCancelRequestType(in.str(tag.MassCancelRequestType)), in.str(tag.Symbol)

	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.sched.Now()
	e.massSeq++
	r := ordermasscancelreport.New(field.NewOrderID(fmt.Sprintf("MC-%d", e.massSeq)),
		field.NewMassCancelRequestType(typ), field.NewMassCancelResponse(enum.MassCancelResponse(typ)))
	r.SetClOrdID(clOrdID)
	r.SetTransactTime(now)
	if symbol != "" {
		r.SetSymbol(symbol)
	}
	rejectWith := func(reason enum.MassCancelRejectReason, text string) quickfix.MessageRejectError {
		r.Set(field.NewMassCancelResponse(enum.MassCancelResponse_CXLREQREJ))
		r.SetMassCancelRejectReason(reason)
		r.SetText(text)
		e.deliverLocked(id, r.ToMessage(), false)
		return nil
	}
	switch typ {
	case enum.MassCancelRequestType_CXLORDERSSECURITY:
		if _, _, ok := e.quotes.Last(symbol); !ok {
			return rejectWith(enum.MassCancelRejectReason_INVALIDSECURITY, "Unknown symbol "+symbol)
		}
	case enum.MassCancelRequestType_CXLALLORDERS:
	default:
		return rejectWith(enum.MassCancelRejectReason_MASSCXLNOTSUPPORTED, "Only MassCancelRequestType 1 (symbol) and 7 (all) are supported")
	}

	var affected []*Order
	for _, o := range e.book.orders {
		if o.Session == id && !o.Closed() && (typ == enum.MassCancelRequestType_CXLALLORDERS || o.Symbol == symbol) {
			affected = append(affected, o)
		}
	}
	slices.SortFunc(affected, func(a, b *Order) int { return int(a.seq) - int(b.seq) })

	r.SetTotalAffectedOrders(len(affected))
	if len(affected) > 0 {
		g := ordermasscancelreport.NewNoAffectedOrdersRepeatingGroup()
		for _, o := range affected {
			a := g.Add()
			a.SetOrigClOrdID(o.ClOrdID)
			a.SetAffectedOrderID(o.OrderID)
		}
		r.SetNoAffectedOrders(g)
	}
	e.deliverLocked(id, r.ToMessage(), false)
	for _, o := range affected {
		o.close(enum.OrdStatus_CANCELED)
		e.book.markClosed(o)
		e.reportLocked(o, enum.ExecType_CANCELED, func(f *erFields) { f.solicited, f.text = true, "Mass cancel "+clOrdID })
	}
	return nil
}

func cxlQty(o *Order) decimal.Decimal {
	switch o.Status {
	case enum.OrdStatus_CANCELED, enum.OrdStatus_EXPIRED:
		return o.OrderQty.Sub(o.CumQty)
	}
	return decimal.Zero
}

// listStatusMsg aggregates the list's orders into ListStatus (N): one
// NoOrders entry per order with CumQty, LeavesQty, CxlQty, AvgPx and
// OrdStatus (section 7.6).
func listStatusMsg(l *List, typ enum.ListStatusType, status enum.ListOrderStatus, text string, now time.Time) *quickfix.Message {
	m := liststatus.New(field.NewListID(l.ListID), field.NewListStatusType(typ), field.NewNoRpts(1),
		field.NewListOrderStatus(status), field.NewRptSeq(1), field.NewTotNoOrders(len(l.Orders)))
	g := liststatus.NewNoOrdersRepeatingGroup()
	for _, o := range l.Orders {
		e := g.Add()
		e.SetClOrdID(o.ClOrdID)
		e.SetCumQty(dec(o.CumQty))
		e.SetOrdStatus(o.Status)
		e.SetLeavesQty(dec(o.LeavesQty))
		e.SetCxlQty(dec(cxlQty(o)))
		e.SetAvgPx(dec(o.AvgPx.Round(8)))
	}
	m.SetNoOrders(g)
	if text != "" {
		m.SetListStatusText(text)
	}
	m.SetTransactTime(now)
	return m.ToMessage()
}

// rejectedListStatus answers a list that failed the structural checks or
// a reject_list rule: ListOrderStatus=7 and every entry Rejected.
func rejectedListStatus(listID string, entries []listEntry, text string, now time.Time) *quickfix.Message {
	l := &List{ListID: listID}
	for _, en := range entries {
		l.Orders = append(l.Orders, &Order{ClOrdID: en.req.ClOrdID, Status: enum.OrdStatus_REJECTED})
	}
	return listStatusMsg(l, enum.ListStatusType_ACK, enum.ListOrderStatus_REJ, text, now)
}

func unknownListStatus(listID string, now time.Time) *quickfix.Message {
	return listStatusMsg(&List{ListID: listID}, enum.ListStatusType_RESP, enum.ListOrderStatus_REJ, "Unknown ListID "+listID, now)
}
