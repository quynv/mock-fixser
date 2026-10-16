// Package config loads, validates and hot-reloads the mockfix YAML
// configuration (design doc, section 9).
package config

import "encoding/json"

// Config is the whole mockfix configuration. A *Config held by the Store is
// immutable: changes produce a new Config that is swapped in atomically.
type Config struct {
	Version       int        `yaml:"version" json:"version"`
	Server        Server     `yaml:"server" json:"server"`
	MarketData    MarketData `yaml:"market_data,omitempty" json:"market_data,omitzero"`
	Symbols       []Symbol   `yaml:"symbols" json:"symbols"`
	OrderRules    []Rule     `yaml:"order_rules,omitempty" json:"order_rules,omitempty"`
	DefaultAction []Action   `yaml:"default_action,omitempty" json:"default_action,omitempty"`
	OrderLists    OrderLists `yaml:"order_lists,omitempty" json:"order_lists,omitzero"`
	ListRules     []ListRule `yaml:"list_rules,omitempty" json:"list_rules,omitempty"`
}

// OrderLists configures NewOrderList (E) handling (section 7.6).
type OrderLists struct {
	// Enabled defaults to true; when false, E, K and M get
	// BusinessMessageReject.
	Enabled              *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	MaxOrdersPerList     int    `yaml:"max_orders_per_list" json:"max_orders_per_list"`
	Ack                  string `yaml:"ack" json:"ack"`       // list_status | none
	Report               string `yaml:"report" json:"report"` // per_order_er | list_status | both
	ListStatusIntervalMs int    `yaml:"list_status_interval_ms" json:"list_status_interval_ms"`
	FinalListStatus      *bool  `yaml:"final_list_status,omitempty" json:"final_list_status,omitempty"`
}

func (l OrderLists) IsEnabled() bool        { return l.Enabled == nil || *l.Enabled }
func (l OrderLists) SendsFinalStatus() bool { return l.FinalListStatus == nil || *l.FinalListStatus }

// ListRule matches a whole list; its single action is reject_list or
// delay_ack.
type ListRule struct {
	Name     string         `yaml:"name" json:"name"`
	Priority int            `yaml:"priority" json:"priority"`
	Match    map[string]any `yaml:"match,omitempty" json:"match,omitempty"`
	Action   Action         `yaml:"action" json:"action"`
}

type Server struct {
	FIX     FIXServer `yaml:"fix" json:"fix"`
	Monitor Monitor   `yaml:"monitor" json:"monitor"`
	HTTP    HTTP      `yaml:"http" json:"http"`
}

type FIXServer struct {
	Listen         string    `yaml:"listen" json:"listen"`
	Version        string    `yaml:"version" json:"version"`
	DataDictionary string    `yaml:"data_dictionary" json:"data_dictionary"`
	Sessions       []Session `yaml:"sessions" json:"sessions"`
}

type Session struct {
	SenderCompID string `yaml:"sender_comp_id" json:"sender_comp_id"`
	TargetCompID string `yaml:"target_comp_id" json:"target_comp_id"`
	// Version is FIX.4.2, FIX.4.4 or FIX.5.0SP2; empty means server.fix.version.
	Version    string             `yaml:"version,omitempty" json:"version,omitempty"`
	Faults     *SessionFaults     `yaml:"faults,omitempty" json:"faults,omitempty"`
	MarketData *SessionMarketData `yaml:"market_data,omitempty" json:"market_data,omitzero"`
}

// SessionFaults simulates session problems (design doc, section 5.4). They
// apply from each logon of the session.
type SessionFaults struct {
	// RejectLogon answers Logon with Logout and this text.
	RejectLogon *RejectLogon `yaml:"reject_logon,omitempty" json:"reject_logon,omitempty"`
	// DropAfter closes the TCP connection after a number of messages (both
	// directions, from logon) or seconds.
	DropAfter *DropAfter `yaml:"drop_after,omitempty" json:"drop_after,omitempty"`
	// SeqGap skips one MsgSeqNum after every N messages sent, so the client
	// has to send ResendRequest.
	SeqGap *SeqGap `yaml:"seq_gap,omitempty" json:"seq_gap,omitempty"`
	// HeartbeatMiss stops sending heartbeats (also answers to TestRequest)
	// after a number of seconds from logon.
	HeartbeatMiss *HeartbeatMiss `yaml:"heartbeat_miss,omitempty" json:"heartbeat_miss,omitempty"`
}

