package marketdata

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/quynv/mock-fixser/internal/clock"
	"github.com/quynv/mock-fixser/internal/config"
	"github.com/quynv/mock-fixser/internal/metrics"
)

// maxLag is how far a symbol may fall behind before its schedule jumps to
// now instead of catching up tick by tick.
const maxLag = time.Second

// Engine runs one generator per symbol on a single scheduler goroutine: a
// min-heap keyed by next tick time instead of a ticker per symbol
// (section 6.2). Every quote goes to the sink, outside the engine lock.
type Engine struct {
	clock clock.Clock
	log   *slog.Logger
	sink  func(Quote)

	mu   sync.Mutex
	syms map[string]*symState
	heap symHeap
	wake chan struct{}
}

type symState struct {
	cfg    config.Symbol
	gen    *Generator
	last   Quote
	next   time.Time
	index  int
	paused bool // runtime command, kept across config changes
}

func NewEngine(symbols []config.Symbol, clk clock.Clock, log *slog.Logger, sink func(Quote)) (*Engine, error) {
	e := &Engine{
		clock: clk,
		log:   log,
		sink:  sink,
		syms:  map[string]*symState{},
		wake:  make(chan struct{}, 1),
	}
	now := clk.Now()
	for _, s := range symbols {
		if err := e.addLocked(s, 0, now); err != nil {
			return nil, err
		}
	}
	return e, nil
}

func (e *Engine) addLocked(s config.Symbol, mid float64, now time.Time) error {
	g, err := NewGenerator(s, mid, now)
	if err != nil {
		return fmt.Errorf("symbol %s: %w", s.Symbol, err)
	}
	st := &symState{cfg: s, gen: g, last: g.Current(), next: now.Add(g.Interval())}
	e.syms[s.Symbol] = st
	heap.Push(&e.heap, st)
	return nil
}

// Known reports whether a symbol is configured.
func (e *Engine) Known(symbol string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.syms[symbol]
	return ok
}

// Last returns the latest quote of a symbol, used for W snapshots.
func (e *Engine) Last(symbol string) (Quote, config.Symbol, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.syms[symbol]
	if !ok {
		return Quote{}, config.Symbol{}, false
	}
	return st.last, st.cfg, true
}

// ApplySymbols adds, removes and updates symbols from a new config. A
// changed symbol keeps its current mid if it is still within [min, max].
func (e *Engine) ApplySymbols(symbols []config.Symbol) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clock.Now()

	keep := map[string]bool{}
	for _, s := range symbols {
		keep[s.Symbol] = true
		st, ok := e.syms[s.Symbol]
		switch {
		case !ok:
			if err := e.addLocked(s, 0, now); err != nil {
				return err
			}
		case !reflect.DeepEqual(st.cfg, s):
			g, err := NewGenerator(s, st.last.Mid, now)
			if err != nil {
				return fmt.Errorf("symbol %s: %w", s.Symbol, err)
			}
			speedChanged := st.cfg.Speed != s.Speed
			st.cfg, st.gen, st.last = s, g, g.Current()
			if speedChanged {
				st.next = now.Add(g.Interval())
				heap.Fix(&e.heap, st.index)
			}
		}
	}
	for name, st := range e.syms {
		if !keep[name] {
			heap.Remove(&e.heap, st.index)
			delete(e.syms, name)
		}
	}
	e.signal()
	return nil
}

func (e *Engine) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Run ticks symbols until ctx ends.
func (e *Engine) Run(ctx context.Context) error {
	// The timer is armed only after each round of work, so a test that
	// waits for an armed timer knows the engine is idle.
	timer := e.clock.NewTimer(e.untilNext())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C():
		case <-e.wake:
		}
		for _, q := range e.tickDue() {
			metrics.Ticks.WithLabelValues(q.Symbol).Inc()
			e.sink(q)
		}
		timer.Reset(e.untilNext())
	}
}

func (e *Engine) untilNext() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.heap) == 0 {
		return time.Hour
	}
	return max(e.heap[0].next.Sub(e.clock.Now()), 0)
}

