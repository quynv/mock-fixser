package order

import (
	"errors"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/quynv/mock-fixser/internal/config"
)

const rulesBase = `
server:
  fix:
    sessions: [ { sender_comp_id: MOCKSRV, target_comp_id: CLIENT1 } ]
symbols:
  - { symbol: AAPL, initial_price: 230, min: 200, max: 260, tick_size: 0.01, spread: 0.02, speed: { interval_ms: 100 } }
`

func parseRules(t *testing.T, rules string) *RuleSet {
	t.Helper()
	cfg, err := config.ParseWith([]byte(rulesBase+rules), Validate)
	if err != nil {
		t.Fatal(err)
	}
	rs, errs := Compile(cfg)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	return rs
}

func req(mut func(*Request)) Request {
	r := Request{MsgType: "D", CompID: "CLIENT1", Symbol: "AAPL", Side: "1", OrdType: "1",
		ClOrdID: "C1", Qty: decimal.NewFromInt(100), Tag: func(int) (string, bool) { return "", false }}
	if mut != nil {
		mut(&r)
	}
	return r
}

func TestRuleMatching(t *testing.T) {
	rs := parseRules(t, `order_rules:
  - name: reject-large-qty
    priority: 1
    match: { qty: { gt: 1000000 } }
    actions: [ { type: reject, reason: 3, text: "Order exceeds limit" } ]
  - name: timeout-test
    priority: 5
    match: { cl_ord_id: { regex: "^TIMEOUT-" } }
    actions: [ { type: ignore } ]
  - name: vip
    priority: 7
    match: { tags: { "9001": VIP }, side: [SELL] }
    actions: [ { type: ack } ]
  - name: partial-then-fill-aapl
    priority: 10
    match: { symbol: AAPL, side: BUY, ord_type: MARKET, comp_id: CLIENT1 }
    actions:
      - { type: ack, delay_ms: 50 }
      - { type: partial_fill, ratio: 0.4, delay_ms: 300 }
      - { type: fill, delay_ms: 800 }
  - name: slow-cancel
    priority: 20
    message_type: F
    match: { symbol: AAPL }
    actions: [ { type: ack, delay_ms: 500 } ]
default_action:
  - { type: ack, delay_ms: 20 }
  - { type: fill, delay_ms: 100 }
`)
	tests := []struct {
		name string
		r    Request
		want string
	}{
		{"priority wins", req(func(r *Request) { r.Qty = decimal.NewFromInt(2000000) }), "reject-large-qty"},
		{"regex", req(func(r *Request) { r.ClOrdID = "TIMEOUT-1" }), "timeout-test"},
		{"chain", req(nil), "partial-then-fill-aapl"},
		{"side list and tag", req(func(r *Request) {
			r.Side = "2"
			r.Tag = func(tag int) (string, bool) { return "VIP", tag == 9001 }
		}), "vip"},
		{"tag missing falls through", req(func(r *Request) { r.Side = "2" }), DefaultRuleName},
		{"other comp id", req(func(r *Request) { r.CompID = "CLIENT2" }), DefaultRuleName},
		{"cancel rule", req(func(r *Request) { r.MsgType = "F" }), "slow-cancel"},
		{"replace has no rule", req(func(r *Request) { r.MsgType = "G" }), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := rs.Match(tt.r); got != tt.want {
				t.Fatalf("matched %q, want %q", got, tt.want)
			}
		})
	}

	_, steps := rs.Match(req(nil))
	if len(steps) != 3 || steps[1].Type != "partial_fill" || !steps[1].Ratio.Equal(decimal.RequireFromString("0.4")) ||
		steps[2].Delay.Milliseconds() != 800 {
		t.Fatalf("steps = %+v", steps)
	}
}

func TestProbabilityIsStableAndProportional(t *testing.T) {
	rs := parseRules(t, `order_rules:
  - { name: sometimes, priority: 1, match: { probability: 0.2 }, actions: [ { type: reject } ] }
`)
	hits := 0
	for i := range 10000 {
		r := req(func(r *Request) {
			r.ClOrdID = "C" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + decimal.NewFromInt(int64(i)).String()
		})
		a, _ := rs.Match(r)
		b, _ := rs.Match(r)
		if a != b {
			t.Fatal("same order matched differently")
		}
		if a == "sometimes" {
			hits++
		}
	}
	if hits < 1800 || hits > 2200 {
		t.Fatalf("probability 0.2 matched %d of 10000", hits)
	}
}

