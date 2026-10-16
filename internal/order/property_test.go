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
	h := newHarness(t, `default_action:
  - { type: ack, delay_ms: 5 }
  - { type: partial_fill, ratio: 0.3, count: 2, delay_ms: 7 }
  - { type: fill, delay_ms: 11 }
`, aaplQuotes(nil), false)
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
			h.handle(nos(id, []enum.Side{enum.Side_BUY, enum.Side_SELL}[rng.IntN(2)], fmt.Sprint(1+rng.IntN(500)), enum.OrdType_MARKET))
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
			h.handle(rpl(ids[i], newID, fmt.Sprint(1+rng.IntN(800)), enum.OrdType_MARKET))
			ids[i] = newID
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
	}
}
