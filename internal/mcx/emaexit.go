package mcx

import (
	"math"
	"time"

	"serene/internal/indicator"
	"serene/internal/market"
)

// EMAExits are exit rules added to the EMA crossover. Zero values are off, and
// with all of them off EMAExitBacktest trades exactly as EMABacktest does.
// Percentages are of the entry price; ATR is the 14-bar ATR of the signal frame
// at its last close.
type EMAExits struct {
	StopPct      float64 // fixed stop
	StopATR      float64 // fixed stop, ATR multiples from entry
	TrailPct     float64 // trail from the best price since entry
	TrailATR     float64 // chandelier trail: best price since entry less k ATR
	TargetPct    float64 // fixed profit target
	BreakevenPct float64 // stop to entry once this far in profit
	TimeBars     int     // out at a frame close this many frames in, if not in profit
	CloseSlow    bool    // out when a frame closes on the wrong side of the slow EMA
	CloseFast    bool    // out when a frame closes on the wrong side of the fast EMA
	Donchian     int     // out when a frame closes beyond the last N frames' opposite extreme
	FlatEOD      bool    // out at each session's last minute; back in at the next session's first
	// After a rule exit the side is wanted again only after the next cross,
	// unless ReEnter: then a frame close beyond the fast EMA puts it back on.
	ReEnter bool
}

// EMAExitResult is the trades and the mark-to-market drawdown: the largest
// fall of booked plus open gross P&L, minute by minute.
type EMAExitResult struct {
	Trades []Trade
	MTMDD  float64 // rupees
}

