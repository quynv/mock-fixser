// Package admin serves the REST API for configuration and control commands
// (design doc, section 10). Realtime data goes over FIX through /ws.
package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/quickfixgo/quickfix"
	"github.com/shopspring/decimal"
	"gopkg.in/yaml.v3"

	"github.com/quynv/mock-fixser/internal/config"
	"github.com/quynv/mock-fixser/internal/fixserver"
	"github.com/quynv/mock-fixser/internal/marketdata"
	"github.com/quynv/mock-fixser/internal/metrics"
	"github.com/quynv/mock-fixser/internal/order"
	"github.com/quynv/mock-fixser/schema"
)

// Deps are the components the API reads and controls.
type Deps struct {
	Store  *config.Store
	Market *marketdata.Engine
	Orders *order.Engine
	Server *fixserver.Server
	// WS serves /ws (the FIX gateway hub); nil disables it.
	WS http.Handler
	// UI holds the built Web UI served at /; nil disables it.
	UI fs.FS
	// Pprof serves net/http/pprof under /debug/pprof/.
	Pprof bool
	Log   *slog.Logger
}

type api struct{ Deps }

// Handler returns the HTTP handler for /api, /ws, /healthz and /metrics.
func Handler(d Deps) http.Handler {
	a := &api{d}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", a.getConfig)
	mux.HandleFunc("GET /api/config.yaml", a.getConfigYAML)
	mux.HandleFunc("PUT /api/config", a.putConfig)
	mux.HandleFunc("POST /api/config/validate", a.validateConfig)
	mux.HandleFunc("GET /api/config/history", a.history)
	mux.HandleFunc("POST /api/config/rollback/{version}", a.rollback)
	mux.HandleFunc("GET /api/schema", a.getSchema)

	mux.HandleFunc("GET /api/symbols", a.listSymbols)
	mux.HandleFunc("POST /api/symbols", a.addSymbol)
	mux.HandleFunc("GET /api/symbols/{symbol}", a.getSymbol)
	mux.HandleFunc("PUT /api/symbols/{symbol}", a.putSymbol)
	mux.HandleFunc("DELETE /api/symbols/{symbol}", a.deleteSymbol)
	mux.HandleFunc("POST /api/symbols/{symbol}/pause", a.pause)
	mux.HandleFunc("POST /api/symbols/{symbol}/resume", a.resume)
	mux.HandleFunc("POST /api/symbols/{symbol}/price", a.setPrice)
	mux.HandleFunc("POST /api/symbols/{symbol}/spike", a.spike)

	mux.HandleFunc("GET /api/rules", a.listRules)
	mux.HandleFunc("POST /api/rules", a.addRule)
	mux.HandleFunc("PUT /api/rules/order", a.reorderRules)
	mux.HandleFunc("PUT /api/rules/{name}", a.putRule)
	mux.HandleFunc("DELETE /api/rules/{name}", a.deleteRule)
	mux.HandleFunc("POST /api/rules/test", a.testRule)

	mux.HandleFunc("GET /api/sessions", a.sessions)
	mux.HandleFunc("POST /api/sessions/{id}/logout", a.logout)
	mux.HandleFunc("POST /api/sessions/{id}/fault", a.fault)
	mux.HandleFunc("GET /api/orders", a.orders)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.Handle("GET /metrics", metrics.Handler())
	if d.WS != nil {
		mux.Handle("GET /ws", d.WS)
	}
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint", nil)
	})
	if d.Pprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	if d.UI != nil {
		// No method in the pattern: "GET /" would conflict with "/api/".
		mux.Handle("/", spa(d.UI))
	}
	return mux
}

