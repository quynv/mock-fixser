// Package order handles orders (D/F/G/H), the rule engine, action chains,
// slippage and order state (design doc, section 7).
package order

import (
	"fmt"
	"hash/fnv"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/quynv/mock-fixser/internal/config"
)

// Request is what rules match against: the fields of a D, F or G.
type Request struct {
	MsgType string // D, F or G
	CompID  string // the client's CompID
	Symbol  string
	Side    string // FIX value, e.g. "1"
	OrdType string // FIX value, e.g. "1"
	Account string
	ClOrdID string
	Qty     decimal.Decimal
	Price   *decimal.Decimal
	Tag     func(tag int) (string, bool)

	// Set for orders that arrive in a NewOrderList (section 7.6).
	InList    bool
	ListID    string
	ListSeqNo int
}

// ListRequest is what list_rules match against.
type ListRequest struct {
	ListID      string
	CompID      string
	TotNoOrders int
}

// ListAction is the outcome of list_rules: reject the list, or delay its
// acknowledgement.
type ListAction struct {
	Rule  string
	Type  string // "", reject_list or delay_ack
	Delay time.Duration
	Text  string
}

// Step is one compiled action of a chain (section 7.3).
type Step struct {
	Type      string
	Delay     time.Duration
	Ratio     decimal.Decimal // partial_fill
	Qty       decimal.Decimal // partial_fill
	Count     int             // partial_fill
	PriceMode string          // fill, partial_fill: market or fixed
	Price     decimal.Decimal // fixed price
	Reason    int             // reject, business_reject
	Text      string
}

type rule struct {
	name     string
	priority int
	msgType  string
	conds    []cond
	steps    []Step
}

// RuleSet is an immutable compiled order_rules + default_action +
// list_rules, with the order_lists settings.
type RuleSet struct {
	rules         []rule
	defaultAction []Step
	listRules     []listRule
	Lists         config.OrderLists
}

type listRule struct {
	name     string
	priority int
	conds    []listCond
	action   ListAction
}

// MatchList returns the first matching list rule's action, or an empty
// action when none matches.
func (rs *RuleSet) MatchList(r ListRequest) ListAction {
	for _, lr := range rs.listRules {
		ok := true
		for _, c := range lr.conds {
			if !c(r) {
				ok = false
				break
			}
		}
		if ok {
			return lr.action
		}
	}
	return ListAction{}
}

// DefaultRuleName names the outcome when no rule matches a D.
const DefaultRuleName = "default_action"

var builtinDefault = []Step{{Type: "ack"}, {Type: "fill", PriceMode: "market"}}

// Match returns the first matching rule by ascending priority. For D with
// no match it returns default_action; for F and G with no match it
// returns no steps, meaning accept immediately.
func (rs *RuleSet) Match(req Request) (name string, steps []Step) {
	for _, r := range rs.rules {
		if r.msgType != req.MsgType {
			continue
		}
		ok := true
		for _, c := range r.conds {
			if !c.match(r.name, req) {
				ok = false
				break
			}
		}
		if ok {
			return r.name, r.steps
		}
	}
	if req.MsgType == "D" {
		return DefaultRuleName, rs.defaultAction
	}
	return "", nil
}

// Validate is a config.Validator for order rules.
func Validate(cfg *config.Config) []config.FieldError {
	_, errs := Compile(cfg)
	return errs
}

