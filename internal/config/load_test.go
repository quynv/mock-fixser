package config

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// chdirRepoRoot makes relative paths in the sample config (data/aapl.csv) resolve.
func chdirRepoRoot(t *testing.T) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	t.Chdir(filepath.Join(filepath.Dir(file), "../.."))
}

func TestParseSampleConfig(t *testing.T) {
	chdirRepoRoot(t)
	cfg, _, err := Load("configs/mock.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != 12 || len(cfg.Symbols) != 2 || len(cfg.OrderRules) != 3 || len(cfg.DefaultAction) != 2 {
		t.Fatalf("unexpected config: version=%d symbols=%d rules=%d default=%d",
			cfg.Version, len(cfg.Symbols), len(cfg.OrderRules), len(cfg.DefaultAction))
	}
	if got := cfg.OrderRules[2].Actions[1]; got.Type != "partial_fill" || got.Params["ratio"] != 0.4 {
		t.Errorf("action params not decoded: %+v", got)
	}
	// Defaults.
	if cfg.MarketData.Batching.Scope != "per_request" || cfg.Symbols[1].DepthLevels != 1 || cfg.Symbols[1].UpdateMode != "incremental" {
		t.Errorf("defaults not applied: %+v %+v", cfg.MarketData.Batching, cfg.Symbols[1])
	}
}

const base = `
server:
  fix:
    sessions:
      - { sender_comp_id: MOCKSRV, target_comp_id: CLIENT1 }
symbols:
  - symbol: EURUSD
    initial_price: 1.0850
    min: 1.0700
    max: 1.1000
    tick_size: 0.00001
    spread: 0.00010
    speed: { ticks_per_second: 5 }
`

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		wantPath string
		wantLine int
		wantMsg  string
	}{
		{
			name:     "syntax",
			yaml:     "server:\n  fix:\n\tsessions: []\n",
			wantLine: 3,
			wantMsg:  "cannot start any token",
		},
		{
			name:     "unknown field",
			yaml:     base + "    colour: red\n",
			wantPath: "symbols[0]",
			wantLine: 7,
			wantMsg:  "colour",
		},
		{
			name:     "wrong type",
			yaml:     strings.Replace(base, "min: 1.0700", "min: low", 1),
			wantPath: "symbols[0].min",
			wantLine: 9,
		},
		{
			name:     "min not below max",
			yaml:     strings.Replace(base, "max: 1.1000", "max: 1.0700", 1),
			wantPath: "symbols[0].max",
			wantLine: 10,
			wantMsg:  "greater than min",
		},
		{
			name:     "initial price outside range",
			yaml:     strings.Replace(base, "initial_price: 1.0850", "initial_price: 1.2", 1),
			wantPath: "symbols[0].initial_price",
			wantLine: 8,
		},
		{
			name:     "spread not multiple of tick",
			yaml:     strings.Replace(base, "spread: 0.00010", "spread: 0.000015", 1),
			wantPath: "symbols[0].spread",
			wantLine: 12,
		},
		{
			name:     "speed needs one of tps or interval",
			yaml:     strings.Replace(base, "{ ticks_per_second: 5 }", "{ ticks_per_second: 5, interval_ms: 100 }", 1),
			wantPath: "symbols[0].speed",
			wantLine: 13,
		},
		{
			name:     "duplicate symbol",
			yaml:     base + strings.SplitN(base, "symbols:\n", 2)[1],
			wantPath: "symbols[1].symbol",
			wantLine: 14,
			wantMsg:  "duplicate symbol",
		},
		{
			name:     "missing replay file",
			yaml:     strings.Replace(base, "spread: 0.00010", "spread: 0.00010\n    model: replay\n    replay: { file: nope.csv }", 1),
			wantPath: "symbols[0].replay.file",
			wantLine: 14,
		},
		{
			name: "bad regex",
			yaml: base + `order_rules:
  - name: r1
    priority: 1
    match: { cl_ord_id: { regex: "(" } }
    actions: [ { type: ack } ]
`,
			wantPath: "order_rules[0].match.cl_ord_id.regex",
			wantLine: 17,
			wantMsg:  "invalid regex",
		},
		{
			name: "duplicate rule name and priority",
			yaml: base + `order_rules:
  - { name: r1, priority: 1, actions: [ { type: ack } ] }
  - { name: r1, priority: 1, actions: [ { type: ack } ] }
`,
			wantPath: "order_rules[1].name",
			wantLine: 16,
		},
		{
			name:     "unknown action type",
			yaml:     base + "default_action: [ { type: explode } ]\n",
			wantPath: "default_action[0].type",
			wantLine: 14,
		},
		{
			name:     "session collides with monitor",
			yaml:     strings.Replace(base, "target_comp_id: CLIENT1", "target_comp_id: UI_MONITOR", 1),
			wantPath: "server.fix.sessions[0].target_comp_id",
			wantLine: 5,
			wantMsg:  "reserved",
		},
		{
			name:     "duplicate session",
			yaml:     strings.Replace(base, "      - { sender_comp_id: MOCKSRV, target_comp_id: CLIENT1 }\n", "      - { sender_comp_id: MOCKSRV, target_comp_id: CLIENT1 }\n      - { sender_comp_id: MOCKSRV, target_comp_id: CLIENT1 }\n", 1),
			wantPath: "server.fix.sessions[1]",
			wantLine: 6,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("want *ValidationError, got %v", err)
			}
			for _, fe := range verr.Errors {
				if fe.Path == tt.wantPath && fe.Line == tt.wantLine && strings.Contains(fe.Message, tt.wantMsg) {
					return
				}
			}
			t.Fatalf("want error at %q line %d containing %q, got:\n%v", tt.wantPath, tt.wantLine, tt.wantMsg, err)
		})
	}
}

func TestParseValidBase(t *testing.T) {
	if _, err := Parse([]byte(base)); err != nil {
		t.Fatal(err)
	}
}

func TestActionJSONRoundTrip(t *testing.T) {
	cfg, err := Parse([]byte(base + "default_action: [ { type: partial_fill, ratio: 0.4, delay_ms: 300 } ]\n"))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.DefaultAction[0]
	data, err := a.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var back Action
	if err := back.UnmarshalJSON(data); err != nil {
		t.Fatal(err)
	}
	if back.Type != "partial_fill" || back.Params["ratio"] != 0.4 || back.Params["delay_ms"] != float64(300) {
		t.Fatalf("round trip lost data: %s -> %+v", data, back)
	}
}
