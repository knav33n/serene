package mcx

import (
	"fmt"
	"sort"
	"time"

	"serene/internal/cost"
	"serene/internal/dow"
	"serene/internal/market"
)

// Spec is what a backtest needs to know about a commodity's futures: the
// rupee value of a one-rupee move per lot, and the tick.
type Spec struct {
	Multiplier int64        // quoted units per lot
	Tick       market.Paise // minimum price step
}

// Specs covers the pulse commodities. Upstox's MCX lot sizes are in trading
// units (GOLD 1 kg, ZINC 5 t), not the quoted unit, so P&L uses these.
var Specs = map[string]Spec{
	"CRUDEOIL":   {100, 100}, // 100 bbl, ₹/bbl, ₹1
	"NATURALGAS": {1250, 10}, // 1250 mmBtu, ₹/mmBtu, ₹0.10
	"GOLD":       {100, 100}, // 1 kg, ₹/10 g, ₹1
	"GOLDM":      {10, 100},  // 100 g, ₹/10 g, ₹1
	"SILVER":     {30, 100},  // 30 kg, ₹/kg, ₹1
	"COPPER":     {2500, 5},  // 2500 kg, ₹/kg, ₹0.05
	"ZINC":       {5000, 5},  // 5 t, ₹/kg, ₹0.05
	"ALUMINIUM":  {5000, 5},  // 5 t, ₹/kg, ₹0.05
}

// BacktestConfig sets up a structure backtest.
type BacktestConfig struct {
	Name     string
	Spec     Spec
	Frame    time.Duration // swing bars, 1H
	Lookback time.Duration // history the zigzag sees at each decision
	Multiple float64       // reversal = Multiple × median frame range
	Slip     int64         // ticks against each fill
	// Market enters at the next minute's open after the signal instead of
	// resting a limit at the broken level.
	Market bool
	// TargetR exits at this multiple of the initial risk; 0 trails only.
	TargetR  float64
	Costs    *cost.Schedule
	From, To market.Date // sessions traded
}

// Trade is one round trip on one lot.
type Trade struct {
	Name, Key   string
	Side        market.Side
	Kind        string // BOS: break with the trend; CHoCH: against it; NEW: no trend yet
	Signal      time.Time
	Level, Stop market.Paise // limit price and the stop when the order was placed
	Entry, Exit time.Time
	In, Out     market.Paise // fill prices after slippage
	Reason      string       // stop, roll, end
	R           float64      // move over initial risk
	Gross, Net  market.Paise // rupees for one lot
}

// Orders counts what became of the limit orders.
type Orders struct {
	Placed, Filled, Ran, Replaced, Rolled int
}

type order struct {
	side        market.Side
	kind        string
	signal      time.Time
	level, stop market.Paise
}

type position struct {
	order
	key   string
	entry time.Time
	in    market.Paise // raw fill, before slippage
	stop  market.Paise
	risk  market.Paise
	tgt   market.Paise // 0 none
}

// ActiveContracts picks, for each session date in days, the contract with
// the most volume in the previous session. The first session has none.
func ActiveContracts(days []market.Date, byDay map[market.Date][]market.Bar) map[market.Date]string {
	out := map[market.Date]string{}
	for i := 1; i < len(days); i++ {
		vol := map[string]int64{}
		for _, b := range byDay[days[i-1]] {
			vol[b.Key] += b.Volume
		}
		best, bv := "", int64(-1)
		for k, v := range vol {
			if v > bv || (v == bv && k < best) {
				best, bv = k, v
			}
		}
		if best != "" {
			out[days[i]] = best
		}
	}
	return out
}