// Compile builds a RuleSet. Every problem is reported with its path.
func Compile(cfg *config.Config) (*RuleSet, []config.FieldError) {
	var errs []config.FieldError
	add := func(path, format string, args ...any) {
		errs = append(errs, config.FieldError{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	rs := &RuleSet{}
	for i, r := range cfg.OrderRules {
		p := fmt.Sprintf("order_rules[%d]", i)
		mt := r.MessageType
		if mt == "" {
			mt = "D"
		}
		cr := rule{name: r.Name, priority: r.Priority, msgType: mt}
		keys := make([]string, 0, len(r.Match))
		for k := range r.Match {
			keys = append(keys, k)
		}
		sort.Strings(keys) // stable error order
		for _, k := range keys {
			c, err := compileCond(k, r.Match[k])
			if err != nil {
				add(p+".match."+k, "%v", err)
				continue
			}
			cr.conds = append(cr.conds, c)
		}
		cr.steps = compileSteps(p+".actions", mt, r.Actions, add)
		if mt != "D" && len(r.Actions) != 1 {
			add(p+".actions", "message_type %s takes exactly one action: ack (accept, optionally delayed), reject or ignore", mt)
		}
		rs.rules = append(rs.rules, cr)
	}
	sort.SliceStable(rs.rules, func(i, j int) bool { return rs.rules[i].priority < rs.rules[j].priority })

	rs.defaultAction = compileSteps("default_action", "D", cfg.DefaultAction, add)
	if len(cfg.DefaultAction) == 0 {
		rs.defaultAction = builtinDefault
	}

	rs.Lists = cfg.OrderLists
	for i, r := range cfg.ListRules {
		rs.listRules = append(rs.listRules, compileListRule(fmt.Sprintf("list_rules[%d]", i), r, add))
	}
	sort.SliceStable(rs.listRules, func(i, j int) bool { return rs.listRules[i].priority < rs.listRules[j].priority })
	return rs, errs
}

type listCond func(ListRequest) bool

func compileListRule(p string, r config.ListRule, add func(string, string, ...any)) listRule {
	lr := listRule{name: r.Name, priority: r.Priority, action: ListAction{Rule: r.Name, Type: r.Action.Type}}
	keys := make([]string, 0, len(r.Match))
	for k := range r.Match {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		kp := p + ".match." + k
		switch k {
		case "tot_no_orders":
			c, err := compileNum(k, r.Match[k])
			if err != nil {
				add(kp, "%v", err)
				continue
			}
			nc := c.(numCond)
			lr.conds = append(lr.conds, func(l ListRequest) bool {
				return nc.match("", Request{Qty: decimal.NewFromInt(int64(l.TotNoOrders))})
			})
		case "list_id", "comp_id":
			m, err := compileStr(r.Match[k], nil)
			if err != nil {
				add(kp, "%v", err)
				continue
			}
			if k == "list_id" {
				lr.conds = append(lr.conds, func(l ListRequest) bool { return m.ok(l.ListID) })
			} else {
				lr.conds = append(lr.conds, func(l ListRequest) bool { return m.ok(l.CompID) })
			}
		default:
			add(kp, "unknown list match field %q (use tot_no_orders, list_id, comp_id)", k)
		}
	}
	allowed := map[string]map[string]bool{"reject_list": {"text": true}, "delay_ack": {"delay_ms": true}}
	for k, v := range r.Action.Params {
		kp := p + ".action." + k
		if !allowed[r.Action.Type][k] {
			add(kp, "unknown parameter %q for action %s", k, r.Action.Type)
			continue
		}
		switch k {
		case "text":
			lr.action.Text, _ = v.(string)
		case "delay_ms":
			n, ok := asInt(v)
			if !ok || n < 0 {
				add(kp, "delay_ms must be a non-negative integer")
			}
			lr.action.Delay = time.Duration(n) * time.Millisecond
		}
	}
	return lr
}

var stepsAllowed = map[string]map[string]bool{
	"D": {"ack": true, "fill": true, "partial_fill": true, "reject": true, "cancel": true,
		"expire": true, "ignore": true, "business_reject": true},
	// F and G: accept (optionally delayed with ack), reject with 9, or no answer.
	"F": {"ack": true, "reject": true, "ignore": true},
	"G": {"ack": true, "reject": true, "ignore": true},
}

var stepParams = map[string]map[string]bool{
	"ack":             {"delay_ms": true},
	"fill":            {"delay_ms": true, "price_mode": true, "price": true},
	"partial_fill":    {"delay_ms": true, "price_mode": true, "price": true, "ratio": true, "qty": true, "count": true},
	"reject":          {"delay_ms": true, "reason": true, "text": true},
	"cancel":          {"delay_ms": true, "text": true},
	"expire":          {"delay_ms": true, "text": true},
	"ignore":          {"delay_ms": true},
	"business_reject": {"delay_ms": true, "reason": true, "text": true},
}

func compileSteps(path, msgType string, actions []config.Action, add func(string, string, ...any)) []Step {
	var steps []Step
	for i, a := range actions {
		p := fmt.Sprintf("%s[%d]", path, i)
		if !stepsAllowed[msgType][a.Type] {
			add(p+".type", "action %q is not allowed for message_type %s", a.Type, msgType)
			continue
		}
		st := Step{Type: a.Type, PriceMode: "market", Count: 1}
		for k, v := range a.Params {
			kp := p + "." + k
			if !stepParams[a.Type][k] {
				add(kp, "unknown parameter %q for action %s", k, a.Type)
				continue
			}
			switch k {
			case "delay_ms":
				n, ok := asInt(v)
				if !ok || n < 0 {
					add(kp, "delay_ms must be a non-negative integer")
				}
				st.Delay = time.Duration(n) * time.Millisecond
			case "reason":
				n, ok := asInt(v)
				if !ok || n < 0 {
					add(kp, "reason must be a non-negative integer")
				}
				st.Reason = n
			case "count":
				n, ok := asInt(v)
				if !ok || n < 1 {
					add(kp, "count must be an integer >= 1")
				}
				st.Count = n
			case "text":
				s, ok := v.(string)
				if !ok {
					add(kp, "text must be a string")
				}
				st.Text = s
			case "price_mode":
				s, _ := v.(string)
				if s != "market" && s != "fixed" {
					add(kp, "price_mode must be market or fixed (limit modes come with Limit orders)")
				}
				st.PriceMode = s
			case "ratio", "qty", "price":
				d, ok := asDecimal(v)
				if !ok || !d.IsPositive() {
					add(kp, "%s must be a positive number", k)
				}
				switch k {
				case "ratio":
					if d.GreaterThan(decimal.NewFromInt(1)) {
						add(kp, "ratio must be in (0, 1]")
					}
					st.Ratio = d
				case "qty":
					st.Qty = d
				case "price":
					st.Price = d
				}
			}
		}
		if a.Type == "partial_fill" && st.Ratio.IsZero() == st.Qty.IsZero() {
			add(p, "partial_fill needs exactly one of ratio or qty")
		}
		if st.PriceMode == "fixed" && st.Price.IsZero() {
			add(p+".price", "price_mode fixed needs a price")
		}
		steps = append(steps, st)
	}
	return steps
}

// cond is one compiled match condition; all of a rule's conds must match.
type cond interface {
	match(rule string, r Request) bool
}

type strField func(Request) string

var stringFields = map[string]strField{
	"symbol":    func(r Request) string { return r.Symbol },
	"side":      func(r Request) string { return r.Side },
	"ord_type":  func(r Request) string { return r.OrdType },
	"account":   func(r Request) string { return r.Account },
	"cl_ord_id": func(r Request) string { return r.ClOrdID },
	"comp_id":   func(r Request) string { return r.CompID },
	"list_id":   func(r Request) string { return r.ListID },
}

type condFunc func(Request) bool

func (f condFunc) match(_ string, r Request) bool { return f(r) }

// Rule values for side and ord_type, mapped to FIX values. The MVP accepts
// Market orders only, so MARKET is the only ord_type.
var (
	sideNames    = enumNames{"BUY": "1", "SELL": "2"}
	ordTypeNames = enumNames{"MARKET": "1"}
)

type enumNames map[string]string

func (n enumNames) allowed() string {
	keys := make([]string, 0, len(n))
	for k := range n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func compileCond(key string, v any) (cond, error) {
	switch key {
	case "qty", "price":
		return compileNum(key, v)
	case "probability":
		p, ok := asFloat(v)
		if !ok || p < 0 || p > 1 {
			return nil, fmt.Errorf("probability must be a number in [0, 1]")
		}
		return probCond(p), nil
	case "list_seq_no":
		c, err := compileNum(key, v)
		if err != nil {
			return nil, err
		}
		nc := c.(numCond)
		return condFunc(func(r Request) bool {
			return r.InList && nc.match("", Request{Qty: decimal.NewFromInt(int64(r.ListSeqNo))})
		}), nil
	case "in_list":
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("in_list must be true or false")
		}
		return condFunc(func(r Request) bool { return r.InList == b }), nil
	case "tags":
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf(`tags must be a map such as { "9001": "VIP" }`)
		}
		var tc tagsCond
		for k, tv := range m {
			tag, err := strconv.Atoi(k)
			if err != nil || tag <= 0 {
				return nil, fmt.Errorf("tag %q is not a tag number", k)
			}
			sc, err := compileStr(tv, nil)
			if err != nil {
				return nil, fmt.Errorf("tag %s: %v", k, err)
			}
			tc = append(tc, tagCond{tag, sc})
		}
		return tc, nil
	}
	get, ok := stringFields[key]
	if !ok {
		return nil, fmt.Errorf("unknown match field %q", key)
	}
	var names enumNames
	switch key {
	case "side":
		names = sideNames
	case "ord_type":
		names = ordTypeNames
	}
	sc, err := compileStr(v, names)
	if err != nil {
		return nil, err
	}
	return fieldCond{get, sc}, nil
}

// strMatch matches equals, in-list or regex.
type strMatch struct {
	values []string
	re     *regexp.Regexp
}

func (m strMatch) ok(s string) bool {
	if m.re != nil {
		return m.re.MatchString(s)
	}
	return slices.Contains(m.values, s)
}

// compileStr compiles a value, a list or { regex }. With names, only the
// listed names are accepted (no regex).
func compileStr(v any, names enumNames) (strMatch, error) {
	mapName := func(s string) (string, error) {
		if names == nil {
			return s, nil
		}
		if fix, ok := names[s]; ok {
			return fix, nil
		}
		return "", fmt.Errorf("unknown value %q (allowed: %s)", s, names.allowed())
	}
	switch x := v.(type) {
	case map[string]any:
		if names != nil {
			return strMatch{}, fmt.Errorf("expected a value or a list of %s", names.allowed())
		}
		re, ok := x["regex"]
		if !ok || len(x) != 1 {
			return strMatch{}, fmt.Errorf("expected a value, a list, or { regex: ... }")
		}
		s, _ := re.(string)
		r, err := regexp.Compile(s)
		if err != nil {
			return strMatch{}, fmt.Errorf("invalid regex: %v", err)
		}
		return strMatch{re: r}, nil
	case []any:
		var m strMatch
		for _, e := range x {
			s, err := mapName(fmt.Sprint(e))
			if err != nil {
				return strMatch{}, err
			}
			m.values = append(m.values, s)
		}
		return m, nil
	default:
		s, err := mapName(fmt.Sprint(x))
		if err != nil {
			return strMatch{}, err
		}
		return strMatch{values: []string{s}}, nil
	}
}

type fieldCond struct {
	get strField
	m   strMatch
}

func (c fieldCond) match(_ string, r Request) bool { return c.m.ok(c.get(r)) }

type tagCond struct {
	tag int
	m   strMatch
}

type tagsCond []tagCond

func (c tagsCond) match(_ string, r Request) bool {
	for _, tc := range c {
		v, ok := r.Tag(tc.tag)
		if !ok || !tc.m.ok(v) {
			return false
		}
	}
	return true
}

type numCond struct {
	price bool
	ops   []numOp
}

type numOp struct {
	op string
	v  decimal.Decimal
}

func compileNum(key string, v any) (cond, error) {
	c := numCond{price: key == "price"}
	if d, ok := asDecimal(v); ok {
		c.ops = []numOp{{"eq", d}}
		return c, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a number or a map of gt/gte/lt/lte/eq", key)
	}
	for op, x := range m {
		switch op {
		case "gt", "gte", "lt", "lte", "eq":
		default:
			return nil, fmt.Errorf("unknown operator %q (use gt, gte, lt, lte, eq)", op)
		}
		d, ok := asDecimal(x)
		if !ok {
			return nil, fmt.Errorf("%s.%s must be a number", key, op)
		}
		c.ops = append(c.ops, numOp{op, d})
	}
	return c, nil
}

func (c numCond) match(_ string, r Request) bool {
	val := r.Qty
	if c.price {
		if r.Price == nil {
			return false
		}
		val = *r.Price
	}
	for _, o := range c.ops {
		cmp := val.Cmp(o.v)
		ok := map[string]bool{"gt": cmp > 0, "gte": cmp >= 0, "lt": cmp < 0, "lte": cmp <= 0, "eq": cmp == 0}[o.op]
		if !ok {
			return false
		}
	}
	return true
}

// probCond matches a fixed share of orders. The draw hashes the rule name,
// CompID and ClOrdID, so the same order always gets the same outcome and
// the rule tester agrees with live execution (section 7.2).
type probCond float64

func (p probCond) match(rule string, r Request) bool {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s|%s|%s", rule, r.CompID, r.ClOrdID)
	return float64(h.Sum64()>>11)/float64(1<<53) < float64(p)
}

func asInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case uint64:
		return int(x), true
	case float64:
		if x == math.Trunc(x) {
			return int(x), true
		}
	}
	return 0, false
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func asDecimal(v any) (decimal.Decimal, bool) {
	switch x := v.(type) {
	case int, int64, uint64, float64:
		f, _ := asFloat(x)
		return decimal.NewFromFloat(f), true
	case string:
		d, err := decimal.NewFromString(x)
		return d, err == nil
	}
	return decimal.Decimal{}, false
}
