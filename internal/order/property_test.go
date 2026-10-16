package order

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/quynv/mock-fixser/fix/enum"
)

// Random orders, cancels and replaces with random delays; the order state
// invariants of section 7.5 must hold after every step.
func TestOrderInvariantsUnderRandomFlow(t *testing.T) {
	q := aaplQuotes(nil)
	h := newHarness(t, `default_action:
  - { type: ack, delay_ms: 5 }
  - { type: partial_fill, ratio: 0.3, count: 2, delay_ms: 7 }
  - { type: fill, delay_ms: 11 }
`, q, false)
	go func() { // drain sent messages; only state is checked here
		for range h.out {
		}
	}()

	rng := rand.New(rand.NewPCG(1, 2))
	var ids []string
	next := 0
	for step := range 1000 {
		switch op := rng.IntN(10); {
		case op < 4 || len(ids) == 0:
			next++
			id := fmt.Sprintf("N%d", next)
			ids = append(ids, id)
			side := []enum.Side{enum.Side_BUY, enum.Side_SELL}[rng.IntN(2)]
			qty := fmt.Sprint(1 + rng.IntN(500))
			if rng.IntN(2) == 0 {
				h.handle(nos(id, side, qty, enum.OrdType_MARKET))
			} else { // limits around the market, sometimes IOC or FOK
				px := fmt.Sprintf("%.2f", 229.5+rng.Float64())
				tif := []string{"", "", "3", "4"}[rng.IntN(4)]
				h.handle(limitOrder(id, side, qty, px, enum.OrdType_LIMIT, "", tif))
			}
		case op < 6:
			i := rng.IntN(len(ids))
			next++
			newID := fmt.Sprintf("X%d", next)
			h.handle(cxl(ids[i], newID))
			ids[i] = newID
		case op < 8:
			i := rng.IntN(len(ids))
			next++
			newID := fmt.Sprintf("R%d", next)
			r := rpl(ids[i], newID, fmt.Sprint(1+rng.IntN(800)), enum.OrdType_MARKET)
			if o := orderByClOrdID(h, ids[i]); o != nil && o.OrdType == ordLimit {
				r = rpl(ids[i], newID, fmt.Sprint(1+rng.IntN(800)), enum.OrdType_LIMIT)
				r.SetPrice(decimal.NewFromFloat(229.5+rng.Float64()).Round(2), 2)
			}
			h.handle(r)
			ids[i] = newID
		case op < 9:
			mid := 229.5 + rng.Float64()
			h.moveMarket(q, mid-0.01, mid+0.01)
		default:
			h.advance(time.Duration(rng.IntN(15)) * time.Millisecond)
		}
		if step%50 == 0 {
			time.Sleep(time.Millisecond) // let the scheduler catch up
		}
		checkInvariants(t, h.e.Orders(client1))
	}
}

func checkInvariants(t *testing.T, orders []Order) {
	t.Helper()
	for _, o := range orders {
		sum, notional := decimal.Zero, decimal.Zero
		for _, f := range o.Fills {
			sum = sum.Add(f.Qty)
			notional = notional.Add(f.Qty.Mul(f.Px))
		}
		switch {
		case !sum.Equal(o.CumQty):
			t.Fatalf("%s: fills %s != CumQty %s", o.OrderID, sum, o.CumQty)
		case o.CumQty.GreaterThan(o.OrderQty):
			t.Fatalf("%s: CumQty %s > OrderQty %s", o.OrderID, o.CumQty, o.OrderQty)
		case o.Closed() && !o.LeavesQty.IsZero():
			t.Fatalf("%s: closed with LeavesQty %s", o.OrderID, o.LeavesQty)
		case !o.Closed() && !o.CumQty.Add(o.LeavesQty).Equal(o.OrderQty):
			t.Fatalf("%s: CumQty %s + LeavesQty %s != OrderQty %s", o.OrderID, o.CumQty, o.LeavesQty, o.OrderQty)
		case o.CumQty.IsPositive() && !o.AvgPx.Round(6).Equal(notional.DivRound(o.CumQty, 6)):
			t.Fatalf("%s: AvgPx %s, want %s", o.OrderID, o.AvgPx, notional.DivRound(o.CumQty, 6))
		case o.Status == enum.OrdStatus_FILLED && !o.CumQty.Equal(o.OrderQty):
			t.Fatalf("%s: Filled with CumQty %s of %s", o.OrderID, o.CumQty, o.OrderQty)
		}
		// Fills are never worse than the limit (section 7.4). A replace may
		// change the limit after earlier fills, so check fills only when
		// the order was never replaced.
		if o.Price != nil && o.OrigClOrdID == "" {
			for _, f := range o.Fills {
				if (isBuy(o.Side) && f.Px.GreaterThan(*o.Price)) || (!isBuy(o.Side) && f.Px.LessThan(*o.Price)) {
					t.Fatalf("%s: side %s filled at %s beyond limit %s", o.OrderID, o.Side, f.Px, o.Price)
				}
			}
		}
	}
}

func orderByClOrdID(h *harness, id string) *Order {
	for _, o := range h.e.Orders(client1) {
		if o.ClOrdID == id {
			return &o
		}
	}
	return nil
}