type RejectLogon struct {
	Text string `yaml:"text,omitempty" json:"text,omitempty"`
}

type DropAfter struct {
	Messages int `yaml:"messages,omitempty" json:"messages,omitempty"`
	Seconds  int `yaml:"seconds,omitempty" json:"seconds,omitempty"`
}

type SeqGap struct {
	Every int `yaml:"every" json:"every"`
}

type HeartbeatMiss struct {
	AfterSeconds int `yaml:"after_seconds,omitempty" json:"after_seconds,omitempty"`
}

// SessionMarketData overrides market_data settings for one session. Unset
// fields fall back to the global market_data section.
type SessionMarketData struct {
	Batching *BatchingOverride `yaml:"batching,omitempty" json:"batching,omitempty"`
}

type Monitor struct {
	Enabled       bool     `yaml:"enabled" json:"enabled"`
	Listen        string   `yaml:"listen" json:"listen"`
	SenderCompID  string   `yaml:"sender_comp_id" json:"sender_comp_id"`
	TargetCompID  string   `yaml:"target_comp_id" json:"target_comp_id"`
	DropCopy      bool     `yaml:"drop_copy" json:"drop_copy"`
	MirrorLog     bool     `yaml:"mirror_log" json:"mirror_log"`
	MirrorBuffer  int      `yaml:"mirror_buffer" json:"mirror_buffer"`
	MirrorExclude []string `yaml:"mirror_exclude" json:"mirror_exclude"`
}

type HTTP struct {
	Listen       string `yaml:"listen" json:"listen"`
	HistoryTicks int    `yaml:"history_ticks" json:"history_ticks"`
}

type MarketData struct {
	Batching Batching `yaml:"batching" json:"batching"`
}

type Batching struct {
	Enabled          bool   `yaml:"enabled" json:"enabled"`
	Scope            string `yaml:"scope" json:"scope"`
	FlushIntervalMs  int    `yaml:"flush_interval_ms" json:"flush_interval_ms"`
	MaxSymbolsPerMsg int    `yaml:"max_symbols_per_msg" json:"max_symbols_per_msg"`
	MaxEntriesPerMsg int    `yaml:"max_entries_per_msg" json:"max_entries_per_msg"`
	Conflate         bool   `yaml:"conflate" json:"conflate"`
	InitialSnapshot  string `yaml:"initial_snapshot" json:"initial_snapshot"`
}

