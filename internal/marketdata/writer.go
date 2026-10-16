package marketdata

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quickfixgo/quickfix"

	"github.com/quynv/mock-fixser/fix/enum"
	"github.com/quynv/mock-fixser/internal/clock"
	"github.com/quynv/mock-fixser/internal/config"
)

// maxPending bounds the tick queue of a session when ticks are not
// conflated; beyond it the oldest tick is dropped.
const maxPending = 10000

// Sender sends one message to a session. quickfix.SendToTarget blocks
// while the session's connection is busy, which is what makes a slow
// session conflate instead of slowing down the others.
type Sender func(m *quickfix.Message, id quickfix.SessionID) error

type pendingTick struct {
	sub    *subscription
	q      Quote
	action enum.MDUpdateAction
}

type slotKey struct {
	sub    *subscription
	symbol string
}

// writer sends market data to one session from its own goroutine
// (section 6.4). Control messages (W replies, Y) queue in order and are
// never dropped. Ticks wait in slots: a new tick for the same subscription
// and symbol replaces the waiting one (conflation), and with batching
// enabled ticks are collected for flush_interval_ms and sent as X
// messages carrying several symbols (section 6.6).
type writer struct {
	id       quickfix.SessionID
	send     Sender
	clock    clock.Clock
	log      *slog.Logger
	batching func() config.Batching

	conflated atomic.Uint64

	mu        sync.Mutex
	fifo      []*quickfix.Message
	ticks     []pendingTick
	slots     map[slotKey]int
	firstTick time.Time

	signal chan struct{}
	stop   chan struct{}
	done   chan struct{}
}

func newWriter(id quickfix.SessionID, send Sender, clk clock.Clock, log *slog.Logger, batching func() config.Batching) *writer {
	w := &writer{
		id: id, send: send, clock: clk, log: log, batching: batching,
		slots:  map[slotKey]int{},
		signal: make(chan struct{}, 1),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go w.run()
	return w
}

// close stops the writer without waiting: it runs on the quickfixgo
// session goroutine at logout, while the writer may be blocked sending to
// that same session.
func (w *writer) close() {
	close(w.stop)
}

func (w *writer) notify() {
	select {
	case w.signal <- struct{}{}:
	default:
	}
}

func (w *writer) enqueueMsg(m *quickfix.Message) {
	w.mu.Lock()
	w.fifo = append(w.fifo, m)
	w.mu.Unlock()
	w.notify()
}

// enqueueTick never blocks. Without batching, ticks are always conflated;
// with batching, the conflate setting decides.
func (w *writer) enqueueTick(t pendingTick) {
	b := w.batching()
	conflate := !b.Enabled || b.Conflate

	w.mu.Lock()
	key := slotKey{t.sub, t.q.Symbol}
	if idx, ok := w.slots[key]; ok && conflate {
		if w.ticks[idx].action == enum.MDUpdateAction_NEW {
			t.action = enum.MDUpdateAction_NEW // the client has not seen New yet
		}
		w.ticks[idx] = t
		w.conflated.Add(1)
	} else {
		if len(w.ticks) == 0 {
			w.firstTick = w.clock.Now()
		}
		if len(w.ticks) >= maxPending {
			w.ticks = w.ticks[1:]
			w.conflated.Add(1)
			w.reindexLocked()
		}
		w.slots[key] = len(w.ticks)
		w.ticks = append(w.ticks, t)
	}
	w.mu.Unlock()
	w.notify()
}

func (w *writer) reindexLocked() {
	clear(w.slots)
	for i, t := range w.ticks {
		w.slots[slotKey{t.sub, t.q.Symbol}] = i
	}
}

func (w *writer) run() {
	defer close(w.done)
	timer := w.clock.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-w.signal:
		case <-timer.C():
		}
		if wait := w.drain(); wait > 0 {
			timer.Reset(wait)
		}
	}
}

// drain sends everything that is due and returns how long until the
// current batch window closes (0 if nothing waits).
func (w *writer) drain() time.Duration {
	for {
		b := w.batching()
		w.mu.Lock()
		fifo := w.fifo
		w.fifo = nil
		var ticks []pendingTick
		var wait time.Duration
		if len(w.ticks) > 0 {
			due := w.firstTick.Add(time.Duration(b.FlushIntervalMs) * time.Millisecond)
			if now := w.clock.Now(); b.Enabled && now.Before(due) {
				wait = due.Sub(now)
			} else {
				ticks = w.ticks
				w.ticks = nil
				clear(w.slots)
			}
		}
		w.mu.Unlock()

		if len(fifo) == 0 && len(ticks) == 0 {
			return wait
		}
		for _, m := range fifo {
			w.sendOne(m)
		}
		w.sendTicks(ticks, b)
		select {
		case <-w.stop:
			return 0
		default:
		}
	}
}

func (w *writer) sendOne(m *quickfix.Message) {
	if err := w.send(m, w.id); err != nil {
		w.log.Debug("market data send failed", "session", w.id.String(), "error", err)
	}
}

func (w *writer) sendTicks(ticks []pendingTick, b config.Batching) {
	var inc []pendingTick
	for _, t := range ticks {
		switch {
		case !t.sub.active.Load():
		case t.sub.full || t.q.FullRefresh:
			w.sendOne(snapshotMsg(t.sub.mdReqID, t.q, t.sub.v))
		case !b.Enabled:
			w.sendOne(incrementalMsg(t.sub.mdReqID, []incItem{{t.q, t.sub.v, t.action}}))
		default:
			inc = append(inc, t)
		}
	}
	if len(inc) == 0 {
		return
	}
	if b.Scope == "per_session" {
		w.sendBatches("", inc, b)
		return
	}
	// per_request: one X stream per MDReqID, in order of first appearance.
	var order []*subscription
	groups := map[*subscription][]pendingTick{}
	for _, t := range inc {
		if _, ok := groups[t.sub]; !ok {
			order = append(order, t.sub)
		}
		groups[t.sub] = append(groups[t.sub], t)
	}
	for _, sub := range order {
		w.sendBatches(sub.mdReqID, groups[sub], b)
	}
}

// sendBatches splits ticks into X messages within max_symbols_per_msg and
// max_entries_per_msg.
func (w *writer) sendBatches(mdReqID string, ticks []pendingTick, b config.Batching) {
	var items []incItem
	symbols := map[string]bool{}
	entries := 0
	flush := func() {
		if len(items) > 0 {
			w.sendOne(incrementalMsg(mdReqID, items))
		}
		items, entries = nil, 0
		clear(symbols)
	}
	for _, t := range ticks {
		n := t.sub.v.entries(t.q)
		if len(items) > 0 && ((!symbols[t.q.Symbol] && len(symbols) >= b.MaxSymbolsPerMsg) || entries+n > b.MaxEntriesPerMsg) {
			flush()
		}
		items = append(items, incItem{t.q, t.sub.v, t.action})
		symbols[t.q.Symbol] = true
		entries += n
	}
	flush()
}