func TestBuiltinDefaultAction(t *testing.T) {
	rs := parseRules(t, "")
	name, steps := rs.Match(req(nil))
	if name != DefaultRuleName || len(steps) != 2 || steps[0].Type != "ack" || steps[1].Type != "fill" {
		t.Fatalf("got %s %+v", name, steps)
	}
}

func TestRuleValidationErrors(t *testing.T) {
	tests := []struct {
		name, rules, path string
		line              int
	}{
		{"unknown field", `order_rules:
  - { name: r, priority: 1, match: { colour: red }, actions: [ { type: ack } ] }
`, "order_rules[0].match.colour", 8},
		{"bad regex", `order_rules:
  - name: r
    priority: 1
    match: { cl_ord_id: { regex: "(" } }
    actions: [ { type: ack } ]
`, "order_rules[0].match.cl_ord_id", 10},
		{"unknown side", `order_rules:
  - { name: r, priority: 1, match: { side: UP }, actions: [ { type: ack } ] }
`, "order_rules[0].match.side", 8},
		{"side outside BUY/SELL", `order_rules:
  - { name: r, priority: 1, match: { side: [BUY, SELL_SHORT] }, actions: [ { type: ack } ] }
`, "order_rules[0].match.side", 8},
		{"raw FIX side", `order_rules:
  - { name: r, priority: 1, match: { side: "1" }, actions: [ { type: ack } ] }
`, "order_rules[0].match.side", 8},
		{"unknown ord_type", `order_rules:
  - { name: r, priority: 1, match: { ord_type: PEGGED }, actions: [ { type: ack } ] }
`, "order_rules[0].match.ord_type", 8},
		{"regex on side", `order_rules:
  - { name: r, priority: 1, match: { side: { regex: "B.*" } }, actions: [ { type: ack } ] }
`, "order_rules[0].match.side", 8},
		{"unknown list match field", `list_rules:
  - { name: l, priority: 1, match: { symbol: AAPL }, action: { type: reject_list } }
`, "list_rules[0].match.symbol", 8},
		{"unknown list action param", `list_rules:
  - { name: l, priority: 1, action: { type: delay_ack, text: x } }
`, "list_rules[0].action.text", 8},
		{"unknown list action", `list_rules:
  - { name: l, priority: 1, action: { type: explode } }
`, "list_rules[0].action.type", 8},
		{"duplicate list rule", `list_rules:
  - { name: l, priority: 1, action: { type: reject_list } }
  - { name: l, priority: 2, action: { type: reject_list } }
`, "list_rules[1].name", 9},
		{"bad report mode", `order_lists: { report: sometimes }
`, "order_lists.report", 7},
		{"in_list not bool", `order_rules:
  - { name: r, priority: 1, match: { in_list: yes please }, actions: [ { type: ack } ] }
`, "order_rules[0].match.in_list", 8},
		{"bad operator", `order_rules:
  - { name: r, priority: 1, match: { qty: { above: 5 } }, actions: [ { type: ack } ] }
`, "order_rules[0].match.qty", 8},
		{"fill on cancel", `order_rules:
  - { name: r, priority: 1, message_type: F, actions: [ { type: fill } ] }
`, "order_rules[0].actions[0].type", 8},
		{"unknown param", `default_action: [ { type: ack, delay: 5 } ]
`, "default_action[0].delay", 7},
		{"partial needs ratio or qty", `default_action: [ { type: partial_fill } ]
`, "default_action[0]", 7},
		{"ratio above 1", `default_action: [ { type: partial_fill, ratio: 1.5 } ]
`, "default_action[0].ratio", 7},
		{"fixed needs price", `default_action: [ { type: fill, price_mode: fixed } ]
`, "default_action[0].price", 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.ParseWith([]byte(rulesBase+tt.rules), Validate)
			var verr *config.ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("want validation error, got %v", err)
			}
			for _, fe := range verr.Errors {
				if fe.Path == tt.path && fe.Line == tt.line {
					return
				}
			}
			t.Fatalf("want error at %s line %d, got:\n%v", tt.path, tt.line, err)
		})
	}
}
