package order

import (
	"log/slog"
	"sync"

	"github.com/quickfixgo/quickfix"
)

// Sender sends one message to a session (quickfix.SendToTarget in
// production). It may block while the session's connection is busy.
type Sender func(m *quickfix.Message, id quickfix.SessionID) error

// outbox sends a session's order messages in order from its own goroutine,
// so a slow session never blocks the order engine or other sessions.
// Unlike market data, nothing is dropped.
type outbox struct {
	id   quickfix.SessionID
	send Sender
	log  *slog.Logger

	mu     sync.Mutex
	queue  []*quickfix.Message
	signal chan struct{}
	stop   chan struct{}
}

func newOutbox(id quickfix.SessionID, send Sender, log *slog.Logger) *outbox {
	b := &outbox{id: id, send: send, log: log, signal: make(chan struct{}, 1), stop: make(chan struct{})}
	go b.run()
	return b
}

func (b *outbox) push(m *quickfix.Message) {
	b.mu.Lock()
	b.queue = append(b.queue, m)
	b.mu.Unlock()
	select {
	case b.signal <- struct{}{}:
	default:
	}
}

func (b *outbox) close() { close(b.stop) }

func (b *outbox) run() {
	for {
		select {
		case <-b.stop:
			return
		case <-b.signal:
		}
		for {
			b.mu.Lock()
			q := b.queue
			b.queue = nil
			b.mu.Unlock()
			if len(q) == 0 {
				break
			}
			for _, m := range q {
				if err := b.send(m, b.id); err != nil {
					b.log.Warn("order message not sent", "session", b.id.String(), "error", err)
				}
			}
		}
	}
}