// EMAExitBacktest is EMABacktest with exit rules. Stops and targets are checked
// on every minute's range: a gap through the level fills at the open, and when
// a minute touches both, the stop is taken. Frame-close rules fill at the next
// minute's open, like the cross.
func EMAExitBacktest(ed *EMAData, cfg EMAConfig, ex EMAExits) (EMAExitResult, error) {
	sr := ed.sr
	fast, slow, atr := map[string][]float64{}, map[string][]float64{}, map[string][]float64{}
	for k, f := range sr.frames[cfg.Frame] {
		fast[k], slow[k], atr[k] = indicator.EMA(f, cfg.Fast), indicator.EMA(f, cfg.Slow), indicator.ATR(f, 14)
	}
	var (
		res      EMAExitResult
		pos      *position
		last     market.Bar
		want     market.Side
		blocked  bool   // a rule exit: wait for the next cross (or ReEnter)
		exitNext string // a frame-close rule fired: out at the next open
		best     market.Paise
		bars     int     // frame closes since entry
		atrNow   float64 // at the last frame close
		booked   float64
		peak, dd float64
	)
	mult := float64(cfg.Spec.Multiplier)
	closePos := func(at time.Time, raw market.Paise, why string) error {
		t, err := settle(cfg.Name, cfg.Spec, cfg.Slip, cfg.Costs, pos, at, raw, why)
		if err != nil {
			return err
		}
		res.Trades = append(res.Trades, t)
		booked += t.Net.Rupees()
		pos = nil
		exitNext = ""
		return nil
	}
	open := func(m market.Bar, key string) {
		pos = &position{order: order{side: want, kind: "cross", signal: m.Time}, key: key, entry: m.Time, in: m.Open}
		pos.risk = max(m.Open/100, 1)
		best, bars = m.Open, 0
		pos.stop = 0
		s := float64(want)
		in := m.Open.Rupees()
		lvl := math.NaN()
		if ex.StopPct > 0 {
			lvl = in * (1 - s*ex.StopPct/100)
		}
		if ex.StopATR > 0 && !math.IsNaN(atrNow) {
			l := in - s*ex.StopATR*atrNow
			if math.IsNaN(lvl) || s*(l-lvl) > 0 {
				lvl = l
			}
		}
		if !math.IsNaN(lvl) {
			pos.stop = market.Paise(math.Round(lvl * 100))
		}
		if ex.TargetPct > 0 {
			pos.tgt = market.Paise(math.Round(in * (1 + s*ex.TargetPct/100) * 100))
		}
	}
	// stopLevel is the tightest stop now in force, 0 for none.
	stopLevel := func() market.Paise {
		s := market.Paise(pos.side)
		lv := pos.stop
		tighter := func(l market.Paise) {
			if lv == 0 || s*(l-lv) > 0 {
				lv = l
			}
		}
		if ex.TrailPct > 0 {
			tighter(best - s*market.Paise(math.Round(best.Rupees()*ex.TrailPct)))
		}
		if ex.TrailATR > 0 && !math.IsNaN(atrNow) {
			tighter(best - s*market.Paise(math.Round(ex.TrailATR*atrNow*100)))
		}
		if ex.BreakevenPct > 0 && s*(best-pos.in) >= market.Paise(math.Round(pos.in.Rupees()*ex.BreakevenPct)) {
			tighter(pos.in)
		}
		return lv
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
				return res, err
			}
		}
		closes := sr.closes[cfg.Frame][key]
		frames := sr.frames[cfg.Frame][key]
		fk, sk, ak := fast[key], slow[key], atr[key]
		day := filterKey(sr.byDay[d], key)
		for j, m := range day {
			if pos != nil && pos.side != want {
				if err := closePos(m.Time, m.Open, "cross"); err != nil {
					return res, err
				}
			}
			if pos != nil && exitNext != "" {
				why := exitNext
				if err := closePos(m.Time, m.Open, why); err != nil {
					return res, err
				}
				blocked = true
			}
			if pos == nil && want != 0 && !blocked {
				open(m, key)
			}
			if pos != nil {
				s := market.Paise(pos.side)
				if lv := stopLevel(); lv != 0 {
					hit, px := false, lv
					if s*(m.Open-lv) <= 0 {
						hit, px = true, m.Open
					} else if (s > 0 && m.Low <= lv) || (s < 0 && m.High >= lv) {
						hit = true
					}
					if hit {
						if err := closePos(m.Time, px, "stop"); err != nil {
							return res, err
						}
						blocked = true
					}
				}
			}
			if pos != nil && pos.tgt != 0 {
				s := market.Paise(pos.side)
				hit, px := false, pos.tgt
				if s*(m.Open-pos.tgt) >= 0 {
					hit, px = true, m.Open
				} else if (s > 0 && m.High >= pos.tgt) || (s < 0 && m.Low <= pos.tgt) {
					hit = true
				}
				if hit {
					if err := closePos(m.Time, px, "target"); err != nil {
						return res, err
					}
					blocked = true
				}
			}
			if pos != nil {
				if pos.side == market.Buy {
					best = max(best, m.High)
				} else {
					best = min(best, m.Low)
				}
				last = m
				if ex.FlatEOD && j == len(day)-1 {
					if err := closePos(m.End, m.Close, "eod"); err != nil {
						return res, err
					}
				}
			}
			eq := booked
			if pos != nil {
				eq += (m.Close - pos.in).Rupees() * float64(pos.side) * mult
			}
			peak = math.Max(peak, eq)
			dd = math.Max(dd, peak-eq)

			i, ok := closes[m.End]
			if !ok || math.IsNaN(sk[i]) {
				continue
			}
			atrNow = ak[i]
			prev := want
			switch {
			case fk[i] > sk[i]:
				want = market.Buy
			case fk[i] < sk[i] && cfg.LongOnly:
				want = 0
			case fk[i] < sk[i]:
				want = market.Sell
			}
			if want != prev {
				blocked = false
			}
			c := frames[i].Close.Rupees()
			if pos != nil && pos.side == want {
				s := float64(pos.side)
				bars++
				switch {
				case ex.CloseSlow && s*(c-sk[i]) < 0:
					exitNext = "slow"
				case ex.CloseFast && s*(c-fk[i]) < 0:
					exitNext = "fast"
				case ex.TimeBars > 0 && bars >= ex.TimeBars && s*(c-pos.in.Rupees()) <= 0:
					exitNext = "time"
				case ex.Donchian > 0 && i >= ex.Donchian:
					ext := frames[i-ex.Donchian].Low
					if s < 0 {
						ext = frames[i-ex.Donchian].High
					}
					for _, b := range frames[i-ex.Donchian : i] {
						if s > 0 {
							ext = min(ext, b.Low)
						} else {
							ext = max(ext, b.High)
						}
					}
					if s*(c-ext.Rupees()) < 0 {
						exitNext = "donchian"
					}
				}
			}
			if pos == nil && blocked && ex.ReEnter && want != 0 && float64(want)*(c-fk[i]) > 0 {
				blocked = false
			}
		}
	}
	if pos != nil {
		if err := closePos(last.End, last.Close, "end"); err != nil {
			return res, err
		}
	}
	res.MTMDD = dd
	return res, nil
}