type BatchingOverride struct {
	Enabled          *bool   `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Scope            *string `yaml:"scope,omitempty" json:"scope,omitempty"`
	FlushIntervalMs  *int    `yaml:"flush_interval_ms,omitempty" json:"flush_interval_ms,omitempty"`
	MaxSymbolsPerMsg *int    `yaml:"max_symbols_per_msg,omitempty" json:"max_symbols_per_msg,omitempty"`
	MaxEntriesPerMsg *int    `yaml:"max_entries_per_msg,omitempty" json:"max_entries_per_msg,omitempty"`
	Conflate         *bool   `yaml:"conflate,omitempty" json:"conflate,omitempty"`
	InitialSnapshot  *string `yaml:"initial_snapshot,omitempty" json:"initial_snapshot,omitempty"`
}

type Symbol struct {
	Symbol       string    `yaml:"symbol" json:"symbol"`
	InitialPrice float64   `yaml:"initial_price" json:"initial_price"`
	Min          float64   `yaml:"min" json:"min"`
	Max          float64   `yaml:"max" json:"max"`
	TickSize     float64   `yaml:"tick_size" json:"tick_size"`
	Model        string    `yaml:"model" json:"model"`
	Volatility   float64   `yaml:"volatility,omitempty" json:"volatility,omitempty"`
	Drift        float64   `yaml:"drift,omitempty" json:"drift,omitempty"`
	Sine         *Sine     `yaml:"sine,omitempty" json:"sine,omitempty"`
	Replay       *Replay   `yaml:"replay,omitempty" json:"replay,omitempty"`
	Speed        Speed     `yaml:"speed" json:"speed"`
	Spread       float64   `yaml:"spread" json:"spread"`
	DepthLevels  int       `yaml:"depth_levels" json:"depth_levels"`
	UpdateMode   string    `yaml:"update_mode" json:"update_mode"`
	Slippage     *Slippage `yaml:"slippage,omitempty" json:"slippage,omitempty"`
	Seed         *int64    `yaml:"seed,omitempty" json:"seed,omitempty"`
}

type Sine struct {
	Amplitude float64 `yaml:"amplitude" json:"amplitude"`
	PeriodMs  int     `yaml:"period_ms" json:"period_ms"`
}

type Replay struct {
	File string `yaml:"file" json:"file"`
	Loop bool   `yaml:"loop" json:"loop"`
}

type Speed struct {
	TicksPerSecond float64 `yaml:"ticks_per_second,omitempty" json:"ticks_per_second,omitempty"`
	IntervalMs     int     `yaml:"interval_ms,omitempty" json:"interval_ms,omitempty"`
	JitterMs       int     `yaml:"jitter_ms,omitempty" json:"jitter_ms,omitempty"`
}

type Slippage struct {
	Type     string  `yaml:"type" json:"type"`
	Ticks    int     `yaml:"ticks,omitempty" json:"ticks,omitempty"`
	Value    float64 `yaml:"value,omitempty" json:"value,omitempty"`
	MinTicks int     `yaml:"min_ticks,omitempty" json:"min_ticks,omitempty"`
	MaxTicks int     `yaml:"max_ticks,omitempty" json:"max_ticks,omitempty"`
}

// Rule is kept close to the YAML here; the order engine compiles Match and
// Actions into typed matchers and actions (design doc, section 7.2).
type Rule struct {
	Name        string         `yaml:"name" json:"name"`
	Priority    int            `yaml:"priority" json:"priority"`
	MessageType string         `yaml:"message_type,omitempty" json:"message_type,omitempty"`
	Match       map[string]any `yaml:"match,omitempty" json:"match,omitempty"`
	Actions     []Action       `yaml:"actions" json:"actions"`
}

// Action is one step of a rule's action chain. Params holds the
// type-specific parameters (delay_ms, ratio, reason, text, ...).
type Action struct {
	Type   string         `yaml:"type" json:"type"`
	Params map[string]any `yaml:",inline" json:"-"`
}

// SessionKey identifies a client session by its CompIDs.
type SessionKey struct {
	SenderCompID string
	TargetCompID string
}

// SessionVersion returns the FIX version of a client session.
func (c *Config) SessionVersion(s Session) string {
	if s.Version != "" {
		return s.Version
	}
	return c.Server.FIX.Version
}

func (s Session) Key() SessionKey {
	return SessionKey{SenderCompID: s.SenderCompID, TargetCompID: s.TargetCompID}
}

func (k SessionKey) String() string { return k.SenderCompID + "->" + k.TargetCompID }

// MarshalJSON flattens Params next to type, matching the YAML shape.
func (a Action) MarshalJSON() ([]byte, error) {
	m := make(map[string]any, len(a.Params)+1)
	for k, v := range a.Params {
		m[k] = v
	}
	m["type"] = a.Type
	return json.Marshal(m)
}

func (a *Action) UnmarshalJSON(data []byte) error {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	t, _ := m["type"].(string)
	delete(m, "type")
	a.Type, a.Params = t, m
	if len(m) == 0 {
		a.Params = nil
	}
	return nil
}
