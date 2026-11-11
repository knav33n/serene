package strategies

import (
	"serene/internal/indicator"
	"serene/internal/market"
	"serene/internal/strategy"
	"fmt"
	"math"
)

// emaCrossFirst buys, once a day, the first at-the-money option whose own
// 1-minute chart crosses its fast EMA above its slow EMA, and sells it when
// the fast EMA crosses back below or at the time exit. It trades only on days
// a set number of trading days before the nearest expiry (dte 1: the day
// before expiry).
//
// The strike is the day's index open rounded to strike_step, in the nearest
// expiry; both the call and the put at it are watched. The EMAs are warmed on
// the previous afternoon's bars of the same contract, so a gap at the open can
// cross on the first bar. The band of strikes recorded for that must cover
// the biggest gaps: 40 strikes of 50 each side is 2,000 points. A cross is decided at a bar's close and the order
// fills in the next minute. No entries from last_entry_minute.
//
// Trading days to expiry skip weekends and NSE holidays. A run stops with an
// error on an expiry past the end of the holiday list, rather than guess.
//
// The rules were frozen on 2026-11-08 from `serene research emacross` (1DTE,
// option chart, 1-minute, 3/8, first trade a day); see
// docs/strategies/ema_cross_first.md.
type emaCrossFirst struct {
	underlying string
	fast, slow int
	dte        int
	lastEntry  int
	flat       int
	lots       int64
	step       float64
	warmFrom   int
	warmBand   int

	day      market.Date
	legs     map[string]*ecLeg
	chosen   bool
	traded   bool
	warming  bool
	warmKeys map[string]bool
	warm     map[string][]market.Bar
	prevWarm map[string][]market.Bar
}

type ecLeg struct {
	inst  market.Instrument
	tag   string
	bars  []market.Bar // warm bars, then today's
	nWarm int
	open  bool
}

var emaCrossParams = []string{"fast", "slow", "dte", "last_entry_minute", "exit_minute", "lots", "strike_step", "warm_from_minute", "warm_strikes"}

func newEMACrossFirst(u string, p strategy.Params) (strategy.Strategy, error) {
	if err := p.Check(emaCrossParams...); err != nil {
		return nil, err
	}
	s := &emaCrossFirst{underlying: u,
		fast:      int(p.Get("fast", 3)),
		slow:      int(p.Get("slow", 8)),
		dte:       int(p.Get("dte", 1)),
		lastEntry: int(p.Get("last_entry_minute", 345)),
		flat:      int(p.Get("exit_minute", 360)),
		lots:      int64(p.Get("lots", 1)),
		step:      p.Get("strike_step", 50),
		warmFrom:  int(p.Get("warm_from_minute", 165)),
		warmBand:  int(p.Get("warm_strikes", 40)),
	}
	if s.fast < 1 || s.slow <= s.fast || s.dte < 0 || s.lastEntry < 1 || s.flat < s.lastEntry || s.lots < 1 || s.step <= 0 || s.warmBand < 0 {
		return nil, fmt.Errorf("ema_cross_first: bad parameters %+v", p)
	}
	return s, nil
}

func (s *emaCrossFirst) Init(ctx strategy.Context) error {
	return ctx.Subscribe(indexKey(s.underlying), market.Minute1)
}

// tradingDaysTo counts NSE trading days after d up to and including exp: 0
// on expiry day, 1 the trading day before.
func tradingDaysTo(d, exp market.Date) (int, error) {
	if market.NSEHolidaysThrough.Before(exp) {
		return 0, fmt.Errorf("expiry %s is past the NSE holiday list, which ends %s: add the new year's holidays to internal/market/holidays.go", exp, market.NSEHolidaysThrough)
	}
	n := 0
	for x := d.AddDays(1); !exp.Before(x); x = x.AddDays(1) {
		if market.NSETradingDay(x) {
			n++
		}
	}
	return n, nil
}

