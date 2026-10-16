package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quickfixgo/quickfix"

	"github.com/quynv/mock-fixser/internal/clock"
	"github.com/quynv/mock-fixser/internal/config"
	"github.com/quynv/mock-fixser/internal/fixserver"
	"github.com/quynv/mock-fixser/internal/marketdata"
	"github.com/quynv/mock-fixser/internal/order"
	"github.com/quynv/mock-fixser/internal/sched"
)

const testYAML = `# mockfix test config
version: 4
server:
  fix:
    listen: 127.0.0.1:0
    sessions:
      - { sender_comp_id: MOCKSRV, target_comp_id: CLIENT1 }
  monitor: { enabled: true }
symbols:
  # the main pair
  - { symbol: EURUSD, initial_price: 1.085, min: 1.07, max: 1.10, tick_size: 0.00001, spread: 0.0001, speed: { ticks_per_second: 5 } }
order_rules:
  - name: reject-large-qty
    priority: 1
    match: { qty: { gt: 1000000 } }
    actions: [ { type: reject, reason: 3, text: "Order exceeds limit" } ]
  - name: partial-then-fill
    priority: 10
    match: { symbol: EURUSD, side: BUY }
    actions:
      - { type: ack, delay_ms: 50 }
      - { type: partial_fill, ratio: 0.4, count: 2, delay_ms: 300 }
      - { type: fill, delay_ms: 800 }
`

type env struct {
	t     *testing.T
	srv   *httptest.Server
	store *config.Store
	path  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	t.Chdir("../..") // spec/FIX44.xml, the default data_dictionary
	dir := t.TempDir()
	path := filepath.Join(dir, "mock.yaml")
	if err := os.WriteFile(path, []byte(testYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	store, err := config.Open(path, config.Options{Logger: log, Validators: []config.Validator{order.Validate}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Current()
	md, err := marketdata.NewEngine(cfg.Symbols, clock.Real{}, log, func(marketdata.Quote) {})
	if err != nil {
		t.Fatal(err)
	}
	orders, err := order.NewEngine(cfg, md, sched.New(clock.Real{}), log,
		func(*quickfix.Message, quickfix.SessionID) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(orders.Close)
	fs, err := fixserver.New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fs.Stop) // unregisters the sessions from quickfixgo's global registry
	store.Subscribe(func(c config.Change) {
		if err := md.ApplySymbols(c.New.Symbols); err != nil {
			t.Error(err)
		}
	})
	srv := httptest.NewServer(Handler(Deps{Store: store, Market: md, Orders: orders, Server: fs, Log: log}))
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, store: store, path: path}
}

// do sends a request and decodes a JSON answer into out (if not nil).
func (e *env) do(method, path, body string, headers map[string]string, out any) *http.Response {
	e.t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if body != "" && !strings.HasPrefix(strings.TrimSpace(body), "{") && !strings.HasPrefix(strings.TrimSpace(body), "[") {
		req.Header.Set("Content-Type", "application/yaml")
	} else if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			e.t.Fatalf("%s %s: %v\n%s", method, path, err, data)
		}
	}
	return res
}

func (e *env) status(method, path, body string, headers map[string]string, want int) *apiError {
	e.t.Helper()
	var ae apiError
	res := e.do(method, path, body, headers, &ae)
	if res.StatusCode != want {
		e.t.Fatalf("%s %s = %d, want %d (%+v)", method, path, res.StatusCode, want, ae)
	}
	return &ae
}

func TestConfigEndpoints(t *testing.T) {
	e := newEnv(t)
	var cfg config.Config
	res := e.do("GET", "/api/config", "", nil, &cfg)
	if res.Header.Get("ETag") != `"4"` || cfg.Version != 4 || cfg.Symbols[0].Symbol != "EURUSD" {
		t.Fatalf("GET /api/config: etag %s, %+v", res.Header.Get("ETag"), cfg)
	}

	// PUT needs If-Match, and the right one.
	newYAML := strings.Replace(testYAML, "spread: 0.0001", "spread: 0.0002", 1)
	e.status("PUT", "/api/config", newYAML, nil, http.StatusPreconditionRequired)
	e.status("PUT", "/api/config", newYAML, map[string]string{"If-Match": `"3"`}, http.StatusConflict)

	var applied config.Result
	res = e.do("PUT", "/api/config", newYAML, map[string]string{"If-Match": `"4"`}, &applied)
	if res.StatusCode != 200 || applied.Version != 5 || applied.Diff.Sections[0] != "symbols" || res.Header.Get("ETag") != `"5"` {
		t.Fatalf("PUT: %d %+v", res.StatusCode, applied)
	}

	// Validation errors carry path and line.
	bad := strings.Replace(testYAML, "max: 1.10", "max: 1.00", 1)
	ae := e.status("PUT", "/api/config", bad, map[string]string{"If-Match": `"5"`}, http.StatusUnprocessableEntity)
	if ae.Code != "invalid_config" || len(ae.Details) == 0 || ae.Details[0].Line == 0 {
		t.Fatalf("validation error: %+v", ae)
	}
	ae = e.status("POST", "/api/config/validate", bad, nil, http.StatusUnprocessableEntity)
	if ae.Details[0].Path == "" {
		t.Fatalf("validate: %+v", ae)
	}
	var v map[string]any
	if res := e.do("POST", "/api/config/validate", testYAML, nil, &v); res.StatusCode != 200 || v["valid"] != true {
		t.Fatalf("validate ok: %d %v", res.StatusCode, v)
	}

	// JSON body.
	var cur config.Config
	e.do("GET", "/api/config", "", nil, &cur)
	cur.Symbols[0].Spread = 0.0003
	js, _ := json.Marshal(cur)
	var raw json.RawMessage
	if res := e.do("PUT", "/api/config", string(js), map[string]string{"If-Match": `"5"`}, &raw); res.StatusCode != 200 {
		t.Fatalf("PUT JSON: %d %s", res.StatusCode, raw)
	}
	json.Unmarshal(raw, &applied)
	if applied.Version != 6 {
		t.Fatalf("PUT JSON version %d", applied.Version)
	}

	var hist []config.HistoryEntry
	e.do("GET", "/api/config/history", "", nil, &hist)
	if len(hist) != 2 || hist[0].Version != 5 {
		t.Fatalf("history %+v", hist)
	}
	if res := e.do("POST", "/api/config/rollback/4", "", nil, &applied); res.StatusCode != 200 || applied.Version != 7 {
		t.Fatalf("rollback: %d %+v", res.StatusCode, applied)
	}
	if e.store.Current().Symbols[0].Spread != 0.0001 {
		t.Fatal("rollback did not restore the spread")
	}
	e.status("POST", "/api/config/rollback/99", "", nil, http.StatusNotFound)

	res = e.do("GET", "/api/schema", "", nil, nil)
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Type"), "schema+json") {
		t.Fatalf("schema: %d", res.StatusCode)
	}
}