// Backtest trades Dow structure on one commodity. bars are its 1-minute bars
// for every contract, in any order. At each frame close it reads the swings
// the active contract's own bars make over the lookback. The first frame to
// close above the last swing high rests a limit buy at that level, with the stop under the last
// swing low; a close below the last swing low is the mirror. The order is
// cancelled if a new swing on the stop side confirms before it fills (the
// pullback ended short of the level), by a newer signal, or at a roll. An
// open position trails its stop to each new confirmed swing and exits when it
// trades, or at a roll to another contract.
func Backtest(bars []market.Bar, cfg BacktestConfig) ([]Trade, Orders, error) {
	var stats Orders
	sr := prepare(bars, cfg.Frame)
	days, active := sr.days, sr.active
	byDay := sr.byDay
	frames, closes := sr.frames[cfg.Frame], sr.closes[cfg.Frame]

	var (
		trades []Trade
		pend   *order
		pendK  string
		pos    *position
		last   market.Bar // last minute seen of the position's contract
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

	for _, d := range days {
		if d.Before(cfg.From) || cfg.To.Before(d) {
			continue
		}
		key, ok := active[d]
		if !ok {
			continue
		}
		if pend != nil && pendK != key {
			pend = nil
			stats.Rolled++
		}
		today := filterKey(byDay[d], key)
		if pos != nil && pos.key != key {
			// Roll: out at the old contract's first price today, else its last.
			old := filterKey(byDay[d], pos.key)
			at, px := last.End, last.Close
			if len(old) > 0 {
				at, px = old[0].Time, old[0].Open
			}
			if err := closePos(at, px, "roll"); err != nil {
				return nil, stats, err
			}
		}
		for _, m := range today {
			// The resting order or open stop meets this minute.
			if pend != nil {
				o := pend
				if cfg.Market {
					pos = &position{order: *o, key: key, entry: m.Time, in: m.Open, stop: o.stop}
				} else if o.side == market.Buy {
					if m.Low < o.level {
						pos = &position{order: *o, key: key, entry: m.Time, in: min(m.Open, o.level), stop: o.stop}
					}
				} else if m.High > o.level {
					pos = &position{order: *o, key: key, entry: m.Time, in: max(m.Open, o.level), stop: o.stop}
				}
				// The stop sits beyond the level, so price can't reach it
				// without filling the order: a gap through both fills at the
				// open and the stop below takes it straight out.
				if pos != nil {
					pend = nil
					stats.Filled++
					// A gap through the stop leaves no risk to measure by.
					pos.risk = max((pos.in-pos.stop)*market.Paise(pos.side), cfg.Spec.Tick)
					if cfg.TargetR > 0 {
						pos.tgt = pos.in + market.Paise(pos.side)*market.Paise(cfg.TargetR*float64(pos.risk))
					}
				}
			}
			if pos != nil {
				last = m
				if pos.side == market.Buy && m.Low <= pos.stop {
					if err := closePos(m.Time, min(m.Open, pos.stop), "stop"); err != nil {
						return nil, stats, err
					}
				} else if pos.side == market.Sell && m.High >= pos.stop {
					if err := closePos(m.Time, max(m.Open, pos.stop), "stop"); err != nil {
						return nil, stats, err
					}
				}
				// The stop is checked first, so a minute through both loses.
				if pos != nil && pos.tgt != 0 {
					if pos.side == market.Buy && m.High >= pos.tgt {
						if err := closePos(m.Time, max(m.Open, pos.tgt), "target"); err != nil {
							return nil, stats, err
						}
					} else if pos.side == market.Sell && m.Low <= pos.tgt {
						if err := closePos(m.Time, min(m.Open, pos.tgt), "target"); err != nil {
							return nil, stats, err
						}
					}
				}
			}
			// A frame closed with this minute: read the structure.
			i, ok := closes[key][m.End]
			if !ok {
				continue
			}
			f := frames[key]
			start := m.End.Add(-cfg.Lookback)
			j := sort.Search(i+1, func(j int) bool { return !f[j].Time.Before(start) })
			q := f[j : i+1]
			if len(q) < 3 {
				continue
			}
			thr := market.Paise(cfg.Multiple * float64(medianRange(q)))
			swings, _ := dow.Swings(q, thr)
			known := dow.At(swings, m.End)
			hi, lo := dow.Last(known)
			if pos != nil {
				if pos.side == market.Buy && lo != nil && lo.Price-cfg.Spec.Tick > pos.stop {
					pos.stop = lo.Price - cfg.Spec.Tick
				}
				if pos.side == market.Sell && hi != nil && hi.Price+cfg.Spec.Tick < pos.stop {
					pos.stop = hi.Price + cfg.Spec.Tick
				}
			}
			if pend != nil {
				// A swing on the stop side confirmed after the order: the
				// pullback turned before reaching the level.
				s := lo
				if pend.side == market.Sell {
					s = hi
				}
				if s != nil && s.Confirmed.After(pend.signal) {
					pend = nil
					stats.Ran++
				}
			}
			if pos != nil {
				continue
			}
			// A signal is the first frame to close beyond the last swing
			// confirmed before it began; a wick through the level is not one.
			bar := q[len(q)-1]
			before := dow.At(swings, bar.Time)
			bh, bl := dow.Last(before)
			if bh == nil || bl == nil {
				continue
			}
			tr := dow.TrendOf(before)
			up := bar.Close > bh.Price && firstClose(q, bh, true)
			down := bar.Close < bl.Price && firstClose(q, bl, false)
			var o order
			switch {
			case up && !down:
				o = order{side: market.Buy, level: bh.Price, stop: bl.Price - cfg.Spec.Tick, kind: kindOf(tr, tr == dow.Down)}
			case down && !up:
				o = order{side: market.Sell, level: bl.Price, stop: bh.Price + cfg.Spec.Tick, kind: kindOf(tr, tr == dow.Up)}
			default:
				continue
			}
			if (o.level-o.stop)*market.Paise(o.side) <= 0 {
				continue
			}
			o.signal = m.End
			if pend != nil {
				stats.Replaced++
			}
			pend, pendK = &o, key
			stats.Placed++
		}
	}
	if pos != nil {
		if err := closePos(last.End, last.Close, "end"); err != nil {
			return nil, stats, err
		}
	}
	return trades, stats, nil
}

// firstClose reports whether no frame before the last in q, since s
// confirmed, closed beyond it.
func firstClose(q []market.Bar, s *dow.Swing, above bool) bool {
	for _, b := range q[:len(q)-1] {
		if b.End.After(s.Confirmed) && ((above && b.Close > s.Price) || (!above && b.Close < s.Price)) {
			return false
		}
	}
	return true
}

// series is one commodity's minute bars split by day and contract, with the
// contract to trade each day and each contract's bars in the given frames.
type series struct {
	byDay  map[market.Date][]market.Bar // every contract, cleaned
	days   []market.Date
	active map[market.Date]string
	frames map[time.Duration]map[string][]market.Bar
	closes map[time.Duration]map[string]map[time.Time]int // frame End → index
}

func prepare(bars []market.Bar, frames ...time.Duration) *series {
	sr := &series{byDay: map[market.Date][]market.Bar{}, frames: map[time.Duration]map[string][]market.Bar{},
		closes: map[time.Duration]map[string]map[time.Time]int{}}
	byKey := map[string][]market.Bar{}
	for _, b := range bars {
		byKey[b.Key] = append(byKey[b.Key], b)
	}
	for _, fr := range frames {
		sr.frames[fr] = map[string][]market.Bar{}
		sr.closes[fr] = map[string]map[time.Time]int{}
	}
	for k, bs := range byKey {
		sort.Slice(bs, func(i, j int) bool { return bs[i].Time.Before(bs[j].Time) })
		bs = CleanStaleOpens(bs)
		for _, fr := range frames {
			f := Bucket(bs, fr)
			sr.frames[fr][k] = f
			c := map[time.Time]int{}
			for i, b := range f {
				c[b.End] = i
			}
			sr.closes[fr][k] = c
		}
		for _, b := range bs {
			d := market.DateOf(b.Time)
			sr.byDay[d] = append(sr.byDay[d], b)
		}
	}
	for d, bs := range sr.byDay {
		sort.SliceStable(bs, func(i, j int) bool { return bs[i].Time.Before(bs[j].Time) })
		sr.days = append(sr.days, d)
	}
	sort.Slice(sr.days, func(i, j int) bool { return sr.days[i].Before(sr.days[j]) })
	sr.active = ActiveContracts(sr.days, sr.byDay)
	return sr
}

// settle closes p at raw, before slippage, into a trade on one lot.
func settle(name string, spec Spec, slipTicks int64, costs *cost.Schedule, p *position, at time.Time, raw market.Paise, why string) (Trade, error) {
	slip := market.Paise(slipTicks) * spec.Tick
	side := market.Paise(p.side)
	out := raw - side*slip
	in := p.in + side*slip
	mv := (out - in) * side
	t := Trade{Name: name, Key: p.key, Side: p.side, Kind: p.kind, Signal: p.signal,
		Level: p.level, Stop: p.order.stop, Entry: p.entry, Exit: at, In: in, Out: out, Reason: why,
		R: float64(mv) / float64(p.risk), Gross: mv * market.Paise(spec.Multiplier)}
	inst, err := market.ParseKey(p.key)
	if err != nil {
		return t, err
	}
	c1, err := costs.Charges(market.Fill{Instrument: inst, Side: p.side, Qty: spec.Multiplier, Price: in, Time: p.entry, Kind: market.Traded})
	if err != nil {
		return t, err
	}
	c2, err := costs.Charges(market.Fill{Instrument: inst, Side: -p.side, Qty: spec.Multiplier, Price: out, Time: at, Kind: market.Traded})
	if err != nil {
		return t, err
	}
	t.Net = t.Gross - c1 - c2
	return t, nil
}

func kindOf(tr dow.Trend, choch bool) string {
	switch {
	case choch:
		return "CHoCH"
	case tr == dow.Up || tr == dow.Down:
		return "BOS"
	}
	return "NEW"
}

func filterKey(bars []market.Bar, key string) []market.Bar {
	var out []market.Bar
	for _, b := range bars {
		if b.Key == key {
			out = append(out, b)
		}
	}
	return out
}

// String names a trade for logs.
func (t Trade) String() string {
	return fmt.Sprintf("%s %s %s %s→%s %s→%s %s", t.Name, t.Side, t.Kind, t.Entry.Format("01-02 15:04"),
		t.Exit.Format("01-02 15:04"), Price(t.In), Price(t.Out), t.Reason)
}
