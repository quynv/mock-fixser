package sched

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/quynv/mock-fixser/internal/clock"
)

func TestRunsInDueThenScheduleOrder(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	s := New(clk)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	clk.BlockUntil(1)

	var mu sync.Mutex
	var got []string
	rec := func(name string) func() {
		return func() { mu.Lock(); got = append(got, name); mu.Unlock() }
	}
	now := clk.Now()
	s.At(now.Add(300*time.Millisecond), rec("c"))
	s.At(now.Add(100*time.Millisecond), rec("a1"))
	s.At(now.Add(100*time.Millisecond), rec("a2"))
	s.At(now, rec("now"))

	// The loop may handle the wake-up after Advance; poll for the result.
	clk.Advance(400 * time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 4 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := "[now a1 a2 c]"; fmtSlice(got) != want {
		t.Fatalf("order = %v, want %s", got, want)
	}
}

func fmtSlice(s []string) string {
	out := "["
	for i, v := range s {
		if i > 0 {
			out += " "
		}
		out += v
	}
	return out + "]"
}
