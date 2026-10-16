package order

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"
	"github.com/shopspring/decimal"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/fix/field"
	"github.com/quynv/mock-fixser/fix/fix44/listcancelrequest"
	"github.com/quynv/mock-fixser/fix/fix44/liststatus"
	"github.com/quynv/mock-fixser/fix/fix44/liststatusrequest"
	"github.com/quynv/mock-fixser/fix/fix44/neworderlist"
	"github.com/quynv/mock-fixser/fix/fix44/ordermasscancelrequest"
	"github.com/quynv/mock-fixser/fix/tag"
)

type entry struct {
	clOrdID string
	side    enum.Side
	qty     string
	symbol  string
}

func list(listID string, tot int, entries ...entry) neworderlist.NewOrderList {
	m := neworderlist.New(field.NewListID(listID), field.NewBidType(enum.BidType_NOBID), field.NewTotNoOrders(tot))
	g := neworderlist.NewNoOrdersRepeatingGroup()
	for i, e := range entries {
		o := g.Add()
		o.SetClOrdID(e.clOrdID)
		o.SetListSeqNo(i + 1)
		sym := e.symbol
		if sym == "" {
			sym = "AAPL"
		}
		o.SetSymbol(sym)
		o.SetSide(e.side)
		o.SetOrderQty(decimal.RequireFromString(e.qty), 0)
		o.SetOrdType(enum.OrdType_MARKET)
	}
	m.SetNoOrders(g)
	return m
}

func three(listID string) neworderlist.NewOrderList {
	return list(listID, 3,
		entry{listID + "-1", enum.Side_BUY, "100", ""},
		entry{listID + "-2", enum.Side_SELL, "200", ""},
		entry{listID + "-3", enum.Side_BUY, "300", ""})
}

// listEntries returns "ClOrdID:OrdStatus:CumQty:LeavesQty:CxlQty" per N entry.
func listEntries(t *testing.T, m *quickfix.Message) []string {
	t.Helper()
	g, err := liststatus.FromMessage(m).GetNoOrders()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := range g.Len() {
		e := g.Get(i)
		id, _ := e.GetClOrdID()
		st, _ := e.GetOrdStatus()
		cum, _ := e.GetCumQty()
		lv, _ := e.GetLeavesQty()
		cx, _ := e.GetCxlQty()
		out = append(out, fmt.Sprintf("%s:%s:%s:%s:%s", id, st, cum, lv, cx))
	}
	return out
}

const (
	ListStatusType  = tag.ListStatusType
	ListOrderStatus = tag.ListOrderStatus
	ListID          = tag.ListID
)

func TestListPerOrderReports(t *testing.T) {
	h := newHarness(t, "", aaplQuotes(nil), false)
	h.handle(three("L1"))
	ack := h.expect(MsgType, "N", ListID, "L1", ListStatusType, "1", ListOrderStatus, "2", tag.TotNoOrders, "3")
	if got := listEntries(t, ack); strings.Join(got, ",") != "L1-1:A:0:100:0,L1-2:A:0:200:0,L1-3:A:0:300:0" {
		t.Fatalf("ack entries %v", got)
	}
	for _, id := range []string{"L1-1", "L1-2", "L1-3"} {
		h.expect(MsgType, "8", tag.ClOrdID, id, ExecType, "0", ListID, "L1")
		h.expect(MsgType, "8", tag.ClOrdID, id, ExecType, "F", ListID, "L1")
	}
	done := h.expect(MsgType, "N", ListStatusType, "5", ListOrderStatus, "6")
	if got := listEntries(t, done); got[1] != "L1-2:2:200:0:0" {
		t.Fatalf("final entries %v", got)
	}
	h.none()
}

func TestListStructuralRejects(t *testing.T) {
	h := newHarness(t, "order_lists: { max_orders_per_list: 3 }\n", aaplQuotes(nil), false)
	h.handle(nos("TAKEN", enum.Side_BUY, "1", enum.OrdType_MARKET))
	h.expect(ExecType, "0")
	h.expect(ExecType, "F")
	h.handle(three("OK"))
	for range 8 { // N ack, 6 ERs, final N
		h.next()
	}

	tests := []struct {
		name string
		msg  neworderlist.NewOrderList
		text string
	}{
		{"count mismatch", list("A", 2, entry{"A-1", enum.Side_BUY, "1", ""}), "TotNoOrders 2 does not match 1"},
		{"duplicate in list", list("B", 2, entry{"B-1", enum.Side_BUY, "1", ""}, entry{"B-1", enum.Side_BUY, "1", ""}), "Duplicate ClOrdID B-1"},
		{"ClOrdID used in session", list("C", 1, entry{"TAKEN", enum.Side_BUY, "1", ""}), "Duplicate ClOrdID TAKEN"},
		{"ListID reused", list("OK", 1, entry{"D-1", enum.Side_BUY, "1", ""}), "ListID OK already used"},
		{"too many orders", list("E", 4, entry{"E-1", enum.Side_BUY, "1", ""}, entry{"E-2", enum.Side_BUY, "1", ""},
			entry{"E-3", enum.Side_BUY, "1", ""}, entry{"E-4", enum.Side_BUY, "1", ""}), "more than max_orders_per_list 3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h.t = t
			h.handle(tt.msg)
			m := h.expect(MsgType, "N", ListOrderStatus, "7", ListStatusType, "1")
			if txt := get(m, tag.ListStatusText); !strings.Contains(txt, tt.text) {
				t.Fatalf("ListStatusText %q, want %q", txt, tt.text)
			}
			for _, e := range listEntries(t, m) {
				if !strings.Contains(e, ":8:") {
					t.Fatalf("entry %s not rejected", e)
				}
			}
			h.none()
		})
	}
	if n := len(h.e.Orders(client1)); n != 4 {
		t.Fatalf("orders stored = %d, want 4 (rejected lists create none)", n)
	}
}