// apiError is the error body of every endpoint: { code, message, details[] }.
type apiError struct {
	Code    string              `json:"code"`
	Message string              `json:"message"`
	Details []config.FieldError `json:"details"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // session IDs contain ->
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string, details []config.FieldError) {
	if details == nil {
		details = []config.FieldError{}
	}
	writeJSON(w, status, apiError{Code: code, Message: msg, Details: details})
}

// writeErr maps package errors to HTTP statuses.
func writeErr(w http.ResponseWriter, err error) {
	var verr *config.ValidationError
	switch {
	case errors.As(err, &verr):
		writeError(w, http.StatusUnprocessableEntity, "invalid_config", "config is invalid", verr.Errors)
	case errors.Is(err, config.ErrVersionConflict):
		writeError(w, http.StatusConflict, "version_conflict", err.Error(), nil)
	case errors.Is(err, config.ErrNotFound), errors.Is(err, config.ErrUnknownVersion), errors.Is(err, marketdata.ErrUnknownSymbol):
		writeError(w, http.StatusNotFound, "not_found", err.Error(), nil)
	case errors.Is(err, config.ErrAlreadyExists):
		writeError(w, http.StatusConflict, "already_exists", err.Error(), nil)
	case errors.Is(err, errBadRequest):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), nil)
	}
}

var errBadRequest = errors.New("bad request")

func badRequest(format string, args ...any) error {
	return &wrapped{errBadRequest, fmt.Sprintf(format, args...)}
}

type wrapped struct {
	base error
	msg  string
}

func (e *wrapped) Error() string { return e.msg }
func (e *wrapped) Unwrap() error { return e.base }

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}

func etag(version int) string { return `"` + strconv.Itoa(version) + `"` }

// ifMatch reads If-Match as a config version; 0 means absent.
func ifMatch(r *http.Request) (int, error) {
	h := strings.TrimSpace(r.Header.Get("If-Match"))
	if h == "" {
		return 0, nil
	}
	v, err := strconv.Atoi(strings.Trim(strings.TrimPrefix(h, "W/"), `"`))
	if err != nil || v <= 0 {
		return 0, badRequest("If-Match must be a config version, got %q", h)
	}
	return v, nil
}

// applied answers a change with the new version and its diff.
func (a *api) applied(w http.ResponseWriter, status int, res *config.Result) {
	w.Header().Set("ETag", etag(res.Version))
	writeJSON(w, status, res)
}

func (a *api) getConfig(w http.ResponseWriter, _ *http.Request) {
	cfg := a.Store.Current()
	w.Header().Set("ETag", etag(cfg.Version))
	writeJSON(w, http.StatusOK, cfg)
}

func (a *api) getConfigYAML(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("ETag", etag(a.Store.Current().Version))
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(a.Store.Raw())
}

// readConfigBody returns the body as YAML. JSON bodies are converted.
func readConfigBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if !strings.Contains(r.Header.Get("Content-Type"), "json") {
		return body, nil
	}
	v, err := parseJSON(body)
	if err != nil {
		return nil, err
	}
	return yaml.Marshal(v)
}

// parseJSON decodes JSON for conversion to YAML: integers stay exact and
// null values are dropped (null means absent).
func parseJSON(data []byte) (any, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, badRequest("invalid JSON body: %v", err)
	}
	return normalize(v), nil
}

func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if e == nil {
				delete(x, k)
				continue
			}
			x[k] = normalize(e)
		}
	case []any:
		for i, e := range x {
			x[i] = normalize(e)
		}
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n
		}
		f, _ := x.Float64()
		return f
	}
	return v
}

// decodeObject reads a JSON object body, keeping only the fields sent, so
// a partial edit does not write zero values for absent fields.
func decodeObject(r *http.Request) (map[string]any, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	v, err := parseJSON(body)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, badRequest("body must be a JSON object")
	}
	return m, nil
}

func (a *api) putConfig(w http.ResponseWriter, r *http.Request) {
	v, err := ifMatch(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if v == 0 {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", "PUT /api/config needs If-Match with the config version", nil)
		return
	}
	data, err := readConfigBody(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	res, err := a.Store.Apply(data, config.ApplyOptions{IfVersion: v, WriteFile: true, Source: "api"})
	if err != nil {
		writeErr(w, err)
		return
	}
	a.applied(w, http.StatusOK, res)
}

func (a *api) validateConfig(w http.ResponseWriter, r *http.Request) {
	data, err := readConfigBody(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	cfg, err := a.Store.Validate(data)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true, "diff": config.Compare(a.Store.Current(), cfg)})
}

func (a *api) history(w http.ResponseWriter, _ *http.Request) {
	h, err := a.Store.History()
	if err != nil {
		writeErr(w, err)
		return
	}
	if h == nil {
		h = []config.HistoryEntry{}
	}
	writeJSON(w, http.StatusOK, h)
}

func (a *api) rollback(w http.ResponseWriter, r *http.Request) {
	v, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		writeErr(w, badRequest("version must be a number"))
		return
	}
	res, err := a.Store.Rollback(v)
	if err != nil {
		writeErr(w, err)
		return
	}
	a.applied(w, http.StatusOK, res)
}

func (a *api) getSchema(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/schema+json")
	_, _ = w.Write(schema.ConfigJSON)
}

