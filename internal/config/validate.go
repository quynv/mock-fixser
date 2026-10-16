package config

import (
	"fmt"
	"math"
	"os"
	"regexp"
)

// validate runs the cross-field checks the JSON Schema cannot express
// (design doc, section 9.2). Paths are filled in; lines are added by Parse.
func validate(c *Config) []FieldError {
	var errs []FieldError
	add := func(path, format string, args ...any) {
		errs = append(errs, FieldError{Path: path, Message: fmt.Sprintf(format, args...)})
	}

	symbols := map[string]int{}
	for i, s := range c.Symbols {
		p := fmt.Sprintf("symbols[%d]", i)
		if j, dup := symbols[s.Symbol]; dup {
			add(p+".symbol", "duplicate symbol %q (also symbols[%d])", s.Symbol, j)
		} else {
			symbols[s.Symbol] = i
		}
		if s.Min >= s.Max {
			add(p+".max", "max (%v) must be greater than min (%v)", s.Max, s.Min)
		}
		if s.InitialPrice < s.Min || s.InitialPrice > s.Max {
			add(p+".initial_price", "initial_price (%v) must be within [min, max] = [%v, %v]", s.InitialPrice, s.Min, s.Max)
		}
		if s.TickSize > 0 && !isMultiple(s.Spread, s.TickSize) {
			add(p+".spread", "spread (%v) must be a multiple of tick_size (%v)", s.Spread, s.TickSize)
		}
		hasTPS, hasInterval := s.Speed.TicksPerSecond > 0, s.Speed.IntervalMs > 0
		if hasTPS == hasInterval {
			add(p+".speed", "set exactly one of ticks_per_second or interval_ms")
		}
		switch s.Model {
		case "sine":
			if s.Sine == nil {
				add(p+".sine", "model sine requires a sine section (amplitude, period_ms)")
			}
		case "replay":
			if s.Replay == nil {
				add(p+".replay", "model replay requires a replay section with a file")
			} else if f, err := os.Open(s.Replay.File); err != nil {
				add(p+".replay.file", "replay file is not readable: %v", err)
			} else {
				f.Close()
			}
		}
		if sl := s.Slippage; sl != nil && sl.Type == "random" && sl.MinTicks > sl.MaxTicks {
			add(p+".slippage.max_ticks", "max_ticks (%d) must be >= min_ticks (%d)", sl.MaxTicks, sl.MinTicks)
		}
	}

	ruleNames := map[string]int{}
	priorities := map[int]int{}
	for i, r := range c.OrderRules {
		p := fmt.Sprintf("order_rules[%d]", i)
		if j, dup := ruleNames[r.Name]; dup {
			add(p+".name", "duplicate rule name %q (also order_rules[%d])", r.Name, j)
		} else {
			ruleNames[r.Name] = i
		}
		if j, dup := priorities[r.Priority]; dup {
			add(p+".priority", "duplicate priority %d (also order_rules[%d])", r.Priority, j)
		} else {
			priorities[r.Priority] = i
		}
		checkRegexes(p+".match", r.Match, add)
	}

	sessions := map[SessionKey]int{}
	for i, s := range c.Server.FIX.Sessions {
		p := fmt.Sprintf("server.fix.sessions[%d]", i)
		if j, dup := sessions[s.Key()]; dup {
			add(p, "duplicate session %s (also server.fix.sessions[%d])", s.Key(), j)
		} else {
			sessions[s.Key()] = i
		}
		if s.TargetCompID == c.Server.Monitor.TargetCompID {
			add(p+".target_comp_id", "target_comp_id %q is reserved for the monitor session", s.TargetCompID)
		}
	}

	return errs
}

// checkRegexes compiles every { regex: "..." } found in a match section.
func checkRegexes(path string, m map[string]any, add func(path, format string, args ...any)) {
	for k, v := range m {
		sub, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if re, ok := sub["regex"]; ok {
			s, isStr := re.(string)
			if !isStr {
				add(path+"."+k+".regex", "regex must be a string")
			} else if _, err := regexp.Compile(s); err != nil {
				add(path+"."+k+".regex", "invalid regex: %v", err)
			}
			continue
		}
		checkRegexes(path+"."+k, sub, add)
	}
}

func isMultiple(v, step float64) bool {
	r := v / step
	return math.Abs(r-math.Round(r)) < 1e-6
}
