// Package sched runs delayed functions on a single goroutine, ordered by
// due time and then by scheduling order (design doc, section 7.3).
package sched

import (
	"container/heap"
	"context"
	"sync"
	"time"

	"github.com/quynv/mock-fixser/internal/clock"
)

type Scheduler struct {
	clock clock.Clock

	mu   sync.Mutex
	h    taskHeap
	seq  uint64
	wake chan struct{}
}

type task struct {
	due   time.Time
	seq   uint64
	fn    func()
	index int
}

func New(clk clock.Clock) *Scheduler {
	return &Scheduler{clock: clk, wake: make(chan struct{}, 1)}
}

// At schedules fn to run at the given time. Functions due at the same time
// run in the order they were scheduled.
func (s *Scheduler) At(due time.Time, fn func()) {
	s.mu.Lock()
	s.seq++
	heap.Push(&s.h, &task{due: due, seq: s.seq, fn: fn})
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Now returns the scheduler's clock time.
func (s *Scheduler) Now() time.Time { return s.clock.Now() }

// Run executes due tasks until ctx ends.
func (s *Scheduler) Run(ctx context.Context) error {
	// The timer is armed only after each round of work, so a test that
	// waits for an armed timer knows the scheduler is idle.
	s.RunDue()
	timer := s.clock.NewTimer(s.untilNext())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C():
		case <-s.wake:
		}
		s.RunDue()
		timer.Reset(s.untilNext())
	}
}

// RunDue runs every task due at the clock's current time, and any tasks
// they schedule for now. Run calls it; tests drive it directly with a fake
// clock so each task runs at exactly its due time.
func (s *Scheduler) RunDue() {
	for {
		tasks := s.due()
		if len(tasks) == 0 {
			return
		}
		for _, t := range tasks {
			t.fn()
		}
	}
}

// Next returns the due time of the earliest task.
func (s *Scheduler) Next() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.h) == 0 {
		return time.Time{}, false
	}
	return s.h[0].due, true
}

func (s *Scheduler) due() []*task {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()
	var out []*task
	for len(s.h) > 0 && !s.h[0].due.After(now) {
		out = append(out, heap.Pop(&s.h).(*task))
	}
	return out
}

func (s *Scheduler) untilNext() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.h) == 0 {
		return time.Hour
	}
	return max(s.h[0].due.Sub(s.clock.Now()), 0)
}

type taskHeap []*task

func (h taskHeap) Len() int { return len(h) }
func (h taskHeap) Less(i, j int) bool {
	if !h[i].due.Equal(h[j].due) {
		return h[i].due.Before(h[j].due)
	}
	return h[i].seq < h[j].seq
}
func (h taskHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *taskHeap) Push(x any) {
	t := x.(*task)
	t.index = len(*h)
	*h = append(*h, t)
}
func (h *taskHeap) Pop() any {
	old := *h
	t := old[len(old)-1]
	*h = old[:len(old)-1]
	return t
}