// tickDue advances every symbol whose next tick time has passed. Each tick
// is stamped with its scheduled time, so a fake clock gives exact dt.
func (e *Engine) tickDue() []Quote {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clock.Now()
	var out []Quote
	for len(e.heap) > 0 && !e.heap[0].next.After(now) {
		st := e.heap[0]
		if !st.paused {
			q := st.gen.Tick(st.next)
			st.last = q
			out = append(out, q)
		}
		st.next = st.next.Add(st.gen.Interval())
		if now.Sub(st.next) > maxLag {
			st.next = now.Add(st.gen.Interval())
		}
		heap.Fix(&e.heap, 0)
	}
	return out
}

type symHeap []*symState

func (h symHeap) Len() int           { return len(h) }
func (h symHeap) Less(i, j int) bool { return h[i].next.Before(h[j].next) }
func (h symHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *symHeap) Push(x any) {
	st := x.(*symState)
	st.index = len(*h)
	*h = append(*h, st)
}
func (h *symHeap) Pop() any {
	old := *h
	st := old[len(old)-1]
	*h = old[:len(old)-1]
	return st
}

// ErrUnknownSymbol is returned by runtime commands for a symbol that is not
// configured.
var ErrUnknownSymbol = errors.New("unknown symbol")

// SymbolState is the runtime view of a symbol for the admin API.
type SymbolState struct {
	Symbol string  `json:"symbol"`
	Paused bool    `json:"paused"`
	Bid    float64 `json:"bid"`
	Ask    float64 `json:"ask"`
	Mid    float64 `json:"mid"`
	Seq    uint64  `json:"seq"`
}

// State returns the runtime state of a symbol.
func (e *Engine) State(symbol string) (SymbolState, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.syms[symbol]
	if !ok {
		return SymbolState{}, ErrUnknownSymbol
	}
	return SymbolState{Symbol: symbol, Paused: st.paused, Bid: st.last.Bid, Ask: st.last.Ask, Mid: st.last.Mid, Seq: st.last.Seq}, nil
}

// Pause stops a symbol's ticks until Resume. Runtime commands are not
// written to the YAML (section 6.5).
func (e *Engine) Pause(symbol string) error { return e.setPaused(symbol, true) }

// Resume restarts a paused symbol.
func (e *Engine) Resume(symbol string) error { return e.setPaused(symbol, false) }

func (e *Engine) setPaused(symbol string, paused bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.syms[symbol]
	if !ok {
		return ErrUnknownSymbol
	}
	st.paused = paused
	return nil
}

// SetPrice forces the mid to a level within [min, max] and publishes the
// new quote at once; the model continues from there.
func (e *Engine) SetPrice(symbol string, mid float64) error {
	return e.force(symbol, func(st *symState) (float64, error) {
		if mid < st.cfg.Min || mid > st.cfg.Max {
			return 0, fmt.Errorf("price %v outside [min, max] = [%v, %v]", mid, st.cfg.Min, st.cfg.Max)
		}
		return mid, nil
	})
}

// Spike moves the mid by percent up or down (clamped to [min, max]) and
// publishes the new quote at once; the model continues from there.
func (e *Engine) Spike(symbol string, percent float64, up bool) error {
	return e.force(symbol, func(st *symState) (float64, error) {
		if percent <= 0 {
			return 0, errors.New("percent must be positive")
		}
		f := 1 + percent/100
		if !up {
			f = 1 - percent/100
		}
		return min(max(st.last.Mid*f, st.cfg.Min), st.cfg.Max), nil
	})
}

func (e *Engine) force(symbol string, target func(*symState) (float64, error)) error {
	e.mu.Lock()
	st, ok := e.syms[symbol]
	if !ok {
		e.mu.Unlock()
		return ErrUnknownSymbol
	}
	mid, err := target(st)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	q := st.gen.Force(mid, e.clock.Now())
	st.last = q
	e.mu.Unlock()
	metrics.Ticks.WithLabelValues(q.Symbol).Inc()
	e.sink(q)
	return nil
}
