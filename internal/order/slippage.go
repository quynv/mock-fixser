package order

import (
	"math/rand/v2"

	"github.com/shopspring/decimal"

	"github.com/quynv/mock-fixser/internal/config"
)

// isBuy reports whether a FIX Side buys (Buy, Buy minus).
func isBuy(side string) bool { return side == "1" || side == "3" }

// slip moves px against the client by the symbol's slippage: up for buys,
// down for sells (section 7.4). The result is rounded to the tick size.
func slip(px decimal.Decimal, side string, s *config.Slippage, tick decimal.Decimal, rng *rand.Rand) decimal.Decimal {
	if s == nil || s.Type == "" || s.Type == "none" {
		return px
	}
	var amount decimal.Decimal
	switch s.Type {
	case "fixed":
		amount = tick.Mul(decimal.NewFromInt(int64(s.Ticks)))
	case "percent":
		amount = px.Mul(decimal.NewFromFloat(s.Value)).Div(decimal.NewFromInt(100))
	case "random":
		n := s.MinTicks
		if s.MaxTicks > s.MinTicks {
			n += rng.IntN(s.MaxTicks - s.MinTicks + 1)
		}
		amount = tick.Mul(decimal.NewFromInt(int64(n)))
	}
	if !isBuy(side) {
		amount = amount.Neg()
	}
	return roundToTick(px.Add(amount), tick, isBuy(side))
}

// roundToTick rounds away from the client: up for buys, down for sells, so
// rounding never gives back slippage.
func roundToTick(px, tick decimal.Decimal, up bool) decimal.Decimal {
	if !tick.IsPositive() {
		return px
	}
	n := px.Div(tick)
	if up {
		n = n.Ceil()
	} else {
		n = n.Floor()
	}
	return n.Mul(tick)
}