func TestSymbolEndpointsKeepComments(t *testing.T) {
	e := newEnv(t)
	gbp := `{"symbol":"GBPUSD","initial_price":1.27,"min":1.2,"max":1.4,"tick_size":0.00001,"spread":0.0001,"speed":{"interval_ms":200}}`
	var res config.Result
	if r := e.do("POST", "/api/symbols", gbp, nil, &res); r.StatusCode != http.StatusCreated || res.Version != 5 {
		t.Fatalf("POST symbol: %d %+v", r.StatusCode, res)
	}
	e.status("POST", "/api/symbols", gbp, nil, http.StatusConflict)

	var list []symbolView
	e.do("GET", "/api/symbols", "", nil, &list)
	if len(list) != 2 || list[1].Symbol.Symbol != "GBPUSD" || list[1].Runtime == nil || list[1].Runtime.Mid != 1.27 {
		t.Fatalf("symbols %+v", list)
	}

	// Invalid edits are rejected with the usual validation errors.
	e.status("PUT", "/api/symbols/GBPUSD", strings.Replace(gbp, `"max":1.4`, `"max":1.0`, 1), nil, http.StatusUnprocessableEntity)
	e.status("PUT", "/api/symbols/NOPE", gbp, nil, http.StatusBadRequest) // body names GBPUSD
	if r := e.do("DELETE", "/api/symbols/EURUSD", "", map[string]string{"If-Match": `"5"`}, &res); r.StatusCode != 200 {
		t.Fatalf("DELETE: %d", r.StatusCode)
	}
	e.status("DELETE", "/api/symbols/EURUSD", "", nil, http.StatusNotFound)

	data, _ := os.ReadFile(e.path)
	if s := string(data); !strings.Contains(s, "# mockfix test config") || !strings.Contains(s, "version: 6") || strings.Contains(s, "EURUSD:") {
		t.Fatalf("file after edits:\n%s", s)
	}
}

func TestRuntimeCommands(t *testing.T) {
	e := newEnv(t)
	var st marketdata.SymbolState
	e.do("POST", "/api/symbols/EURUSD/pause", "", nil, &st)
	if !st.Paused {
		t.Fatalf("pause: %+v", st)
	}
	e.do("POST", "/api/symbols/EURUSD/price", `{"mid":1.09}`, nil, &st)
	if st.Mid != 1.09 {
		t.Fatalf("price: %+v", st)
	}
	e.do("POST", "/api/symbols/EURUSD/spike", `{"percent":1,"direction":"down"}`, nil, &st)
	if st.Mid != 1.0791 {
		t.Fatalf("spike: %+v", st)
	}
	e.status("POST", "/api/symbols/EURUSD/price", `{"mid":5}`, nil, http.StatusBadRequest)
	e.status("POST", "/api/symbols/EURUSD/spike", `{"percent":1,"direction":"sideways"}`, nil, http.StatusBadRequest)
	e.status("POST", "/api/symbols/NOPE/pause", "", nil, http.StatusNotFound)
	if e.store.Current().Version != 4 {
		t.Fatal("runtime commands changed the config")
	}
}

