package order

import (
	"time"

	"github.com/quickfixgo/quickfix"
	"github.com/shopspring/decimal"

	"github.com/quynv/mock-fixser/fix/enum"
)

// DefaultRetention is how many closed orders are kept for H and lookups
// (section 7.5).
const DefaultRetention = 10000

type Fill struct {
	ExecID string
	Qty    decimal.Decimal
	Px     decimal.Decimal
	Time   time.Time
}

// Order is the in-memory state of one order. Invariants: CumQty +
// LeavesQty = OrderQty while open, and AvgPx is the qty-weighted average
// of the fills.
type Order struct {
	OrderID     string
	Session     quickfix.SessionID
	ClOrdID     string
	OrigClOrdID string
	Symbol      string
	Side        string
	OrdType     string
	Account     string
	OrderQty    decimal.Decimal
	CumQty      decimal.Decimal
	LeavesQty   decimal.Decimal
	AvgPx       decimal.Decimal
	Status      enum.OrdStatus
	Rule        string
	Fills       []Fill
	CreatedAt   time.Time
	ListID      string // set for orders of a NewOrderList
	ListSeqNo   int

	seq uint64 // creation order

	// chain is bumped when an order's pending actions are cancelled; steps
	// scheduled under an older value do nothing.
	chain uint64
}

func (o *Order) Closed() bool {
	switch o.Status {
	case enum.OrdStatus_FILLED, enum.OrdStatus_CANCELED, enum.OrdStatus_REJECTED, enum.OrdStatus_EXPIRED:
		return true
	}
	return false
}

func (o *Order) applyFill(f Fill) {
	notional := o.AvgPx.Mul(o.CumQty).Add(f.Px.Mul(f.Qty))
	o.CumQty = o.CumQty.Add(f.Qty)
	o.LeavesQty = o.OrderQty.Sub(o.CumQty)
	o.AvgPx = notional.DivRound(o.CumQty, 10)
	o.Fills = append(o.Fills, f)
	if o.LeavesQty.Sign() <= 0 {
		o.LeavesQty = decimal.Zero
		o.Status = enum.OrdStatus_FILLED
	} else {
		o.Status = enum.OrdStatus_PARTIAL
	}
}

// close ends the order with a terminal status and cancels pending steps.
func (o *Order) close(status enum.OrdStatus) {
	o.Status = status
	o.LeavesQty = decimal.Zero
	o.chain++
}

type clKey struct {
	session quickfix.SessionID
	clOrdID string
}

// book indexes orders by OrderID and by every ClOrdID they have used in
// their session, and lists by ListID.
type book struct {
	retention int
	lists     map[clKey]*List
	orders    map[string]*Order
	byClOrdID map[clKey]*Order
	clOrdIDs  map[string][]string // OrderID -> ClOrdIDs, for eviction
	closed    []string            // closed OrderIDs, oldest first
}

func newBook(retention int) *book {
	return &book{
		retention: retention,
		lists:     map[clKey]*List{},
		orders:    map[string]*Order{},
		byClOrdID: map[clKey]*Order{},
		clOrdIDs:  map[string][]string{},
	}
}

func (b *book) add(o *Order) {
	b.orders[o.OrderID] = o
	b.index(o, o.ClOrdID)
}

func (b *book) index(o *Order, clOrdID string) {
	b.byClOrdID[clKey{o.Session, clOrdID}] = o
	b.clOrdIDs[o.OrderID] = append(b.clOrdIDs[o.OrderID], clOrdID)
}

func (b *book) list(s quickfix.SessionID, listID string) *List {
	return b.lists[clKey{s, listID}]
}

func (b *book) byClOrd(s quickfix.SessionID, clOrdID string) *Order {
	return b.byClOrdID[clKey{s, clOrdID}]
}

func (b *book) remove(o *Order) {
	for _, c := range b.clOrdIDs[o.OrderID] {
		delete(b.byClOrdID, clKey{o.Session, c})
	}
	delete(b.clOrdIDs, o.OrderID)
	delete(b.orders, o.OrderID)
}

// closed records a closed order and evicts the oldest beyond retention.
func (b *book) markClosed(o *Order) {
	b.closed = append(b.closed, o.OrderID)
	for len(b.closed) > b.retention {
		if old, ok := b.orders[b.closed[0]]; ok {
			b.remove(old)
		}
		b.closed = b.closed[1:]
	}
}
