package strategies

import (
	"fmt"
	"math"
	"sort"
	"time"

	"calm/internal/indicator"
	"calm/internal/market"
	"calm/internal/strategy"
	"calm/internal/swing"
)

// lowerHighBreak buys the at-the-money call or put when its own chart, on
// timeframe bars, makes a second break: a close above a significant lower
// high, then a close above the next swing high, with the close above the
// option's session VWAP and its EMA. It sells when the price trades below a
// stop, after a fixed holding time, or at the time exit. The stop starts at
// the low of the pullback before the break and rises to each higher low on
// closes. Call and put trade independently.
//
// The strike is the day's index open rounded to step, in the nearest expiry,
// and is fixed for the day. It is chosen after the first index bar, so the
// strategy subscribes then and builds its own timeframe bars from 1-minute
// bars, starting with the 9:15 bar it reads back. For the EMA to be settled at
// the open, it records the afternoon's bars of strikes around spot and warms
// the next day's chosen contract with them.
//
// Stops are checked on 1-minute lows, as a resting stop order would be, and
// every order is a market order filled in the next minute.
//
// Two optional filters keep it to trending markets. The daily regime (from
// calm market, as of the previous close) can require Bull or Bear, or Bull for
// calls and Bear for puts. The index's ADX on timeframe bars can require a
// trend at the signal. The engine delivers option bars before the index bar
// of the same minute, so with the ADX filter an entry waits for that index
// bar, then is placed in the same minute as without it.
type lowerHighBreak struct {
	underlying string
	tf         market.Timeframe
	k          int
	pullback   float64
	emaN       int
	vwap       bool
	exitK      int
	hold       time.Duration
	flat       int // minutes after the open
	capital    float64
	allocPct   float64
	maxLots    int64
	step       float64
	warmFrom   int // minutes after the open
	warmBand   int // strikes each side of spot to record
	regimeMode int // 0 off, 1 Bull or Bear, 2 Bull for calls and Bear for puts
	adxMin     float64
	adxN       int

	states  map[market.Date]string
	regime  string       // state at the previous close
	idxBars []market.Bar // index timeframe bars, latest last

	day      market.Date
	legs     map[string]*lhbLeg
	chosen   bool
	warming  bool
	warmKeys map[string]bool
	warm     map[string][]market.Bar // this afternoon's 1-minute bars, by key
	prevWarm map[string][]market.Bar // yesterday's
}

// lhbLeg is the day's call or put.
type lhbLeg struct {
	inst market.Instrument
	tag  string
	r    *market.Resampler
	warm []market.Bar // previous session's timeframe bars, for the EMA
	bars []market.Bar // today's timeframe bars

	open     bool
	entryAt  time.Time
	entryIdx int          // timeframe bar that signalled the entry
	stop     market.Paise // current stop; 0 for none
	nextStop market.Paise // initial stop for the pending entry
	nextIdx  int
	// candidate is an entry waiting for the index bar of its minute.
	candidate *lhbEntry
}

type lhbEntry struct {
	end  time.Time
	lots int64
	stop market.Paise
	idx  int
}

var lowerHighParams = []string{"timeframe", "swing_bars", "min_pullback_pct", "ema", "vwap", "trail_bars",
	"hold_minutes", "exit_minute", "size_capital", "alloc_pct", "max_lots", "strike_step", "warm_from_minute", "warm_strikes", "regime", "adx_min", "adx_bars"}