func (s *emaCrossFirst) OnBar(ctx strategy.Context, b market.Bar) error {
	if d := market.DateOf(b.Time); d != s.day {
		s.day, s.legs, s.chosen, s.traded, s.warming = d, map[string]*ecLeg{}, false, false, false
		s.prevWarm, s.warm, s.warmKeys = s.warm, map[string][]market.Bar{}, map[string]bool{}
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
			if err := s.subscribeWarm(ctx, b.Close); err != nil {
				return err
			}
		}
		return nil
	}
	if leg := s.legs[b.Key]; leg != nil {
		leg.bars = append(leg.bars, b)
		return s.onLegBar(ctx, leg, minute)
	}
	return nil
}

// choose picks today's call and put if today is the chosen distance from the
// nearest expiry.
func (s *emaCrossFirst) choose(ctx strategy.Context, first market.Bar) error {
	expiries := ctx.Expiries(s.underlying)
	if len(expiries) == 0 {
		return nil
	}
	if n, err := tradingDaysTo(s.day, expiries[0]); err != nil || n != s.dte {
		return err
	}
	strike := market.Rupees(math.Round(first.Open.Rupees()/s.step) * s.step)
	for _, q := range ctx.Chain(s.underlying, expiries[0]) {
		if q.Instrument.Strike != strike {
			continue
		}
		key := q.Instrument.Key()
		leg := &ecLeg{inst: q.Instrument, tag: fmt.Sprintf("emax-%s-%s", s.day, q.Instrument.Right)}
		leg.bars = append(leg.bars, s.prevWarm[key]...)
		leg.nWarm = len(leg.bars)
		if err := ctx.Subscribe(key, market.Minute1); err != nil {
			return err
		}
		s.legs[key] = leg
		// The option's bar for this minute may have come before the index's.
		if last, ok := ctx.Last(key); ok && market.DateOf(last.Time) == s.day && !last.Time.After(first.Time) {
			leg.bars = append(leg.bars, last)
			if err := s.onLegBar(ctx, leg, minutesIntoSession(last)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *emaCrossFirst) subscribeWarm(ctx strategy.Context, spot market.Paise) error {
	atm := math.Round(spot.Rupees()/s.step) * s.step
	for i, exp := range ctx.Expiries(s.underlying) {
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

// onLegBar acts on the close of one of today's option bars: the time exit,
// the exit on a cross down, and the day's one entry on a cross up.
func (s *emaCrossFirst) onLegBar(ctx strategy.Context, leg *ecLeg, minute int) error {
	if ctx.Pending() > 0 {
		return nil
	}
	if leg.open && minute >= s.flat {
		return s.sell(ctx, leg)
	}
	i := len(leg.bars) - 1
	if i < 1 || i < leg.nWarm {
		return nil
	}
	ef, es := indicator.EMA(leg.bars, s.fast), indicator.EMA(leg.bars, s.slow)
	if math.IsNaN(es[i-1]) {
		return nil
	}
	was, is := ef[i-1] > es[i-1], ef[i] > es[i]
	switch {
	case leg.open && was && !is:
		return s.sell(ctx, leg)
	case !s.traded && !was && is && minute < s.lastEntry:
		s.traded = true
		return ignoreRejection(ctx.Submit(leg.tag, market.Leg{Instrument: leg.inst, Side: market.Buy, Lots: s.lots}))
	}
	return nil
}

func (s *emaCrossFirst) sell(ctx strategy.Context, leg *ecLeg) error {
	for _, p := range ctx.Positions() {
		if p.Instrument.Key() == leg.inst.Key() && p.Qty > 0 {
			return ignoreRejection(ctx.Submit(leg.tag, market.Leg{Instrument: p.Instrument, Side: market.Sell, Lots: p.Qty / p.Instrument.LotSize}))
		}
	}
	return nil
}

func (s *emaCrossFirst) OnFill(_ strategy.Context, f market.Fill) error {
	leg := s.legs[f.Instrument.Key()]
	if leg == nil {
		return nil
	}
	leg.open = f.Kind == market.Traded && f.Side == market.Buy
	return nil
}
