package mcx

import (
	"math"
	"time"

	"serene/internal/cost"
	"serene/internal/dow"
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
	LongOnly   bool                              // flat, not short, while fast is under slow
	MinGap     float64                           // percent of slow the EMAs must part by to flip; inside it the side is kept
	MA         func([]market.Bar, int) []float64 // average used for both lines; nil is indicator.EMA
	SlowMA     func([]market.Bar, int) []float64 // average of the slow line only; nil is MA
	Slope      bool                              // compare fast with itself two frame bars back, not with slow
	// StructPct > 0 gates the side by Dow structure on the frame bars: a
	// zigzag of StructPct percent of the contract's mean close marks swings,
	// and long needs the last high HH and last low HL, short LH and LL.
	StructPct  float64
	StructMode StructMode
	// StructN > 2 reads the last StructN swing highs and lows instead of the
	// last one of each, with StructRule deciding what counts as rising.
	StructN    int
	StructRule StructRule
	Slip       int64
	Costs      *cost.Schedule
	From, To   market.Date
}

// StructMode says when the structure gate is applied.
type StructMode int

const (
	StructTurn   StructMode = iota // only at a turn: a filtered turn stays flat until the next turn
	StructLate                     // also enter later, once structure agrees with the side already turned to
	StructStrict                   // also exit when structure stops agreeing
)

func (m StructMode) String() string { return [...]string{"turn", "late", "strict"}[m] }

// StructRule decides whether the last N swing highs (or lows) are rising.
type StructRule int

const (
	RuleAll  StructRule = iota // every step from one swing to the next rises
	RuleMost                   // at least three quarters of the steps rise
	RuleEnds                   // the latest swing is above the oldest of the N
)

func (r StructRule) String() string { return [...]string{"all", "most", "ends"}[r] }

// trendN reads the trend from the last n swing highs and lows: up when both
// rise by rule, down when both fall, else mixed. Fewer than n of either is
// unknown.
func trendN(swings []dow.Swing, n int, rule StructRule) dow.Trend {
	var hs, ls []market.Paise // newest first
	for i := len(swings) - 1; i >= 0 && (len(hs) < n || len(ls) < n); i-- {
		if swings[i].High && len(hs) < n {
			hs = append(hs, swings[i].Price)
		} else if !swings[i].High && len(ls) < n {
			ls = append(ls, swings[i].Price)
		}
	}
	if len(hs) < n || len(ls) < n {
		return dow.Unknown
	}
	dir := func(p []market.Paise) int { // 1 rising, -1 falling, 0 neither
		if rule == RuleEnds {
			switch {
			case p[0] > p[n-1]:
				return 1
			case p[0] < p[n-1]:
				return -1
			}
			return 0
		}
		up, down := 0, 0
		for i := 0; i+1 < n; i++ {
			if p[i] > p[i+1] {
				up++
			} else if p[i] < p[i+1] {
				down++
			}
		}
		need := n - 1
		if rule == RuleMost {
			need = (3*(n-1) + 3) / 4
		}
		switch {
		case up >= need:
			return 1
		case down >= need:
			return -1
		}
		return 0
	}
	h, l := dir(hs), dir(ls)
	switch {
	case h == 1 && l == 1:
		return dow.Up
	case h == -1 && l == -1:
		return dow.Down
	}
	return dow.Mixed
}

// EMABacktest trades an EMA crossover on the active contract. Each contract's
// EMAs come from its own frame bars. At every frame close the wanted side is
// long when fast > slow and short (or flat if LongOnly) when under; a change
// fills at the next minute's open. Positions hold overnight and exit at a
// roll; the new contract is entered on its first minute in the wanted side.
// With MinGap, a side is wanted only once fast is more than MinGap percent of
// slow above (or below) it; a thinner cross keeps the side already wanted.
// Trade.R is the move as a percent of the entry price.
func EMABacktest(ed *EMAData, cfg EMAConfig) ([]Trade, error) {
	sr := ed.sr
	fast, slow := map[string][]float64{}, map[string][]float64{}
	ma := cfg.MA
	if ma == nil {
		ma = indicator.EMA
	}
	slowMA := cfg.SlowMA
	if slowMA == nil {
		slowMA = ma
	}
	for k, f := range sr.frames[cfg.Frame] {
		fast[k] = ma(f, cfg.Fast)
		if cfg.Slope {
			s := make([]float64, len(f))
			for i := range s {
				s[i] = math.NaN()
				if i >= 2 {
					s[i] = fast[k][i-2]
				}
			}
			slow[k] = s
		} else {
			slow[k] = slowMA(f, cfg.Slow)
		}
	}
	swings := map[string][]dow.Swing{}
	if cfg.StructPct > 0 {
		for k, f := range sr.frames[cfg.Frame] {
			if len(f) == 0 {
				continue
			}
			var sum float64
			for _, b := range f {
				sum += float64(b.Close)
			}
			th := market.Paise(sum / float64(len(f)) * cfg.StructPct / 100)
			swings[k], _ = dow.Swings(f, max(th, 1))
		}
	}
	var raw market.Side // the MA's own side, before the gate
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
			if !ok || math.IsNaN(sk[i]) || math.IsNaN(fk[i]) {
				continue
			}
			band := sk[i] * cfg.MinGap / 100
			prev := raw
			switch {
			case fk[i]-sk[i] > band:
				raw = market.Buy
			case sk[i]-fk[i] > band && cfg.LongOnly:
				raw = 0
			case sk[i]-fk[i] > band:
				raw = market.Sell
			}
			if cfg.StructPct <= 0 {
				want = raw
				continue
			}
			known := dow.At(swings[key], m.End)
			tr := dow.TrendOf(known)
			if cfg.StructN > 2 {
				tr = trendN(known, cfg.StructN, cfg.StructRule)
			}
			agree := (raw == market.Buy && tr == dow.Up) || (raw == market.Sell && tr == dow.Down)
			gated := market.Side(0)
			if agree {
				gated = raw
			}
			switch {
			case raw != prev || cfg.StructMode == StructStrict:
				want = gated
			case cfg.StructMode == StructLate && want == 0:
				want = gated
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
