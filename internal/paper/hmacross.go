package paper

import (
	"math"
	"time"

	"serene/internal/indicator"
	"serene/internal/market"
)

// HMACrossName is the crossover signal's name in messages and records. It
// replaced nifty_hma_retest on 2026-11-22: signals only, no orders.
const HMACrossName = "nifty_hma_crossover"

// CrossConfig sets nifty_hma_crossover's rules.
type CrossConfig struct {
	Day        market.Date
	Fast, Slow int              // Hull moving averages of closes
	TF         market.Timeframe // bar size the legs' bars are in
}

// DefaultHMACross is the configuration chosen on 2026-11-22: 10-minute bars,
// HMA 13/34, every cross of the day from 09:15.
func DefaultHMACross(d market.Date) CrossConfig {
	return CrossConfig{Day: d, Fast: 13, Slow: 34, TF: 10}
}

// Cross is one bar's close where the fast HMA crossed the slow one.
type Cross struct {
	Instrument market.Instrument
	Time, End  time.Time // the crossing bar
	Up         bool      // fast closed above slow, after being at or below it
	Close      market.Paise
	Fast, Slow float64
}

// HMACrosses lists every cross on today's bars of each leg, oldest first
// within a leg. Bars before today only warm the averages; a bar whose
// average, or the one before it, isn't defined yet can't cross.
func HMACrosses(legs []Leg, cfg CrossConfig) []Cross {
	var out []Cross
	for _, l := range legs {
		bars := l.Bars
		ef, es := indicator.HMA(bars, cfg.Fast), indicator.HMA(bars, cfg.Slow)
		for i := max(today(bars, cfg.Day), 1); i < len(bars); i++ {
			if math.IsNaN(ef[i]) || math.IsNaN(es[i]) || math.IsNaN(ef[i-1]) || math.IsNaN(es[i-1]) {
				continue
			}
			now, before := ef[i] > es[i], ef[i-1] > es[i-1]
			if now == before {
				continue
			}
			b := bars[i]
			out = append(out, Cross{Instrument: l.Instrument, Time: b.Time, End: b.End, Up: now, Close: b.Close, Fast: ef[i], Slow: es[i]})
		}
	}
	return out
}
