package mcx

import (
	"fmt"
	"math"
	"sort"
	"time"

	"serene/internal/cost"
	"serene/internal/dow"
	"serene/internal/indicator"
	"serene/internal/market"
)

// Trade Cloud is the user's TradingView indicator: seven readings, each
// bullish (+1), bearish (−1) or neutral (0).
const (
	LongTrend  = iota // EMA30 vs EMA74
	ShortTrend        // EMA7 vs EMA21
	STMomentum        // RSI12 vs its 4-bar SMA
	MTMomentum        // RSI12 vs its 12-bar SMA
	LTMomentum        // 4-bar SMA vs 12-bar SMA of RSI12
	HeikinAshi        // HA candle with no wick against it
	Clouds            // close beyond all four EMAs
	NumReadings
)

// ReadingNames label the readings as the indicator's table does.
var ReadingNames = [NumReadings]string{"Long Trend", "Short Trend", "ST Momentum", "MT Momentum",
	"LT Momentum", "Price Action 1 - Heikin Ashi", "Price Action 2 - Clouds"}

// Reading is one bar's seven readings.
type Reading [NumReadings]int8

// Count returns how many readings are bullish and bearish.
func (r Reading) Count() (bull, bear int) {
	for _, v := range r {
		if v > 0 {
			bull++
		} else if v < 0 {
			bear++
		}
	}
	return bull, bear
}

// Bias is +1 when at least n readings are bullish, −1 when at least n are
// bearish, else 0.
func (r Reading) Bias(n int) int {
	bull, bear := r.Count()
	switch {
	case bull >= n && bull > bear:
		return 1
	case bear >= n && bear > bull:
		return -1
	}
	return 0
}

// CloudSeries is the indicator over a run of bars, value i known at the
// close of bar i.
type CloudSeries struct {
	Readings       []Reading
	EMA21          []float64
	HAOpen, HAClos []float64
	ATR14          []float64
}

func vs(a, b float64) int8 {
	switch {
	case a > b:
		return 1
	case a < b:
		return -1
	}
	return 0 // equal, or either is NaN
}

func smaOf(xs []float64, n int) []float64 {
	out := make([]float64, len(xs))
	for i := range xs {
		out[i] = math.NaN()
		if i < n-1 {
			continue
		}
		s := 0.0
		for _, x := range xs[i-n+1 : i+1] {
			s += x
		}
		out[i] = s / float64(n) // NaN if any input is
	}
	return out
}

// Cloud computes the indicator with its default inputs.
func Cloud(bars []market.Bar) CloudSeries {
	sf, ss := indicator.EMA(bars, 7), indicator.EMA(bars, 21)
	lf, ls := indicator.EMA(bars, 30), indicator.EMA(bars, 74)
	rsi := indicator.RSI(bars, 12)
	r4, r12 := smaOf(rsi, 4), smaOf(rsi, 12)
	cs := CloudSeries{Readings: make([]Reading, len(bars)), EMA21: ss, HAOpen: make([]float64, len(bars)),
		HAClos: make([]float64, len(bars)), ATR14: indicator.ATR(bars, 14)}
	for i, b := range bars {
		o, h, l, c := b.Open.Rupees(), b.High.Rupees(), b.Low.Rupees(), b.Close.Rupees()
		haC := (o + h + l + c) / 4
		haO := (o + c) / 2
		if i > 0 {
			haO = (cs.HAOpen[i-1] + cs.HAClos[i-1]) / 2
		}
		cs.HAOpen[i], cs.HAClos[i] = haO, haC
		var r Reading
		r[LongTrend] = vs(lf[i], ls[i])
		r[ShortTrend] = vs(sf[i], ss[i])
		r[STMomentum] = vs(rsi[i], r4[i])
		r[MTMomentum] = vs(rsi[i], r12[i])
		r[LTMomentum] = vs(r4[i], r12[i])
		// No lower wick: the HA low, min(low, haOpen, haClose), is the HA open.
		switch {
		case haC > haO && haO <= l:
			r[HeikinAshi] = 1
		case haC < haO && haO >= h:
			r[HeikinAshi] = -1
		}
		switch {
		case c > sf[i] && c > ss[i] && c > lf[i] && c > ls[i]:
			r[Clouds] = 1
		case c < sf[i] && c < ss[i] && c < lf[i] && c < ls[i]:
			r[Clouds] = -1
		}
		cs.Readings[i] = r
	}
	return cs
}

