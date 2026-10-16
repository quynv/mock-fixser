// Package clock abstracts time so tests can drive schedulers with a fake
// clock instead of sleeping (design doc, section 6.2).
package clock

import (
	"sort"
	"sync"
	"time"
)

type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

type Timer interface {
	C() <-chan time.Time
	// Stop and Reset follow time.Timer semantics (Go 1.23+: no stale
	// values are left in C after Stop or Reset).
	Stop() bool
	Reset(d time.Duration) bool
}

// Real is the wall clock.
type Real struct{}

func (Real) Now() time.Time { return time.Now() }

func (Real) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time        { return r.t.C }
func (r realTimer) Stop() bool                 { return r.t.Stop() }
func (r realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }

// Fake is a manually advanced clock. Timers fire during Advance, in
// deadline order, with Now set to each deadline as it fires.
type Fake struct {
	mu     sync.Mutex
	cond   *sync.Cond
	now    time.Time
	timers map[*fakeTimer]struct{}
}

func NewFake(start time.Time) *Fake {
	f := &Fake{now: start, timers: map[*fakeTimer]struct{}{}}
	f.cond = sync.NewCond(&f.mu)
	return f
}

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{f: f, c: make(chan time.Time, 1)}
	f.arm(t, d)
	return t
}

// Advance moves the clock forward by d, firing due timers.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	end := f.now.Add(d)
	for {
		due := make([]*fakeTimer, 0)
		for t := range f.timers {
			if !t.when.After(end) {
				due = append(due, t)
			}
		}
		if len(due) == 0 {
			break
		}
		sort.Slice(due, func(i, j int) bool { return due[i].when.Before(due[j].when) })
		t := due[0]
		if t.when.After(f.now) {
			f.now = t.when
		}
		delete(f.timers, t)
		select {
		case t.c <- f.now:
		default:
		}
	}
	f.now = end
	f.cond.Broadcast()
}

// BlockUntil waits until at least n timers are armed. Tests call it after
// Advance to know the code under test has handled the fired timers and
// armed its next one.
func (f *Fake) BlockUntil(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.timers) < n {
		f.cond.Wait()
	}
}

func (f *Fake) arm(t *fakeTimer, d time.Duration) {
	t.when = f.now.Add(d)
	f.timers[t] = struct{}{}
	f.cond.Broadcast()
}

type fakeTimer struct {
	f    *Fake
	when time.Time
	c    chan time.Time
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }

func (t *fakeTimer) Stop() bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	_, active := t.f.timers[t]
	delete(t.f.timers, t)
	t.drain()
	return active
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	_, active := t.f.timers[t]
	t.drain()
	t.f.arm(t, d)
	return active
}

func (t *fakeTimer) drain() {
	select {
	case <-t.c:
	default:
	}
}