// update runs a partial edit through the validate-and-swap path; every
// partial edit creates a new version (section 10).
func (a *api) update(w http.ResponseWriter, r *http.Request, status int, edit func([]byte) ([]byte, error)) {
	v, err := ifMatch(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	res, err := a.Store.Update(edit, config.ApplyOptions{IfVersion: v, WriteFile: true, Source: "api"})
	if err != nil {
		writeErr(w, err)
		return
	}
	a.applied(w, status, res)
}

// symbolView is a configured symbol with its runtime state.
type symbolView struct {
	config.Symbol
	Runtime *marketdata.SymbolState `json:"runtime,omitempty"`
}

func (a *api) symbolView(s config.Symbol) symbolView {
	v := symbolView{Symbol: s}
	if st, err := a.Market.State(s.Symbol); err == nil {
		v.Runtime = &st
	}
	return v
}

func (a *api) listSymbols(w http.ResponseWriter, _ *http.Request) {
	out := []symbolView{}
	for _, s := range a.Store.Current().Symbols {
		out = append(out, a.symbolView(s))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) getSymbol(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("symbol")
	for _, s := range a.Store.Current().Symbols {
		if s.Symbol == name {
			writeJSON(w, http.StatusOK, a.symbolView(s))
			return
		}
	}
	writeErr(w, config.ErrNotFound)
}

// itemBody reads a symbol or rule object and checks its id field against
// the path (when given); a missing id is taken from the path.
func itemBody(r *http.Request, idField, pathID string) (map[string]any, string, error) {
	m, err := decodeObject(r)
	if err != nil {
		return nil, "", err
	}
	id, _ := m[idField].(string)
	switch {
	case id == "" && pathID == "":
		return nil, "", badRequest("%s is required", idField)
	case id == "":
		m[idField], id = pathID, pathID
	}
	return m, id, nil
}

func (a *api) addSymbol(w http.ResponseWriter, r *http.Request) {
	s, name, err := itemBody(r, "symbol", "")
	if err != nil {
		writeErr(w, err)
		return
	}
	a.update(w, r, http.StatusCreated, func(raw []byte) ([]byte, error) {
		return config.SetItem(raw, "symbols", "symbol", name, s, true, false)
	})
}

func (a *api) putSymbol(w http.ResponseWriter, r *http.Request) {
	path := r.PathValue("symbol")
	s, name, err := itemBody(r, "symbol", path)
	if err != nil {
		writeErr(w, err)
		return
	}
	if name != path {
		writeErr(w, badRequest("symbol in the body (%s) differs from the path (%s)", name, path))
		return
	}
	a.update(w, r, http.StatusOK, func(raw []byte) ([]byte, error) {
		return config.SetItem(raw, "symbols", "symbol", path, s, false, true)
	})
}

func (a *api) deleteSymbol(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("symbol")
	a.update(w, r, http.StatusOK, func(raw []byte) ([]byte, error) {
		return config.DeleteItem(raw, "symbols", "symbol", name)
	})
}

// Runtime commands take effect at once and are not written to the YAML.
func (a *api) runtime(w http.ResponseWriter, r *http.Request, cmd func(string) error) {
	name := r.PathValue("symbol")
	if err := cmd(name); err != nil {
		if !errors.Is(err, marketdata.ErrUnknownSymbol) {
			err = badRequest("%v", err)
		}
		writeErr(w, err)
		return
	}
	st, _ := a.Market.State(name)
	writeJSON(w, http.StatusOK, st)
}

func (a *api) pause(w http.ResponseWriter, r *http.Request)  { a.runtime(w, r, a.Market.Pause) }
func (a *api) resume(w http.ResponseWriter, r *http.Request) { a.runtime(w, r, a.Market.Resume) }

func (a *api) setPrice(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mid float64 `json:"mid"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	a.runtime(w, r, func(s string) error { return a.Market.SetPrice(s, body.Mid) })
}

func (a *api) spike(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Percent   float64 `json:"percent"`
		Direction string  `json:"direction"` // up or down
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if body.Direction != "up" && body.Direction != "down" {
		writeErr(w, badRequest("direction must be up or down"))
		return
	}
	a.runtime(w, r, func(s string) error { return a.Market.Spike(s, body.Percent, body.Direction == "up") })
}

func (a *api) listRules(w http.ResponseWriter, _ *http.Request) {
	cfg := a.Store.Current()
	rules := slices.Clone(cfg.OrderRules)
	slices.SortStableFunc(rules, func(x, y config.Rule) int { return x.Priority - y.Priority })
	if rules == nil {
		rules = []config.Rule{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules, "default_action": cfg.DefaultAction})
}

func (a *api) addRule(w http.ResponseWriter, r *http.Request) {
	rule, name, err := itemBody(r, "name", "")
	if err != nil {
		writeErr(w, err)
		return
	}
	a.update(w, r, http.StatusCreated, func(raw []byte) ([]byte, error) {
		return config.SetItem(raw, "order_rules", "name", name, rule, true, false)
	})
}

func (a *api) putRule(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rule, _, err := itemBody(r, "name", name)
	if err != nil {
		writeErr(w, err)
		return
	}
	// A rename is allowed: the rule at name is replaced by the body.
	a.update(w, r, http.StatusOK, func(raw []byte) ([]byte, error) {
		return config.SetItem(raw, "order_rules", "name", name, rule, false, true)
	})
}

func (a *api) deleteRule(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	a.update(w, r, http.StatusOK, func(raw []byte) ([]byte, error) {
		return config.DeleteItem(raw, "order_rules", "name", name)
	})
}

// reorderRules takes {"order": [names...]} in the new order (drag and drop)
// and assigns priorities 10, 20, 30, ...
func (a *api) reorderRules(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Order []string `json:"order"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	prio := map[string]any{}
	for i, name := range body.Order {
		if _, dup := prio[name]; dup {
			writeErr(w, badRequest("rule %q listed twice", name))
			return
		}
		prio[name] = (i + 1) * 10
	}
	a.update(w, r, http.StatusOK, func(raw []byte) ([]byte, error) {
		return config.SetField(raw, "order_rules", "name", "priority", prio)
	})
}

// testOrder is a sample order for the rule tester.
type testOrder struct {
	MessageType string            `json:"message_type"`
	Symbol      string            `json:"symbol"`
	Side        string            `json:"side"`
	OrdType     string            `json:"ord_type"`
	Qty         decimal.Decimal   `json:"qty"`
	Price       *decimal.Decimal  `json:"price"`
	Account     string            `json:"account"`
	ClOrdID     string            `json:"cl_ord_id"`
	CompID      string            `json:"comp_id"`
	Tags        map[string]string `json:"tags"`
	InList      bool              `json:"in_list"`
	ListID      string            `json:"list_id"`
	ListSeqNo   int               `json:"list_seq_no"`
	// YAML, when set, is a draft config to test instead of the running one.
	YAML string `json:"yaml,omitempty"`
}

type testStep struct {
	Type      string          `json:"type"`
	DelayMs   int64           `json:"delay_ms"`
	AtMs      int64           `json:"at_ms"` // cumulative time from arrival
	Ratio     decimal.Decimal `json:"ratio,omitzero"`
	Qty       decimal.Decimal `json:"qty,omitzero"`
	Count     int             `json:"count,omitempty"`
	PriceMode string          `json:"price_mode,omitempty"`
	Price     decimal.Decimal `json:"price,omitzero"`
	Reason    int             `json:"reason,omitempty"`
	Text      string          `json:"text,omitempty"`
}

// testRule returns the rule a sample order matches and its action chain.
// Rules keep no state, so the result equals live execution (section 7.2).
func (a *api) testRule(w http.ResponseWriter, r *http.Request) {
	var t testOrder
	if err := decodeJSON(r, &t); err != nil {
		writeErr(w, err)
		return
	}
	cfg := a.Store.Current()
	if t.YAML != "" {
		draft, err := a.Store.Validate([]byte(t.YAML))
		if err != nil {
			writeErr(w, err)
			return
		}
		cfg = draft
	}
	rs, errs := order.Compile(cfg)
	if len(errs) > 0 {
		writeErr(w, &config.ValidationError{Errors: errs})
		return
	}
	req := order.Request{
		MsgType: orDefault(t.MessageType, "D"), CompID: t.CompID, Symbol: t.Symbol,
		Side: fixValue(t.Side, map[string]string{"BUY": "1", "SELL": "2"}), OrdType: fixValue(orDefault(t.OrdType, "MARKET"), map[string]string{"MARKET": "1"}),
		Account: t.Account, ClOrdID: t.ClOrdID, Qty: t.Qty, Price: t.Price,
		InList: t.InList, ListID: t.ListID, ListSeqNo: t.ListSeqNo,
		Tag: func(tag int) (string, bool) { v, ok := t.Tags[strconv.Itoa(tag)]; return v, ok },
	}
	name, steps := rs.Match(req)
	out := []testStep{}
	var at time.Duration
	for _, s := range steps {
		at += s.Delay
		ts := testStep{Type: s.Type, DelayMs: s.Delay.Milliseconds(), AtMs: at.Milliseconds(), Ratio: s.Ratio,
			Qty: s.Qty, Price: s.Price, Reason: s.Reason, Text: s.Text}
		if s.Type == "fill" || s.Type == "partial_fill" {
			ts.PriceMode = s.PriceMode
		}
		if s.Type == "partial_fill" {
			ts.Count = s.Count
		}
		out = append(out, ts)
		if s.Type == "partial_fill" && s.Count > 1 {
			at += s.Delay * time.Duration(s.Count-1)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rule": name, "actions": out, "total_ms": at.Milliseconds()})
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func fixValue(v string, names map[string]string) string {
	if fix, ok := names[strings.ToUpper(v)]; ok {
		return fix
	}
	return v
}

func (a *api) sessions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.Server.Sessions(a.Store.Current()))
}

// sessionByID finds a session by its full ID or its target CompID.
func (a *api) sessionByID(id string) (quickfix.SessionID, bool) {
	for _, s := range a.Server.Sessions(a.Store.Current()) {
		if s.ID == id || s.TargetCompID == id {
			return quickfix.SessionID{BeginString: a.Store.Current().Server.FIX.Version, SenderCompID: s.SenderCompID, TargetCompID: s.TargetCompID}, true
		}
	}
	return quickfix.SessionID{}, false
}

func (a *api) logout(w http.ResponseWriter, r *http.Request) {
	id, ok := a.sessionByID(r.PathValue("id"))
	if !ok {
		writeErr(w, config.ErrNotFound)
		return
	}
	if err := a.Server.Logout(id, "Logged out from the admin API"); err != nil {
		if errors.Is(err, fixserver.ErrNotLoggedOn) {
			writeError(w, http.StatusConflict, "not_logged_on", err.Error(), nil)
			return
		}
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logged_out": id.String()})
}

// fault triggers a fault at once: {"type": "drop" | "seq_gap" | "heartbeat_miss"}.
func (a *api) fault(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type string `json:"type"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	id, ok := a.sessionByID(r.PathValue("id"))
	if !ok {
		writeErr(w, config.ErrNotFound)
		return
	}
	switch err := a.Server.TriggerFault(id, body.Type); {
	case errors.Is(err, fixserver.ErrNotLoggedOn):
		writeError(w, http.StatusConflict, "not_logged_on", err.Error(), nil)
	case errors.Is(err, fixserver.ErrUnknownFault):
		writeErr(w, badRequest("%v", err))
	case err != nil:
		writeErr(w, err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"session": id.String(), "fault": body.Type})
	}
}

type orderView struct {
	OrderID     string          `json:"order_id"`
	Session     string          `json:"session"`
	ClOrdID     string          `json:"cl_ord_id"`
	OrigClOrdID string          `json:"orig_cl_ord_id,omitempty"`
	ListID      string          `json:"list_id,omitempty"`
	Symbol      string          `json:"symbol"`
	Side        string          `json:"side"`
	OrderQty    decimal.Decimal `json:"order_qty"`
	CumQty      decimal.Decimal `json:"cum_qty"`
	LeavesQty   decimal.Decimal `json:"leaves_qty"`
	AvgPx       decimal.Decimal `json:"avg_px"`
	Status      string          `json:"status"`
	Rule        string          `json:"rule,omitempty"`
	Fills       []order.Fill    `json:"fills"`
	CreatedAt   time.Time       `json:"created_at"`
}

// orders filters by session (ID or target CompID), symbol and status
// ("open", "closed" or a FIX OrdStatus), newest first, up to limit
// (default 100).
func (a *api) orders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 100
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n <= 0 {
			writeErr(w, badRequest("limit must be a positive number"))
			return
		}
		limit = n
	}
	var session quickfix.SessionID
	if s := q.Get("session"); s != "" {
		id, ok := a.sessionByID(s)
		if !ok {
			writeJSON(w, http.StatusOK, []orderView{})
			return
		}
		session = id
	}
	all := a.Orders.Orders(session)
	slices.SortFunc(all, func(x, y order.Order) int { return y.CreatedAt.Compare(x.CreatedAt) })
	out := []orderView{}
	for _, o := range all {
		if s := q.Get("symbol"); s != "" && o.Symbol != s {
			continue
		}
		switch st := q.Get("status"); st {
		case "":
		case "open":
			if o.Closed() {
				continue
			}
		case "closed":
			if !o.Closed() {
				continue
			}
		default:
			if string(o.Status) != st {
				continue
			}
		}
		out = append(out, orderView{OrderID: o.OrderID, Session: o.Session.String(), ClOrdID: o.ClOrdID, OrigClOrdID: o.OrigClOrdID,
			ListID: o.ListID, Symbol: o.Symbol, Side: o.Side, OrderQty: o.OrderQty, CumQty: o.CumQty, LeavesQty: o.LeavesQty,
			AvgPx: o.AvgPx.Round(8), Status: string(o.Status), Rule: o.Rule, Fills: o.Fills, CreatedAt: o.CreatedAt})
		if len(out) == limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}