// Exit rules for the cloud strategy, judged on 30m bars.
const (
	ExitSwing30 = "swing30" // trail beyond each confirmed 30m swing
	ExitVote30  = "vote30"  // 4 or more 30m readings against
	ExitCloud30 = "cloud30" // 30m close beyond EMA21 against
	ExitHA30    = "ha30"    // opposite-colour 30m Heikin Ashi candle
	Exit2R      = "2R"      // fixed target at twice the initial risk
)

// ExitRules lists them in report order.
var ExitRules = []string{ExitSwing30, ExitVote30, ExitCloud30, ExitHA30, Exit2R}

// CloudConfig sets up a cloud strategy run.
type CloudConfig struct {
	Name     string
	Spec     Spec
	N        int     // readings that must agree for a bias
	Exit     string  // one of ExitRules
	NineK    float64 // skip 10:00–10:59 entries after a 9:00 hour beyond this × ATR14 their way; 0 off
	Lookback time.Duration
	Multiple float64 // swing reversal, × median range
	Slip     int64
	Costs    *cost.Schedule
	From, To market.Date
}

// CloudData is one commodity prepared once for many runs.
type CloudData struct {
	sr  *series
	ind map[time.Duration]map[string]CloudSeries
}

var (
	entryFrame = time.Hour
	exitFrame  = 30 * time.Minute
)

// PrepareCloud splits bars by contract and computes the indicator on each
// contract's own 1H and 30m bars.
func PrepareCloud(bars []market.Bar) *CloudData {
	cd := &CloudData{sr: prepare(bars, entryFrame, exitFrame), ind: map[time.Duration]map[string]CloudSeries{}}
	for _, fr := range []time.Duration{entryFrame, exitFrame} {
		cd.ind[fr] = map[string]CloudSeries{}
		for k, f := range cd.sr.frames[fr] {
			cd.ind[fr][k] = Cloud(f)
		}
	}
	return cd
}

// LastReading returns the latest 1H bar of the most recently active
// contract and its readings.
func (cd *CloudData) LastReading() (string, market.Bar, Reading, bool) {
	if len(cd.sr.days) == 0 {
		return "", market.Bar{}, Reading{}, false
	}
	d := cd.sr.days[len(cd.sr.days)-1]
	key, ok := cd.sr.active[d]
	if !ok {
		return "", market.Bar{}, Reading{}, false
	}
	f := cd.sr.frames[entryFrame][key]
	if len(f) == 0 {
		return "", market.Bar{}, Reading{}, false
	}
	return key, f[len(f)-1], cd.ind[entryFrame][key].Readings[len(f)-1], true
}

// swingsAt reads the zigzag over the lookback ending with frame bar i.
func swingsAt(f []market.Bar, i int, lookback time.Duration, mult float64) (q []market.Bar, swings []dow.Swing) {
	start := f[i].End.Add(-lookback)
	j := sort.Search(i+1, func(j int) bool { return !f[j].Time.Before(start) })
	q = f[j : i+1]
	if len(q) < 3 {
		return q, nil
	}
	sw, _ := dow.Swings(q, market.Paise(mult*float64(medianRange(q))))
	return q, sw
}

