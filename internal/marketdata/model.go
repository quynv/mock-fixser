// Package marketdata generates prices per symbol and publishes them over
// FIX as W/X/Y in answer to V (design doc, section 6).
package marketdata

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/quynv/mock-fixser/internal/config"
)

// PriceModel produces the next mid from the previous one (section 6.1).
// The generator then reflects the result into [min, max] and rounds it.
type PriceModel interface {
	Next(prev float64, dt time.Duration) float64
}

// quoteSource is implemented by models that supply bid and ask directly
// (replay), bypassing the spread computation.
type quoteSource interface {
	NextQuote() (bid, ask float64)
}

// randomWalk: p' = p + U(-v, +v), v = volatility in price units
// (default one tick).
type randomWalk struct {
	v   float64
	rng *rand.Rand
}

func (m *randomWalk) Next(prev float64, _ time.Duration) float64 {
	return prev + (m.rng.Float64()*2-1)*m.v
}

// gbm: p' = p · exp((μ − σ²/2)·dt + σ·√dt·Z), with dt in seconds, σ =
// volatility per √second and μ = drift per second.
type gbm struct {
	mu, sigma float64
	rng       *rand.Rand
}

func (m *gbm) Next(prev float64, dt time.Duration) float64 {
	s := dt.Seconds()
	return prev * math.Exp((m.mu-m.sigma*m.sigma/2)*s+m.sigma*math.Sqrt(s)*m.rng.NormFloat64())
}

// sine: p = center + amplitude · sin(2π·t / period) + U(-noise, +noise),
// where t is the time since the generator started and noise = volatility.
type sine struct {
	center, amplitude, noise float64
	period                   time.Duration
	elapsed                  time.Duration
	rng                      *rand.Rand
}

func (m *sine) Next(_ float64, dt time.Duration) float64 {
	m.elapsed += dt
	p := m.center + m.amplitude*math.Sin(2*math.Pi*float64(m.elapsed)/float64(m.period))
	if m.noise > 0 {
		p += (m.rng.Float64()*2 - 1) * m.noise
	}
	return p
}

// replay reads one CSV row per tick. Rows hold bid and ask, or mid; the
// header names the columns (time or time_ms is ignored here).
type replay struct {
	bids, asks []float64
	loop       bool
	pos        int
}

func (m *replay) Next(prev float64, _ time.Duration) float64 {
	bid, ask := m.NextQuote()
	return (bid + ask) / 2
}

func (m *replay) NextQuote() (bid, ask float64) {
	if m.pos >= len(m.bids) {
		if m.loop {
			m.pos = 0
		} else {
			m.pos = len(m.bids) - 1 // hold the last row
		}
	}
	bid, ask = m.bids[m.pos], m.asks[m.pos]
	m.pos++
	return bid, ask
}

func loadReplay(r *config.Replay) (*replay, error) {
	f, err := os.Open(r.File)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cr := csv.NewReader(f)
	cr.TrimLeadingSpace = true

	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("%s: read header: %w", r.File, err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	bidCol, hasBid := col["bid"]
	askCol, hasAsk := col["ask"]
	midCol, hasMid := col["mid"]
	if !(hasBid && hasAsk) && !hasMid {
		return nil, fmt.Errorf("%s: header needs bid and ask, or mid", r.File)
	}

	m := &replay{loop: r.Loop}
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.File, err)
		}
		num := func(i int) (float64, error) {
			v, err := strconv.ParseFloat(strings.TrimSpace(rec[i]), 64)
			if err != nil {
				return 0, fmt.Errorf("%s line %d: %w", r.File, line, err)
			}
			return v, nil
		}
		var bid, ask float64
		if hasBid && hasAsk {
			if bid, err = num(bidCol); err != nil {
				return nil, err
			}
			if ask, err = num(askCol); err != nil {
				return nil, err
			}
		} else {
			if bid, err = num(midCol); err != nil {
				return nil, err
			}
			ask = bid
		}
		m.bids, m.asks = append(m.bids, bid), append(m.asks, ask)
	}
	if len(m.bids) == 0 {
		return nil, fmt.Errorf("%s: no rows", r.File)
	}
	return m, nil
}

func newModel(s config.Symbol, rng *rand.Rand) (PriceModel, error) {
	switch s.Model {
	case "", "random_walk":
		v := s.Volatility
		if v == 0 {
			v = s.TickSize
		}
		return &randomWalk{v: v, rng: rng}, nil
	case "gbm":
		return &gbm{mu: s.Drift, sigma: s.Volatility, rng: rng}, nil
	case "sine":
		return &sine{
			center:    s.InitialPrice,
			amplitude: s.Sine.Amplitude,
			period:    time.Duration(s.Sine.PeriodMs) * time.Millisecond,
			noise:     s.Volatility,
			rng:       rng,
		}, nil
	case "replay":
		return loadReplay(s.Replay)
	}
	return nil, fmt.Errorf("unknown model %q", s.Model)
}
