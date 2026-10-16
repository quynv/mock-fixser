package marketdata

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/quynv/mock-fixser/internal/config"
)

var t0 = time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)

func seed(v int64) *int64 { return new(v) }

func eurusd(model string) config.Symbol {
	return config.Symbol{
		Symbol: "EURUSD", InitialPrice: 1.0850, Min: 1.0700, Max: 1.1000, TickSize: 0.00001,
		Model: model, Volatility: 0.002, Spread: 0.00010, DepthLevels: 3,
		Speed: config.Speed{TicksPerSecond: 5}, Seed: seed(42),
		Sine: &config.Sine{Amplitude: 0.01, PeriodMs: 10000},
	}
}

func onTick(p, tick float64) bool {
	r := p / tick
	return math.Abs(r-math.Round(r)) < 1e-6
}

func TestModelsStayInRangeOnTick(t *testing.T) {
	for _, model := range []string{"random_walk", "gbm", "sine"} {
		t.Run(model, func(t *testing.T) {
			s := eurusd(model)
			s.Volatility = 0.05 // large moves to hit the bounds often
			g, err := NewGenerator(s, 0, t0)
			if err != nil {
				t.Fatal(err)
			}
			now := t0
			for i := range 5000 {
				now = now.Add(200 * time.Millisecond)
				q := g.Tick(now)
				if q.Mid < s.Min || q.Mid > s.Max {
					t.Fatalf("tick %d: mid %v outside [%v, %v]", i, q.Mid, s.Min, s.Max)
				}
				if !onTick(q.Bid, s.TickSize) || !onTick(q.Ask, s.TickSize) {
					t.Fatalf("tick %d: bid %v / ask %v not on tick size", i, q.Bid, q.Ask)
				}
				if spread := q.Ask - q.Bid; math.Abs(spread-s.Spread) > 1e-9 {
					t.Fatalf("tick %d: spread %v, want %v", i, spread, s.Spread)
				}
				if len(q.Bids) != 3 || q.Bids[0].Px != q.Bid || q.Asks[2].Px <= q.Asks[1].Px || q.Bids[2].Px >= q.Bids[1].Px {
					t.Fatalf("tick %d: bad depth %+v %+v", i, q.Bids, q.Asks)
				}
				if q.Seq != uint64(i+1) {
					t.Fatalf("seq = %d", q.Seq)
				}
			}
		})
	}
}

func TestSameSeedSameSequence(t *testing.T) {
	for _, model := range []string{"random_walk", "gbm", "sine"} {
		s := eurusd(model)
		s.Speed.JitterMs = 30
		a, _ := NewGenerator(s, 0, t0)
		b, _ := NewGenerator(s, 0, t0)
		now := t0
		for range 1000 {
			da, db := a.Interval(), b.Interval()
			if da != db {
				t.Fatalf("%s: intervals differ %v %v", model, da, db)
			}
			now = now.Add(da)
			if qa, qb := a.Tick(now), b.Tick(now); qa.Bid != qb.Bid || qa.Asks[1] != qb.Asks[1] {
				t.Fatalf("%s: quotes differ %+v %+v", model, qa, qb)
			}
		}
	}

	other := eurusd("gbm")
	other.Seed = seed(7)
	a, _ := NewGenerator(eurusd("gbm"), 0, t0)
	b, _ := NewGenerator(other, 0, t0)
	same := true
	for i := range 20 {
		now := t0.Add(time.Duration(i+1) * time.Second)
		if a.Tick(now).Bid != b.Tick(now).Bid {
			same = false
		}
	}
	if same {
		t.Fatal("different seeds produced the same sequence")
	}
}

func TestKeepsMidWhenInRange(t *testing.T) {
	g, _ := NewGenerator(eurusd("gbm"), 1.09, t0)
	if g.Current().Mid != 1.09 {
		t.Fatalf("mid = %v, want 1.09", g.Current().Mid)
	}
	g, _ = NewGenerator(eurusd("gbm"), 1.5, t0)
	if g.Current().Mid != 1.085 {
		t.Fatalf("mid = %v, want initial 1.085", g.Current().Mid)
	}
}

func TestOddTickSpreadStaysExact(t *testing.T) {
	s := config.Symbol{Symbol: "X", InitialPrice: 100, Min: 90, Max: 110, TickSize: 0.01,
		Spread: 0.03, Speed: config.Speed{IntervalMs: 10}, Seed: seed(1)}
	g, _ := NewGenerator(s, 0, t0)
	q := g.Current()
	if q.Bid != 99.98 || q.Ask != 100.01 {
		t.Fatalf("bid/ask = %v/%v, want 99.98/100.01", q.Bid, q.Ask)
	}
}

func TestReplayModel(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "r.csv")
	os.WriteFile(file, []byte("time_ms,bid,ask\n0,10.00,10.02\n500,10.01,10.04\n"), 0o644)
	s := config.Symbol{Symbol: "R", InitialPrice: 10, Min: 5, Max: 15, TickSize: 0.01,
		Model: "replay", Replay: &config.Replay{File: file, Loop: true}, Speed: config.Speed{IntervalMs: 500}}
	g, err := NewGenerator(s, 0, t0)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]float64{{10.00, 10.02}, {10.01, 10.04}, {10.00, 10.02}}
	for i, w := range want {
		q := g.Tick(t0.Add(time.Duration(i+1) * time.Second))
		if q.Bid != w[0] || q.Ask != w[1] {
			t.Fatalf("row %d: %v/%v, want %v", i, q.Bid, q.Ask, w)
		}
	}

	s.Replay.Loop = false
	g, _ = NewGenerator(s, 0, t0)
	for range 5 {
		g.Tick(t0)
	}
	if q := g.Tick(t0); q.Bid != 10.01 {
		t.Fatalf("without loop the last row should repeat, got %v", q.Bid)
	}
}

func TestReflect(t *testing.T) {
	for _, c := range []struct{ in, want float64 }{
		{5, 5}, {11, 9}, {-1, 1}, {10, 10}, {0, 0}, {35, 5},
	} {
		if got := reflectInto(c.in, 0, 10); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("reflectInto(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
