package marketdata

import (
	"hash/fnv"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/quynv/mock-fixser/internal/config"
)

// Level is one depth level.
type Level struct {
	Px, Size float64
}

// Quote is one tick of a symbol. Bids and Asks hold depth_levels levels,
// best first.
type Quote struct {
	Symbol     string
	Bid, Ask   float64
	Mid        float64
	Bids, Asks []Level
	Seq        uint64
	Time       time.Time
	// Decimals is the number of decimals of tick_size, for encoding.
	Decimals int32
	// FullRefresh is set for update_mode: snapshot (send W, not X).
	FullRefresh bool
}

// Generator produces quotes for one symbol. It is not safe for concurrent
// use; the engine calls it from its scheduler goroutine.
type Generator struct {
	cfg      config.Symbol
	model    PriceModel
	rng      *rand.Rand // model randomness
	aux      *rand.Rand // jitter and depth sizes, kept apart from the model
	decimals int32
	mid      float64
	seq      uint64
	last     time.Time
}

// NewGenerator builds a generator starting at mid, or at initial_price
// when mid is outside [min, max].
func NewGenerator(s config.Symbol, mid float64, now time.Time) (*Generator, error) {
	seed := symbolSeed(s)
	rng := rand.New(rand.NewPCG(seed, 1))
	model, err := newModel(s, rng)
	if err != nil {
		return nil, err
	}
	if mid < s.Min || mid > s.Max || mid == 0 {
		mid = s.InitialPrice
	}
	return &Generator{
		cfg:      s,
		model:    model,
		rng:      rng,
		aux:      rand.New(rand.NewPCG(seed, 2)),
		decimals: tickDecimals(s.TickSize),
		mid:      roundTick(mid, s.TickSize),
		last:     now,
	}, nil
}

// symbolSeed uses the configured seed, or a random one when unset.
func symbolSeed(s config.Symbol) uint64 {
	if s.Seed != nil {
		return uint64(*s.Seed)
	}
	h := fnv.New64a()
	h.Write([]byte(s.Symbol))
	return h.Sum64() ^ rand.Uint64()
}

// Interval returns the time to the next tick, including jitter.
func (g *Generator) Interval() time.Duration {
	sp := g.cfg.Speed
	d := time.Duration(sp.IntervalMs) * time.Millisecond
	if sp.TicksPerSecond > 0 {
		d = time.Duration(float64(time.Second) / sp.TicksPerSecond)
	}
	if j := sp.JitterMs; j > 0 {
		d += time.Duration(g.aux.IntN(2*j+1)-j) * time.Millisecond
	}
	return max(d, time.Millisecond)
}

// Current returns a quote for the current mid without advancing the model.
func (g *Generator) Current() Quote {
	return g.quote(g.mid, g.last)
}

// Tick advances the model to now and returns the new quote.
func (g *Generator) Tick(now time.Time) Quote {
	dt := now.Sub(g.last)
	g.last = now
	g.seq++

	if src, ok := g.model.(quoteSource); ok {
		bid, ask := src.NextQuote()
		q := g.book(roundTick(bid, g.cfg.TickSize), roundTick(ask, g.cfg.TickSize), now)
		g.mid = q.Mid
		return q
	}
	g.mid = roundTick(reflectInto(g.model.Next(g.mid, dt), g.cfg.Min, g.cfg.Max), g.cfg.TickSize)
	return g.quote(g.mid, now)
}

// quote derives bid/ask from mid: bid = mid − spread/2 rounded down to a
// tick, ask = bid + spread, so the spread stays exact.
func (g *Generator) quote(mid float64, now time.Time) Quote {
	tick := g.cfg.TickSize
	bid := math.Floor((mid-g.cfg.Spread/2)/tick+1e-9) * tick
	return g.book(roundTick(bid, tick), roundTick(bid+g.cfg.Spread, tick), now)
}

func (g *Generator) book(bid, ask float64, now time.Time) Quote {
	tick := g.cfg.TickSize
	n := max(g.cfg.DepthLevels, 1)
	q := Quote{
		Symbol:      g.cfg.Symbol,
		Bid:         bid,
		Ask:         ask,
		Mid:         roundTick((bid+ask)/2, tick/2),
		Bids:        make([]Level, n),
		Asks:        make([]Level, n),
		Seq:         g.seq,
		Time:        now,
		Decimals:    g.decimals,
		FullRefresh: g.cfg.UpdateMode == "snapshot",
	}
	for i := range n {
		q.Bids[i] = Level{Px: roundTick(bid-float64(i)*tick, tick), Size: g.levelSize()}
		q.Asks[i] = Level{Px: roundTick(ask+float64(i)*tick, tick), Size: g.levelSize()}
	}
	return q
}

func (g *Generator) levelSize() float64 {
	return float64(g.aux.IntN(10)+1) * 100
}

// reflectInto folds p back into [lo, hi]: above hi it becomes 2·hi − p.
func reflectInto(p, lo, hi float64) float64 {
	for i := 0; i < 4 && (p > hi || p < lo); i++ {
		if p > hi {
			p = 2*hi - p
		}
		if p < lo {
			p = 2*lo - p
		}
	}
	return math.Min(math.Max(p, lo), hi)
}

func roundTick(p, tick float64) float64 {
	if tick <= 0 {
		return p
	}
	r := math.Round(p/tick) * tick
	// Trim float noise such as 1.08500000000001.
	d := tickDecimals(tick)
	v, _ := strconv.ParseFloat(strconv.FormatFloat(r, 'f', int(d), 64), 64)
	return v
}

func tickDecimals(tick float64) int32 {
	s := strconv.FormatFloat(tick, 'f', -1, 64)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return int32(len(s) - i - 1)
	}
	return 0
}
