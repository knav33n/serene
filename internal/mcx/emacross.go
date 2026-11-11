package mcx

import (
	"math"
	"time"

	"serene/internal/cost"
	"serene/internal/indicator"
	"serene/internal/market"
)

// EMAData is one commodity's minute bars prepared once for many EMA runs.
type EMAData struct {
	sr     *series
	frames []time.Duration
}

// PrepareEMA splits bars by contract and buckets each contract's own bars
// into the given frames.
func PrepareEMA(bars []market.Bar, frames ...time.Duration) *EMAData {
	return &EMAData{sr: prepare(bars, frames...), frames: frames}
}

// EMAConfig sets up one EMA crossover run.
type EMAConfig struct {
	Name       string
	Spec       Spec
	Frame      time.Duration
	Fast, Slow int
	LongOnly   bool // flat, not short, while fast is under slow
	Slip       int64
	Costs      *cost.Schedule
	From, To   market.Date
}

// EMABacktest trades an EMA crossover on the active contract. Each contract's
// EMAs come from its own frame bars. At every frame close the wanted side is
// long when fast > slow and short (or flat if LongOnly) when under; a change
// fills at the next minute's open. Positions hold overnight and exit at a
// roll; the new contract is entered on its first minute in the wanted side.
// Trade.R is the move as a percent of the entry price.
func EMABacktest(ed *EMAData, cfg EMAConfig) ([]Trade, error) {
	sr := ed.sr
	fast, slow := map[string][]float64{}, map[string][]float64{}
	for k, f := range sr.frames[cfg.Frame] {
		fast[k], slow[k] = indicator.EMA(f, cfg.Fast), indicator.EMA(f, cfg.Slow)
	}
	var (
		trades []Trade
		pos    *position
		last   market.Bar
		want   market.Side // 0 flat
	)
	closePos := func(at time.Time, raw market.Paise, why string) error {
		t, err := settle(cfg.Name, cfg.Spec, cfg.Slip, cfg.Costs, pos, at, raw, why)
		if err != nil {
			return err
		}
		trades = append(trades, t)
		pos = nil
		return nil
	}
	open := func(m market.Bar, key string) {
		pos = &position{order: order{side: want, kind: "cross", signal: m.Time}, key: key, entry: m.Time, in: m.Open}
		pos.risk = max(m.Open/100, 1) // R = percent move
	}
	for _, d := range sr.days {
		if d.Before(cfg.From) || cfg.To.Before(d) {
			continue
		}
		key, ok := sr.active[d]
		if !ok {
			continue
		}
		if pos != nil && pos.key != key {
			old := filterKey(sr.byDay[d], pos.key)
			at, px := last.End, last.Close
			if len(old) > 0 {
				at, px = old[0].Time, old[0].Open
			}
			if err := closePos(at, px, "roll"); err != nil {
				return nil, err
			}
		}
		closes := sr.closes[cfg.Frame][key]
		fk, sk := fast[key], slow[key]
		for _, m := range filterKey(sr.byDay[d], key) {
			if pos != nil && pos.side != want {
				if err := closePos(m.Time, m.Open, "cross"); err != nil {
					return nil, err
				}
			}
			if pos == nil && want != 0 {
				open(m, key)
			}
			if pos != nil {
				last = m
			}
			i, ok := closes[m.End]
			if !ok || math.IsNaN(sk[i]) {
				continue
			}
			switch {
			case fk[i] > sk[i]:
				want = market.Buy
			case fk[i] < sk[i] && cfg.LongOnly:
				want = 0
			case fk[i] < sk[i]:
				want = market.Sell
			}
		}
	}
	if pos != nil {
		if err := closePos(last.End, last.Close, "end"); err != nil {
			return nil, err
		}
	}
	return trades, nil
}