func newLowerHighBreak(u string, p strategy.Params) (strategy.Strategy, error) {
	if err := p.Check(lowerHighParams...); err != nil {
		return nil, err
	}
	s := &lowerHighBreak{underlying: u,
		tf:         market.Timeframe(p.Get("timeframe", 5)),
		k:          int(p.Get("swing_bars", 2)),
		pullback:   p.Get("min_pullback_pct", 10),
		emaN:       int(p.Get("ema", 21)),
		vwap:       p.Get("vwap", 1) != 0,
		exitK:      int(p.Get("trail_bars", 1)),
		hold:       time.Duration(p.Get("hold_minutes", 15)) * time.Minute,
		flat:       int(p.Get("exit_minute", 360)),
		capital:    p.Get("size_capital", 100000),
		allocPct:   p.Get("alloc_pct", 25),
		maxLots:    int64(p.Get("max_lots", 5)),
		step:       p.Get("strike_step", 100),
		warmFrom:   int(p.Get("warm_from_minute", 165)),
		warmBand:   int(p.Get("warm_strikes", 4)),
		regimeMode: int(p.Get("regime", 0)),
		adxMin:     p.Get("adx_min", 0),
		adxN:       int(p.Get("adx_bars", 14)),
	}
	if s.tf < 1 || s.k < 1 || s.exitK < 0 || s.emaN < 0 || s.maxLots < 1 || s.allocPct <= 0 || s.step <= 0 || s.flat < 1 ||
		s.regimeMode < 0 || s.regimeMode > 2 || s.adxMin < 0 || s.adxN < 2 {
		return nil, fmt.Errorf("lower_high_break: bad parameters %+v", p)
	}
	return s, nil
}

// SetRegime supplies the daily regime, which the regime filter needs.
func (s *lowerHighBreak) SetRegime(states map[market.Date]string) { s.states = states }

func (s *lowerHighBreak) Init(ctx strategy.Context) error {
	if s.regimeMode != 0 && s.states == nil {
		return fmt.Errorf("lower_high_break: the regime filter needs the daily regime; run with settlement data")
	}
	if err := ctx.Subscribe(indexKey(s.underlying), market.Minute1); err != nil {
		return err
	}
	if s.adxMin > 0 {
		return ctx.Subscribe(indexKey(s.underlying), s.tf)
	}
	return nil
}

// regimeOn returns the state at the latest close before d.
func (s *lowerHighBreak) regimeOn(d market.Date) string {
	best, state := market.Date{}, ""
	for day, st := range s.states {
		if day.Before(d) && best.Before(day) {
			best, state = day, st
		}
	}
	return state
}

func (s *lowerHighBreak) OnBar(ctx strategy.Context, b market.Bar) error {
	if d := market.DateOf(b.Time); d != s.day {
		s.day, s.legs, s.chosen, s.warming = d, map[string]*lhbLeg{}, false, false
		s.prevWarm, s.warm, s.warmKeys = s.warm, map[string][]market.Bar{}, map[string]bool{}
		if s.regimeMode != 0 {
			s.regime = s.regimeOn(d)
		}
	}
	if b.Key == indexKey(s.underlying) && b.Timeframe == s.tf && s.adxMin > 0 {
		if err := s.onIndexTimeframe(ctx, b); err != nil {
			return err
		}
	}
	if b.Timeframe != market.Minute1 {
		return nil
	}
	minute := minutesIntoSession(b)
	if s.warmKeys[b.Key] && minute > s.warmFrom {
		s.warm[b.Key] = append(s.warm[b.Key], b)
	}
	if b.Key == indexKey(s.underlying) {
		if !s.chosen {
			s.chosen = true
			if err := s.choose(ctx, b); err != nil {
				return err
			}
		}
		if !s.warming && minute >= s.warmFrom {
			s.warming = true
			return s.subscribeWarm(ctx, b.Close)
		}
		return nil
	}
	leg := s.legs[b.Key]
	if leg == nil {
		return nil
	}
	if err := s.checkExit(ctx, leg, b, minute); err != nil {
		return err
	}
	done, err := leg.r.Add(b)
	if err != nil {
		return err
	}
	for _, tb := range done {
		leg.bars = append(leg.bars, tb)
		if err := s.onTimeframe(ctx, leg, minutesIntoSession(tb)); err != nil {
			return err
		}
	}
	return nil
}

