package marketdata

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/quynv/mock-fixser/internal/clock"
	"github.com/quynv/mock-fixser/internal/config"
)

type quoteLog struct {
	mu     sync.Mutex
	quotes []Quote
}

func (l *quoteLog) add(q Quote) {
	l.mu.Lock()
	l.quotes = append(l.quotes, q)
	l.mu.Unlock()
}

func (l *quoteLog) count(symbol string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, q := range l.quotes {
		if q.Symbol == symbol {
			n++
		}
	}
	return n
}

func sym(name string, speed config.Speed) config.Symbol {
	return config.Symbol{Symbol: name, InitialPrice: 100, Min: 90, Max: 110, TickSize: 0.01,
		Spread: 0.02, Speed: speed, Seed: seed(1), DepthLevels: 1}
}

func startEngine(t *testing.T, symbols []config.Symbol) (*Engine, *clock.Fake, *quoteLog) {
	t.Helper()
	clk := clock.NewFake(t0)
	log := &quoteLog{}
	e, err := NewEngine(symbols, clk, slog.New(slog.DiscardHandler), log.add)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	clk.BlockUntil(1)
	return e, clk, log
}

// step advances the fake clock in small steps, letting the engine handle
// each one before the next.
func step(clk *clock.Fake, total, by time.Duration) {
	for d := time.Duration(0); d < total; d += by {
		clk.Advance(by)
		clk.BlockUntil(1)
	}
}

func TestEngineTicksAtConfiguredSpeed(t *testing.T) {
	_, clk, log := startEngine(t, []config.Symbol{
		sym("A", config.Speed{TicksPerSecond: 5}),
		sym("B", config.Speed{IntervalMs: 100}),
	})
	step(clk, time.Second, 10*time.Millisecond)
	if a, b := log.count("A"), log.count("B"); a != 5 || b != 10 {
		t.Fatalf("ticks A=%d B=%d, want 5 and 10", a, b)
	}
}

func TestEngineApplySymbols(t *testing.T) {
	e, clk, log := startEngine(t, []config.Symbol{sym("A", config.Speed{IntervalMs: 100})})
	step(clk, 500*time.Millisecond, 10*time.Millisecond)

	lastA, _, _ := e.Last("A")
	changed := sym("A", config.Speed{IntervalMs: 100})
	changed.Spread = 0.04
	if err := e.ApplySymbols([]config.Symbol{changed, sym("C", config.Speed{IntervalMs: 50})}); err != nil {
		t.Fatal(err)
	}
	clk.BlockUntil(1)

	q, cfg, _ := e.Last("A")
	if cfg.Spread != 0.04 || q.Mid != lastA.Mid || q.Ask-q.Bid < 0.039 {
		t.Fatalf("A not updated in place: cfg spread=%v mid %v->%v", cfg.Spread, lastA.Mid, q.Mid)
	}
	if !e.Known("C") {
		t.Fatal("C not added")
	}

	before := log.count("A")
	step(clk, 500*time.Millisecond, 10*time.Millisecond)
	if got := log.count("C"); got != 10 {
		t.Fatalf("C ticks = %d, want 10", got)
	}
	if got := log.count("A") - before; got != 5 {
		t.Fatalf("A ticks after change = %d, want 5", got)
	}

	if err := e.ApplySymbols([]config.Symbol{changed}); err != nil {
		t.Fatal(err)
	}
	clk.BlockUntil(1)
	before = log.count("C")
	step(clk, 300*time.Millisecond, 10*time.Millisecond)
	if e.Known("C") || log.count("C") != before {
		t.Fatal("C still ticking after removal")
	}
}

func TestEngineJumpsAheadWhenLagging(t *testing.T) {
	_, clk, log := startEngine(t, []config.Symbol{sym("A", config.Speed{IntervalMs: 10})})
	clk.Advance(10 * time.Second) // one big jump: no 1000-tick burst
	clk.BlockUntil(1)
	if n := log.count("A"); n > 150 {
		t.Fatalf("caught up with %d ticks, want a bounded catch-up", n)
	}
}

func TestRuntimeCommands(t *testing.T) {
	e, clk, log := startEngine(t, []config.Symbol{sym("A", config.Speed{IntervalMs: 100})})
	if err := e.Pause("A"); err != nil {
		t.Fatal(err)
	}
	step(clk, 500*time.Millisecond, 10*time.Millisecond)
	if n := log.count("A"); n != 0 {
		t.Fatalf("%d ticks while paused", n)
	}
	if err := e.SetPrice("A", 105); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.State("A"); s.Mid != 105 || s.Bid != 104.99 || !s.Paused || log.count("A") != 1 {
		t.Fatalf("after set_price: %+v, %d quotes", s, log.count("A"))
	}
	if err := e.Spike("A", 10, false); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.State("A"); s.Mid != 94.5 {
		t.Fatalf("after spike down 10%%: mid %v", s.Mid)
	}
	if err := e.Spike("A", 50, true); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.State("A"); s.Mid != 110 {
		t.Fatalf("spike clamped to max: mid %v", s.Mid)
	}
	if err := e.SetPrice("A", 500); err == nil {
		t.Fatal("price outside [min, max] accepted")
	}
	if err := e.Resume("A"); err != nil {
		t.Fatal(err)
	}
	step(clk, 300*time.Millisecond, 10*time.Millisecond)
	if n := log.count("A"); n != 6 {
		t.Fatalf("quotes = %d, want 3 forced + 3 ticks", n)
	}
	if err := e.Pause("NOPE"); err != ErrUnknownSymbol {
		t.Fatalf("Pause(NOPE) = %v", err)
	}
}
