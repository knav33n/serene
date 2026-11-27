package paper

import (
	"math"
	"sort"
	"time"

	"serene/internal/indicator"
	"serene/internal/market"
)

// HMACrossName is the crossover signal's name in messages and records. It
// replaced nifty_hma_retest on 2026-11-22; it was nifty_hma_crossover until
// SENSEX was added on 2026-11-26.
const HMACrossName = "hma_crossover"

// CrossConfig sets hma_crossover's rules.
type CrossConfig struct {
	Day        market.Date
	Fast, Slow int              // Hull moving averages of closes
	TF         market.Timeframe // bar size the legs' bars are in
	// NearPct: after an up-cross, a minute whose low comes within this
	// fraction above the nearest HMA (or under it) is a pullback alert.
	NearPct float64
}

// DefaultHMACross is the configuration chosen on 2026-11-22: 10-minute bars,
// HMA 13/34, every cross of the day from 09:15.
func DefaultHMACross(d market.Date) CrossConfig {
	return CrossConfig{Day: d, Fast: 13, Slow: 34, TF: 10, NearPct: 0.02}
}

// Cross is one bar's close where the fast HMA crossed the slow one.
type Cross struct {
	Instrument market.Instrument
	Time, End  time.Time // the crossing bar
	Up         bool      // fast closed above slow, after being at or below it
	Close      market.Paise
	Fast, Slow float64
}

// HMACrosses lists every cross on today's bars of each leg, oldest first. Bars before today only warm the averages; a bar whose
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
	sort.SliceStable(out, func(a, b int) bool { return out[a].Time.Before(out[b].Time) })
	return out
}

// Near is the first minute after an up-cross whose low came within
// CrossConfig.NearPct of the nearer HMA of the last closed bar.
type Near struct {
	Cross  Cross
	Time   time.Time // the minute bar's start
	Low    market.Paise
	Period int     // which HMA was nearer: Fast or Slow
	MA     float64 // its value on the last bar closed by Time
}

// HMANears lists, for each up-cross in crosses, the first of its leg's
// minute bars (Leg.Ones) from the crossing bar's end until the leg's next
// cross is known whose low is at most (1+NearPct) × the HMA nearer to it.
// The averages are those of the last bar of Leg.Bars that closed by the
// minute's start, so they don't move within the minute. Oldest first.
func HMANears(legs []Leg, crosses []Cross, cfg CrossConfig) []Near {
	var out []Near
	for _, l := range legs {
		bars := l.Bars
		ef, es := indicator.HMA(bars, cfg.Fast), indicator.HMA(bars, cfg.Slow)
		var mine []Cross
		for _, c := range crosses {
			if c.Instrument.Key() == l.Instrument.Key() {
				mine = append(mine, c)
			}
		}
		for k, c := range mine {
			if !c.Up {
				continue
			}
			var stop time.Time // when the next cross is known
			if k+1 < len(mine) {
				stop = mine[k+1].End
			}
			for _, m := range l.Ones {
				if m.Time.Before(c.End) {
					continue
				}
				if !stop.IsZero() && !m.Time.Before(stop) {
					break
				}
				// The last bar closed by m.Time.
				j := sort.Search(len(bars), func(i int) bool { return bars[i].End.After(m.Time) }) - 1
				if j < 0 || math.IsNaN(ef[j]) || math.IsNaN(es[j]) {
					continue
				}
				low := m.Low.Rupees()
				period, ma := cfg.Fast, ef[j]
				if math.Abs(low-es[j]) < math.Abs(low-ma) {
					period, ma = cfg.Slow, es[j]
				}
				if low <= ma*(1+cfg.NearPct) {
					out = append(out, Near{Cross: c, Time: m.Time, Low: m.Low, Period: period, MA: ma})
					break
				}
			}
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Time.Before(out[b].Time) })
	return out
}