func TestListRules(t *testing.T) {
	h := newHarness(t, `list_rules:
  - { name: reject-big-list, priority: 1, match: { tot_no_orders: { gt: 2 } }, action: { type: reject_list, text: "List too large" } }
  - { name: slow-list, priority: 10, match: { list_id: { regex: "^SLOW-" } }, action: { type: delay_ack, delay_ms: 1000 } }
default_action: [ { type: ack } ]
`, aaplQuotes(nil), false)
	h.handle(three("BIG"))
	h.expect(MsgType, "N", ListOrderStatus, "7", tag.ListStatusText, "List too large")

	h.handle(list("SLOW-1", 1, entry{"S1", enum.Side_BUY, "10", ""}))
	h.none()
	h.advance(999 * time.Millisecond)
	h.none()
	h.advance(time.Millisecond)
	h.expect(MsgType, "N", ListStatusType, "1")
	h.expect(MsgType, "8", tag.ClOrdID, "S1", ExecType, "0")
}

func TestOrderRulesMatchListFields(t *testing.T) {
	h := newHarness(t, `order_rules:
  - { name: third, priority: 1, match: { list_seq_no: 3 }, actions: [ { type: reject, text: third } ] }
  - { name: singles, priority: 2, match: { in_list: false }, actions: [ { type: reject, text: single } ] }
default_action: [ { type: ack } ]
`, aaplQuotes(nil), false)
	h.handle(three("L"))
	h.expect(MsgType, "N")
	h.expect(tag.ClOrdID, "L-1", ExecType, "0")
	h.expect(tag.ClOrdID, "L-2", ExecType, "0")
	h.expect(tag.ClOrdID, "L-3", ExecType, "8", tag.Text, "third")

	h.handle(nos("D1", enum.Side_BUY, "1", enum.OrdType_MARKET))
	h.expect(tag.ClOrdID, "D1", ExecType, "8", tag.Text, "single")
}

func TestListStatusReporting(t *testing.T) {
	h := newHarness(t, `order_lists: { report: list_status, list_status_interval_ms: 100 }
default_action: [ { type: ack }, { type: fill, delay_ms: 250 } ]
`, aaplQuotes(nil), true)
	h.handle(list("L", 2, entry{"L-1", enum.Side_BUY, "10", ""}, entry{"L-2", enum.Side_SELL, "20", ""}))

	// Only ListStatus messages; each also goes to the monitor.
	byTarget := func() (client, mon []out) {
		for {
			select {
			case o := <-h.out:
				if o.session == monitor {
					mon = append(mon, o)
				} else {
					client = append(client, o)
				}
				continue
			case <-time.After(50 * time.Millisecond):
			}
			return
		}
	}
	client, mon := byTarget()
	if len(client) != 1 || get(client[0].m, ListStatusType) != "1" || len(mon) != 1 || get(mon[0].m, tag.MirrorSessionID) != client1.String() {
		t.Fatalf("after E: client %d, monitor %d messages", len(client), len(mon))
	}

	h.advance(100 * time.Millisecond) // acks changed the orders
	client, _ = byTarget()
	if len(client) != 1 || get(client[0].m, ListStatusType) != "3" || get(client[0].m, ListOrderStatus) != "3" {
		t.Fatalf("first timed N: %v", client)
	}
	h.advance(100 * time.Millisecond) // nothing changed: no N
	if client, _ = byTarget(); len(client) != 0 {
		t.Fatalf("N sent without changes: %v", client[0].m)
	}
	h.advance(50 * time.Millisecond) // fills at 250ms close the list
	client, _ = byTarget()
	if len(client) != 1 || get(client[0].m, ListStatusType) != "5" {
		t.Fatalf("final N: %v", client)
	}
	h.advance(time.Second)
	if client, _ = byTarget(); len(client) != 0 {
		t.Fatal("N after the list was done")
	}

	// ListStatusRequest is answered at once, outside the timer.
	st := liststatusrequest.New(field.NewListID("L"))
	h.handle(st)
	if client, _ = byTarget(); len(client) != 1 || get(client[0].m, MsgType) != "N" || get(client[0].m, ListStatusType) != "2" {
		t.Fatalf("M answer: %v", client)
	}
}

