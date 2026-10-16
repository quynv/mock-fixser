package config

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const editBase = `# top comment
version: 3
symbols:
  # EURUSD is the main pair
  - { symbol: EURUSD, initial_price: 1.08, min: 1.0, max: 1.2, tick_size: 0.0001, spread: 0.0002, speed: { ticks_per_second: 5 } }
order_rules:
  - name: a
    priority: 1
    actions: [ { type: ack } ]
  - name: b # keep me
    priority: 2
    actions: [ { type: ack } ]
`

func TestSetItemAndDelete(t *testing.T) {
	gbp := Symbol{Symbol: "GBPUSD", InitialPrice: 1.27, Min: 1.2, Max: 1.4, TickSize: 0.0001, Spread: 0.0002, Speed: Speed{IntervalMs: 100}}
	out, err := SetItem([]byte(editBase), "symbols", "symbol", "GBPUSD", gbp, true, false)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "# top comment") || !strings.Contains(s, "# EURUSD is the main pair") || !strings.Contains(s, "symbol: GBPUSD") {
		t.Fatalf("append lost content:\n%s", s)
	}
	if _, err := SetItem(out, "symbols", "symbol", "GBPUSD", gbp, true, false); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("second create: %v", err)
	}
	gbp.Spread = 0.0004
	out, err = SetItem(out, "symbols", "symbol", "GBPUSD", gbp, false, true)
	if err != nil || !strings.Contains(string(out), "spread: 0.0004") {
		t.Fatalf("replace: %v\n%s", err, out)
	}
	if _, err := SetItem(out, "symbols", "symbol", "NOPE", gbp, false, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replace missing: %v", err)
	}
	out, err = DeleteItem(out, "symbols", "symbol", "EURUSD")
	if err != nil || strings.Contains(string(out), "EURUSD") {
		t.Fatalf("delete: %v\n%s", err, out)
	}
	if _, err := DeleteItem(out, "order_rules", "name", "zzz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestSetFieldPriorities(t *testing.T) {
	out, err := SetField([]byte(editBase), "order_rules", "name", "priority", map[string]any{"a": 20, "b": 10})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(out, cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.OrderRules[0].Priority != 20 || cfg.OrderRules[1].Priority != 10 || !strings.Contains(string(out), "# keep me") {
		t.Fatalf("priorities not set:\n%s", out)
	}
	if _, err := SetField([]byte(editBase), "order_rules", "name", "priority", map[string]any{"x": 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown rule: %v", err)
	}
}
