package fixserver

import (
	"bytes"
	"errors"
	"sync"
	"time"

	"github.com/quickfixgo/quickfix"
)

// Flow control. quickfix.SendToTarget never blocks: it appends to an
// unbounded queue, and while the connection is busy the session goroutine
// spins re-sending to itself. Server.Send instead lets at most one
// application message per session wait for the network: the connection
// wrapper reports each completed write. Market data writers therefore block
// on a slow session and conflate (section 6.4), order messages go first,
// and quickfixgo's queue stays empty.

// ErrSendTimeout is returned when a session makes no progress for a while.
var ErrSendTimeout = errors.New("send timed out: the session is not reading")

const sendTimeout = 10 * time.Second

type flow struct {
	mu       sync.Mutex
	cond     *sync.Cond
	inFlight int
	waitHigh int    // high-priority senders waiting
	epoch    uint64 // bumped at disconnect to release waiters
}

func newFlow() *flow {
	f := &flow{}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// acquire waits for the session's single slot. High priority (orders)
// goes before waiting low-priority senders (market data).
func (f *flow) acquire(high bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	epoch := f.epoch
	if high {
		f.waitHigh++
		defer func() { f.waitHigh-- }()
	}
	deadline := time.Now().Add(sendTimeout)
	timer := time.AfterFunc(sendTimeout, func() {
		f.mu.Lock()
		f.cond.Broadcast()
		f.mu.Unlock()
	})
	defer timer.Stop()
	for f.inFlight > 0 || (!high && f.waitHigh > 0) {
		if f.epoch != epoch {
			return ErrNotLoggedOn
		}
		if time.Now().After(deadline) {
			return ErrSendTimeout
		}
		f.cond.Wait()
	}
	f.inFlight++
	return nil
}

func (f *flow) release() {
	f.mu.Lock()
	if f.inFlight > 0 {
		f.inFlight--
	}
	f.cond.Broadcast()
	f.mu.Unlock()
}

// reset frees every waiter, at disconnect.
func (f *flow) reset() {
	f.mu.Lock()
	f.inFlight = 0
	f.epoch++
	f.cond.Broadcast()
	f.mu.Unlock()
}

func (s *Server) flowFor(id quickfix.SessionID) *flow {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.flows[id]
	if !ok {
		f = newFlow()
		s.flows[id] = f
	}
	return f
}

// Send sends an application message with flow control. It blocks while the
// session's previous message is still being written, and fails at once for
// a session that is not logged on (nothing queues for offline sessions).
// priority messages (orders) go before market data.
func (s *Server) Send(msg *quickfix.Message, id quickfix.SessionID, priority bool) error {
	if !s.app.loggedOn(id) {
		return ErrNotLoggedOn
	}
	f := s.flowFor(id)
	if err := f.acquire(priority); err != nil {
		return err
	}
	if err := quickfix.SendToTarget(msg, id); err != nil {
		f.release()
		return err
	}
	return nil
}

// SendMarketData and SendOrder are Send with the two priorities, shaped as
// the engines' Sender functions.
func (s *Server) SendMarketData(m *quickfix.Message, id quickfix.SessionID) error {
	return s.Send(m, id, false)
}

func (s *Server) SendOrder(m *quickfix.Message, id quickfix.SessionID) error {
	return s.Send(m, id, true)
}

// written is called by the connection wrapper after each completed write.
// Only application messages sent through Send count: admin messages and
// resends (PossDupFlag) did not take a slot.
func (f *flow) written(b []byte) {
	if isAdmin(MsgType(b)) || bytes.Contains(b, []byte("\x0143=Y\x01")) {
		return
	}
	f.release()
}