// choose picks the day's strike and expiry, subscribes to its call and put,
// and seeds their bars with the minute already seen.
func (s *lowerHighBreak) choose(ctx strategy.Context, first market.Bar) error {
	expiries := ctx.Expiries(s.underlying)
	if len(expiries) == 0 {
		return nil
	}
	strike := market.Rupees(math.Round(first.Open.Rupees()/s.step) * s.step)
	for _, q := range ctx.Chain(s.underlying, expiries[0]) {
		if q.Instrument.Strike != strike {
			continue
		}
		key := q.Instrument.Key()
		r, err := market.NewResampler(market.NSESession, s.tf)
		if err != nil {
			return err
		}
		leg := &lhbLeg{inst: q.Instrument, r: r, warm: resampled(s.prevWarm[key], s.tf),
			tag: fmt.Sprintf("lhb-%s-%s", s.day, q.Instrument.Right)}
		if err := ctx.Subscribe(key, market.Minute1); err != nil {
			return err
		}
		if last, ok := ctx.Last(key); ok && market.DateOf(last.Time) == s.day && !last.Time.After(first.Time) {
			if _, err := r.Add(last); err != nil {
				return err
			}
		}
		s.legs[key] = leg
	}
	return nil
}

// subscribeWarm records strikes around spot in the two nearest expiries from
// the afternoon on, so tomorrow's contract has bars to settle its EMA.
func (s *lowerHighBreak) subscribeWarm(ctx strategy.Context, spot market.Paise) error {
	if s.emaN == 0 {
		return nil
	}
	atm := math.Round(spot.Rupees()/s.step) * s.step
	expiries := ctx.Expiries(s.underlying)
	for i, exp := range expiries {
		if i == 2 {
			break
		}
		for _, q := range ctx.Chain(s.underlying, exp) {
			if math.Abs(q.Instrument.Strike.Rupees()-atm) > float64(s.warmBand)*s.step+0.5 {
				continue
			}
			key := q.Instrument.Key()
			if err := ctx.Subscribe(key, market.Minute1); err != nil {
				return err
			}
			s.warmKeys[key] = true
		}
	}
	return nil
}

func resampled(bars []market.Bar, tf market.Timeframe) []market.Bar {
	if len(bars) == 0 {
		return nil
	}
	r, _ := market.NewResampler(market.NSESession, tf)
	var out []market.Bar
	for _, b := range bars {
		done, err := r.Add(b)
		if err != nil {
			return nil
		}
		out = append(out, done...)
	}
	if b, ok := r.FlushUntil(bars[len(bars)-1].End.Add(time.Hour)); ok {
		out = append(out, b)
	}
	return out
}

// checkExit sells an open leg whose 1-minute bar traded below the stop, whose
// holding time has run out, or that reached the time exit.
func (s *lowerHighBreak) checkExit(ctx strategy.Context, leg *lhbLeg, b market.Bar, minute int) error {
	if !leg.open || ctx.Pending() > 0 {
		return nil
	}
	stopped := leg.stop > 0 && b.Low < leg.stop
	held := s.hold > 0 && b.End.Sub(leg.entryAt) >= s.hold
	if !stopped && !held && minute < s.flat {
		return nil
	}
	for _, p := range ctx.Positions() {
		if p.Instrument.Key() == leg.inst.Key() && p.Qty > 0 {
			return ignoreRejection(ctx.Submit(leg.tag, market.Leg{Instrument: p.Instrument, Side: market.Sell, Lots: p.Qty / p.Instrument.LotSize}))
		}
	}
	return nil
}