func TestRuleEndpointsAndTester(t *testing.T) {
	e := newEnv(t)
	var res config.Result
	rule := `{"name":"vip","priority":5,"match":{"tags":{"9001":"VIP"}},"actions":[{"type":"ack"},{"type":"fill","delay_ms":10}]}`
	if r := e.do("POST", "/api/rules", rule, nil, &res); r.StatusCode != http.StatusCreated {
		t.Fatalf("POST rule: %d", r.StatusCode)
	}
	e.status("POST", "/api/rules", `{"name":"x","priority":7,"match":{"side":"SELL_SHORT"},"actions":[{"type":"ack"}]}`, nil, http.StatusUnprocessableEntity)

	var out struct {
		Rule    string     `json:"rule"`
		Actions []testStep `json:"actions"`
		TotalMs int64      `json:"total_ms"`
	}
	e.do("POST", "/api/rules/test", `{"symbol":"EURUSD","side":"BUY","qty":"100","cl_ord_id":"C1","comp_id":"CLIENT1"}`, nil, &out)
	if out.Rule != "partial-then-fill" || len(out.Actions) != 3 || out.Actions[1].AtMs != 350 || out.Actions[2].AtMs != 1450 || out.TotalMs != 1450 {
		t.Fatalf("tester: %+v", out)
	}
	e.do("POST", "/api/rules/test", `{"symbol":"EURUSD","side":"SELL","qty":"1","tags":{"9001":"VIP"}}`, nil, &out)
	if out.Rule != "vip" {
		t.Fatalf("tag rule: %+v", out)
	}

	// Drag and drop: vip first, then the others.
	e.do("PUT", "/api/rules/order", `{"order":["vip","partial-then-fill","reject-large-qty"]}`, nil, &res)
	var rules struct {
		Rules []config.Rule `json:"rules"`
	}
	e.do("GET", "/api/rules", "", nil, &rules)
	if rules.Rules[0].Name != "vip" || rules.Rules[0].Priority != 10 || rules.Rules[2].Name != "reject-large-qty" {
		t.Fatalf("after reorder: %+v", rules.Rules)
	}
	e.do("POST", "/api/rules/test", `{"symbol":"EURUSD","side":"BUY","qty":"2000000","tags":{"9001":"VIP"}}`, nil, &out)
	if out.Rule != "vip" {
		t.Fatalf("priority after reorder: %s", out.Rule)
	}

	e.do("PUT", "/api/rules/vip", `{"priority":10,"match":{"tags":{"9001":"GOLD"}},"actions":[{"type":"reject","text":"gold"}]}`, nil, &res)
	e.do("POST", "/api/rules/test", `{"symbol":"EURUSD","side":"SELL","qty":"1","tags":{"9001":"GOLD"}}`, nil, &out)
	if out.Rule != "vip" || out.Actions[0].Text != "gold" {
		t.Fatalf("after PUT rule: %+v", out)
	}
	e.do("DELETE", "/api/rules/vip", "", nil, &res)
	e.status("DELETE", "/api/rules/vip", "", nil, http.StatusNotFound)
}

func TestSessionsAndOrders(t *testing.T) {
	e := newEnv(t)
	var sessions []fixserver.SessionInfo
	e.do("GET", "/api/sessions", "", nil, &sessions)
	if len(sessions) != 2 || sessions[0].TargetCompID != "CLIENT1" || sessions[1].Kind != "monitor" || sessions[0].LoggedOn {
		t.Fatalf("sessions %+v", sessions)
	}
	e.status("POST", "/api/sessions/CLIENT1/logout", "", nil, http.StatusConflict)
	e.status("POST", "/api/sessions/NOPE/logout", "", nil, http.StatusNotFound)
	e.status("POST", "/api/sessions/CLIENT1/fault", "", nil, http.StatusNotImplemented)

	var orders []orderView
	if r := e.do("GET", "/api/orders?session=CLIENT1&status=open&limit=10", "", nil, &orders); r.StatusCode != 200 || len(orders) != 0 {
		t.Fatalf("orders: %d %+v", r.StatusCode, orders)
	}
	e.status("GET", "/api/orders?limit=x", "", nil, http.StatusBadRequest)

	res := e.do("GET", "/metrics", "", nil, nil)
	if res.StatusCode != 200 {
		t.Fatalf("/metrics %d", res.StatusCode)
	}
	e.status("GET", "/api/nope", "", nil, http.StatusNotFound)
}
