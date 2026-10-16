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
	SenderCompID string             `yaml:"sender_comp_id" json:"sender_comp_id"`
	TargetCompID string             `yaml:"target_comp_id" json:"target_comp_id"`
	Faults       map[string]any     `yaml:"faults,omitempty" json:"faults,omitempty"`
	MarketData   *SessionMarketData `yaml:"market_data,omitempty" json:"market_data,omitzero"`
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