func TestListCancelAndStatusRequest(t *testing.T) {
	h := newHarness(t, `order_rules:
  - { name: first-fills, priority: 1, match: { list_seq_no: 1 }, actions: [ { type: fill } ] }
default_action: [ { type: ack } ]
`, aaplQuotes(nil), false)
	h.handle(three("L"))
	h.expect(MsgType, "N")
	h.expect(tag.ClOrdID, "L-1", ExecType, "F")
	h.expect(tag.ClOrdID, "L-2", ExecType, "0")
	h.expect(tag.ClOrdID, "L-3", ExecType, "0")

	h.handle(liststatusrequest.New(field.NewListID("L")))
	m := h.expect(MsgType, "N", ListStatusType, "2", ListOrderStatus, "3")
	if got := strings.Join(listEntries(t, m), ","); got != "L-1:2:100:0:0,L-2:0:0:200:0,L-3:0:0:300:0" {
		t.Fatalf("status entries %s", got)
	}

	h.handle(listcancelrequest.New(field.NewListID("L"), field.NewTransactTime(t0)))
	h.expect(tag.ClOrdID, "L-2", ExecType, "4", tag.Text, "List canceled")
	h.expect(tag.ClOrdID, "L-3", ExecType, "4")
	m = h.expect(MsgType, "N", ListStatusType, "2", ListOrderStatus, "6")
	if got := strings.Join(listEntries(t, m), ","); got != "L-1:2:100:0:0,L-2:4:0:0:200,L-3:4:0:0:300" {
		t.Fatalf("after cancel %s", got)
	}
	h.expect(MsgType, "N", ListStatusType, "5")

	h.handle(listcancelrequest.New(field.NewListID("NOPE"), field.NewTransactTime(t0)))
	h.expect(MsgType, "N", ListOrderStatus, "7", tag.ListStatusText, "Unknown ListID NOPE")
}

func TestMassCancel(t *testing.T) {
	quotes := aaplQuotes(nil)
	quotes["MSFT"] = quotes["AAPL"]
	h := newHarness(t, "default_action: [ { type: ack } ]\n", quotes, false)
	for _, o := range []struct{ id, sym string }{{"A1", "AAPL"}, {"M1", "MSFT"}, {"A2", "AAPL"}} {
		d := nos(o.id, enum.Side_BUY, "10", enum.OrdType_MARKET)
		d.SetSymbol(o.sym)
		h.handle(d)
		h.expect(ExecType, "0")
	}

	q := ordermasscancelrequest.New(field.NewClOrdID("MC1"), field.NewMassCancelRequestType(enum.MassCancelRequestType_CXLORDERSSECURITY), field.NewTransactTime(t0))
	q.SetSymbol("AAPL")
	h.handle(q)
	h.expect(MsgType, "r", tag.MassCancelResponse, "1", tag.TotalAffectedOrders, "2", tag.ClOrdID, "MC1")
	h.expect(tag.ClOrdID, "A1", ExecType, "4")
	h.expect(tag.ClOrdID, "A2", ExecType, "4")

	all := ordermasscancelrequest.New(field.NewClOrdID("MC2"), field.NewMassCancelRequestType(enum.MassCancelRequestType_CXLALLORDERS), field.NewTransactTime(t0))
	h.handle(all)
	h.expect(MsgType, "r", tag.MassCancelResponse, "7", tag.TotalAffectedOrders, "1")
	h.expect(tag.ClOrdID, "M1", ExecType, "4")

	h.handle(all)
	h.expect(MsgType, "r", tag.TotalAffectedOrders, "0")

	bad := ordermasscancelrequest.New(field.NewClOrdID("MC3"), field.NewMassCancelRequestType(enum.MassCancelRequestType_CXLORDERSPRODUCT), field.NewTransactTime(t0))
	h.handle(bad)
	h.expect(MsgType, "r", tag.MassCancelResponse, "0", tag.MassCancelRejectReason, "0")

	unknown := ordermasscancelrequest.New(field.NewClOrdID("MC4"), field.NewMassCancelRequestType(enum.MassCancelRequestType_CXLORDERSSECURITY), field.NewTransactTime(t0))
	unknown.SetSymbol("NOPE")
	h.handle(unknown)
	h.expect(MsgType, "r", tag.MassCancelResponse, "0", tag.MassCancelRejectReason, "1")
}

func TestListsDisabled(t *testing.T) {
	h := newHarness(t, "order_lists: { enabled: false }\n", aaplQuotes(nil), false)
	err := h.e.handle(wire(three("L").ToMessage()), client1)
	if err == nil || !err.IsBusinessReject() || err.RejectReason() != 3 {
		t.Fatalf("want BusinessMessageReject reason 3, got %v", err)
	}
}
