package clock

import (
	"testing"
	"time"
)

func TestFakeTimersFireInOrder(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f := NewFake(start)
	a := f.NewTimer(2 * time.Second)
	b := f.NewTimer(1 * time.Second)

	f.Advance(500 * time.Millisecond)
	select {
	case <-b.C():
		t.Fatal("fired early")
	default:
	}

	f.Advance(2 * time.Second)
	if got := <-b.C(); !got.Equal(start.Add(time.Second)) {
		t.Errorf("b fired at %v", got)
	}
	if got := <-a.C(); !got.Equal(start.Add(2 * time.Second)) {
		t.Errorf("a fired at %v", got)
	}
	if !f.Now().Equal(start.Add(2500 * time.Millisecond)) {
		t.Errorf("now = %v", f.Now())
	}
}

func TestFakeStopAndReset(t *testing.T) {
	f := NewFake(time.Unix(0, 0))
	tm := f.NewTimer(time.Second)
	if !tm.Stop() {
		t.Fatal("Stop on active timer returned false")
	}
	f.Advance(2 * time.Second)
	select {
	case <-tm.C():
		t.Fatal("stopped timer fired")
	default:
	}
	if tm.Reset(time.Second) {
		t.Fatal("Reset on stopped timer returned true")
	}
	f.Advance(time.Second)
	<-tm.C()
}

func TestFakeBlockUntil(t *testing.T) {
	f := NewFake(time.Unix(0, 0))
	done := make(chan struct{})
	go func() {
		f.BlockUntil(1)
		close(done)
	}()
	f.NewTimer(time.Second)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("BlockUntil did not return")
	}
}