// CloudBacktest trades the cloud on one commodity. At each 1H close, with a
// bull bias (at least N readings bullish) it buys at the next minute's open
// when the bar is the first to close above the last swing high (breakout),
// stop under the last swing low; otherwise it rests a limit buy at the last
// swing low (support), stop 0.5×ATR14 under it, cancelled when the bias goes,
// a 1H close lands below it, or a newer swing low replaces it. A bear bias is
// the mirror. Exits follow cfg.Exit on 30m bars, behind the initial stop.
func CloudBacktest(cd *CloudData, cfg CloudConfig) ([]Trade, error) {
	sr := cd.sr
	var (
		trades   []Trade
		pend     *order
		pendMkt  bool
		pendKey  string
		pos      *position
		last     market.Bar
		exitNext bool
	)
	closePos := func(at time.Time, raw market.Paise, why string) error {
		t, err := settle(cfg.Name, cfg.Spec, cfg.Slip, cfg.Costs, pos, at, raw, why)
		if err != nil {
			return err
		}
		trades = append(trades, t)
		pos, exitNext = nil, false
		return nil
	}
	tick := cfg.Spec.Tick
	for _, d := range sr.days {
		if d.Before(cfg.From) || cfg.To.Before(d) {
			continue
		}
		key, ok := sr.active[d]
		if !ok {
			continue
		}
		if pend != nil && pendKey != key {
			pend = nil
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
		H, M := sr.frames[entryFrame][key], sr.frames[exitFrame][key]
		hc, mc := sr.closes[entryFrame][key], sr.closes[exitFrame][key]
		hi, mi := cd.ind[entryFrame][key], cd.ind[exitFrame][key]
		nine := -1 // today's 9:00 1H bar
		if j := sort.Search(len(H), func(j int) bool { return !H[j].Time.Before(d.At(9, 0)) }); j < len(H) && H[j].Time.Equal(d.At(9, 0)) {
			nine = j
		}
		for _, m := range filterKey(sr.byDay[d], key) {
			if pos != nil && exitNext {
				if err := closePos(m.Time, m.Open, cfg.Exit); err != nil {
					return nil, err
				}
			}
			if pend != nil {
				o := *pend
				var in market.Paise
				filled := false
				switch {
				case pendMkt:
					in, filled = m.Open, true
				case o.side == market.Buy && m.Low < o.level:
					in, filled = min(m.Open, o.level), true
				case o.side == market.Sell && m.High > o.level:
					in, filled = max(m.Open, o.level), true
				}
				if filled {
					pend = nil
					if cfg.NineK > 0 && m.Time.Hour() == 10 && nine >= 0 && !math.IsNaN(hi.ATR14[nine]) {
						mv := (H[nine].Close - H[nine].Open).Rupees() * float64(o.side)
						if mv > cfg.NineK*hi.ATR14[nine] {
							filled = false // skipped: the 9:00 hour already ran their way
						}
					}
				}
				if filled {
					pos = &position{order: o, key: key, entry: m.Time, in: in, stop: o.stop}
					pos.risk = max((in-o.stop)*market.Paise(o.side), tick)
					if cfg.Exit == Exit2R {
						pos.tgt = in + market.Paise(o.side)*2*pos.risk
					}
				}
			}
			if pos != nil {
				last = m
				hit := (pos.side == market.Buy && m.Low <= pos.stop) || (pos.side == market.Sell && m.High >= pos.stop)
				if hit {
					px := min(m.Open, pos.stop)
					if pos.side == market.Sell {
						px = max(m.Open, pos.stop)
					}
					if err := closePos(m.Time, px, "stop"); err != nil {
						return nil, err
					}
				} else if pos.tgt != 0 && ((pos.side == market.Buy && m.High >= pos.tgt) || (pos.side == market.Sell && m.Low <= pos.tgt)) {
					px := max(m.Open, pos.tgt)
					if pos.side == market.Sell {
						px = min(m.Open, pos.tgt)
					}
					if err := closePos(m.Time, px, "target"); err != nil {
						return nil, err
					}
				}
			}
			// 30m close: the exit rule.
			if i, ok := mc[m.End]; ok && pos != nil {
				side := int8(pos.side)
				switch cfg.Exit {
				case ExitSwing30:
					_, sw := swingsAt(M, i, cfg.Lookback, cfg.Multiple)
					h, l := dow.Last(dow.At(sw, m.End))
					if pos.side == market.Buy && l != nil && l.Price-tick > pos.stop && l.Confirmed.After(pos.entry) {
						pos.stop = l.Price - tick
					}
					if pos.side == market.Sell && h != nil && h.Price+tick < pos.stop && h.Confirmed.After(pos.entry) {
						pos.stop = h.Price + tick
					}
				case ExitVote30:
					bull, bear := mi.Readings[i].Count()
					if (side > 0 && bear >= 4) || (side < 0 && bull >= 4) {
						exitNext = true
					}
				case ExitCloud30:
					if vs(M[i].Close.Rupees(), mi.EMA21[i]) == -side {
						exitNext = true
					}
				case ExitHA30:
					if vs(mi.HAClos[i], mi.HAOpen[i]) == -side {
						exitNext = true
					}
				}
			}
			// 1H close: bias and entries.
			i, ok := hc[m.End]
			if !ok {
				continue
			}
			bias := hi.Readings[i].Bias(cfg.N)
			q, sw := swingsAt(H, i, cfg.Lookback, cfg.Multiple)
			if len(sw) == 0 {
				pend = nil
				continue
			}
			bar := q[len(q)-1]
			bh, bl := dow.Last(dow.At(sw, bar.Time))
			if pend != nil && !pendMkt {
				// The resting support or resistance order.
				lvl := bl
				if pend.side == market.Sell {
					lvl = bh
				}
				switch {
				case int(pend.side) != bias,
					pend.side == market.Buy && bar.Close < pend.level,
					pend.side == market.Sell && bar.Close > pend.level,
					lvl == nil:
					pend = nil
				case lvl.Price != pend.level:
					pend = nil // a newer swing: placed again below
				}
			}
			if pos != nil || bias == 0 || bh == nil || bl == nil {
				continue
			}
			atr := hi.ATR14[i]
			if bias > 0 {
				switch {
				case bar.Close > bh.Price && firstClose(q, bh, true) && bl.Price-tick < bar.Close:
					pend = &order{side: market.Buy, kind: "breakout", signal: m.End, level: bh.Price, stop: bl.Price - tick}
					pendMkt, pendKey = true, key
				case pend == nil && bar.Close > bl.Price && !math.IsNaN(atr):
					pend = &order{side: market.Buy, kind: "support", signal: m.End, level: bl.Price, stop: offTick(bl.Price, -0.5*atr, tick)}
					pendMkt, pendKey = false, key
				}
			} else {
				switch {
				case bar.Close < bl.Price && firstClose(q, bl, false) && bh.Price+tick > bar.Close:
					pend = &order{side: market.Sell, kind: "breakdown", signal: m.End, level: bl.Price, stop: bh.Price + tick}
					pendMkt, pendKey = true, key
				case pend == nil && bar.Close < bh.Price && !math.IsNaN(atr):
					pend = &order{side: market.Sell, kind: "resistance", signal: m.End, level: bh.Price, stop: offTick(bh.Price, 0.5*atr, tick)}
					pendMkt, pendKey = false, key
				}
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

// offTick moves p by rupees, rounded outward to a whole tick.
func offTick(p market.Paise, rupees float64, tick market.Paise) market.Paise {
	n := math.Ceil(math.Abs(rupees) * 100 / float64(tick))
	return p + market.Paise(math.Copysign(n, rupees))*tick
}

// FormatReading lays a reading out like the indicator's table.
func FormatReading(r Reading) string {
	s := ""
	for i, v := range r {
		t := "Neutral"
		if v > 0 {
			t = "Bullish"
		} else if v < 0 {
			t = "Bearish"
		}
		s += fmt.Sprintf("%-30s %s\n", ReadingNames[i], t)
	}
	return s
}