// onTimeframe raises an open leg's stop to new higher lows, or enters a flat
// leg whose newest bar is a second break.
func (s *lowerHighBreak) onTimeframe(ctx strategy.Context, leg *lhbLeg, minute int) error {
	last := len(leg.bars) - 1
	if leg.open {
		if s.exitK > 0 {
			for _, l := range swing.LowsOnClose(leg.bars, s.exitK) {
				if l.Index > leg.entryIdx && l.Close > leg.stop {
					leg.stop = l.Close
				}
			}
		}
		return nil
	}
	if ctx.Pending() > 0 || minute >= s.flat {
		return nil
	}
	breaks := swing.Breaks(leg.bars, s.k, s.pullback, s.entryFilter(leg))
	if len(breaks) == 0 {
		return nil
	}
	e := breaks[len(breaks)-1]
	if e.Kind != swing.Second || e.BarIndex != last {
		return nil
	}
	if !s.regimeAllows(leg.inst.Right) {
		return nil
	}
	cost := e.Bar.Close.Rupees() * float64(leg.inst.LotSize)
	lots := min(s.maxLots, max(1, int64(s.capital*s.allocPct/100/cost)))
	entry := &lhbEntry{end: e.Bar.End, lots: lots, stop: e.Pullback, idx: e.BarIndex}
	if s.adxMin > 0 {
		leg.candidate = entry
		return nil
	}
	return s.enter(ctx, leg, entry)
}

func (s *lowerHighBreak) enter(ctx strategy.Context, leg *lhbLeg, e *lhbEntry) error {
	leg.nextStop, leg.nextIdx = e.stop, e.idx
	return ignoreRejection(ctx.Submit(leg.tag, market.Leg{Instrument: leg.inst, Side: market.Buy, Lots: e.lots}))
}

// regimeAllows applies the daily regime filter to a call or put.
func (s *lowerHighBreak) regimeAllows(r market.Right) bool {
	switch s.regimeMode {
	case 1:
		return s.regime == "Bull" || s.regime == "Bear"
	case 2:
		return (s.regime == "Bull" && r == market.Call) || (s.regime == "Bear" && r == market.Put)
	}
	return true
}

// onIndexTimeframe records an index bar and decides the entries that were
// waiting for it: each enters if the index ADX shows a trend.
func (s *lowerHighBreak) onIndexTimeframe(ctx strategy.Context, b market.Bar) error {
	s.idxBars = append(s.idxBars, b)
	if keep := 20 * s.adxN; len(s.idxBars) > 2*keep {
		s.idxBars = append([]market.Bar(nil), s.idxBars[len(s.idxBars)-keep:]...)
	}
	var adx float64
	computed := false
	for _, key := range sortedKeys(s.legs) {
		leg := s.legs[key]
		c := leg.candidate
		if c == nil || c.end.After(b.End) {
			continue
		}
		leg.candidate = nil
		if !c.end.Equal(b.End) || leg.open || ctx.Pending() > 0 {
			continue // the index bar for its minute never came, or something else trades
		}
		if !computed {
			a := indicator.ADX(s.idxBars, s.adxN)
			adx, computed = a[len(a)-1], true
		}
		if math.IsNaN(adx) || adx < s.adxMin {
			continue
		}
		if err := s.enter(ctx, leg, c); err != nil {
			return err
		}
	}
	return nil
}

func sortedKeys(m map[string]*lhbLeg) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// entryFilter requires the close above the session VWAP and the EMA, which is
// warmed up on yesterday's bars when there are any.
func (s *lowerHighBreak) entryFilter(leg *lhbLeg) func(int) bool {
	var ema, vwap []float64
	if s.emaN > 0 {
		series := append(append([]market.Bar(nil), leg.warm...), leg.bars...)
		ema = indicator.EMA(series, s.emaN)[len(leg.warm):]
	}
	if s.vwap {
		vwap = indicator.VWAP(leg.bars)
	}
	return func(i int) bool {
		c := leg.bars[i].Close.Rupees()
		return (vwap == nil || c > vwap[i]) && (ema == nil || (!math.IsNaN(ema[i]) && c > ema[i]))
	}
}

func (s *lowerHighBreak) OnFill(_ strategy.Context, f market.Fill) error {
	leg := s.legs[f.Instrument.Key()]
	if leg == nil {
		return nil
	}
	if f.Kind == market.Traded && f.Side == market.Buy {
		leg.open, leg.entryAt = true, f.Time
		leg.stop, leg.entryIdx = leg.nextStop, leg.nextIdx
		return nil
	}
	leg.open, leg.stop = false, 0
	return nil
}
