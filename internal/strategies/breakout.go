package strategies

import (
	"fmt"

	"serene/internal/market"
	"serene/internal/strategy"
)

// breakout tracks the opening range on 1-minute bars, then buys an at-the-money
// option when a higher-timeframe bar closes outside the range. It shows how a
// strategy uses several timeframes at once.
type breakout struct {
	underlying string
	rangeMins  int
	tf         market.Timeframe
	exit       int
	stopPct    float64
	targetPct  float64
	lots       int64

	day       market.Date
	high, low market.Paise
	traded    bool
	entry     market.Paise
	tag       string
}

func newBreakout(u string, p strategy.Params) (strategy.Strategy, error) {
	if err := p.Check("range_minutes", "timeframe", "exit_minute", "stop_pct", "target_pct", "lots"); err != nil {
		return nil, err
	}
	s := &breakout{underlying: u,
		rangeMins: int(p.Get("range_minutes", 30)),
		tf:        market.Timeframe(p.Get("timeframe", 5)),
		exit:      int(p.Get("exit_minute", 360)),
		stopPct:   p.Get("stop_pct", 30),
		targetPct: p.Get("target_pct", 60),
		lots:      int64(p.Get("lots", 1)),
	}
	if s.rangeMins >= s.exit || s.tf <= 1 || s.lots < 1 {
		return nil, fmt.Errorf("breakout: bad parameters %+v", p)
	}
	return s, nil
}

func (s *breakout) Init(ctx strategy.Context) error {
	k := indexKey(s.underlying)
	if err := ctx.Subscribe(k, market.Minute1); err != nil {
		return err
	}
	return ctx.Subscribe(k, s.tf)
}

func (s *breakout) OnBar(ctx strategy.Context, b market.Bar) error {
	if d := market.DateOf(b.Time); d != s.day {
		s.day, s.traded, s.high, s.low, s.entry = d, false, 0, 0, 0
		s.tag = "breakout-" + d.String()
	}
	if b.Timeframe == market.Minute1 {
		return s.onMinute(ctx, b)
	}
	minute := minutesIntoSession(b)
	if s.traded || minute <= s.rangeMins || minute >= s.exit || s.high == 0 ||
		ctx.Pending() > 0 || len(ctx.Positions()) > 0 {
		return nil
	}
	var right market.Right
	switch {
	case b.Close > s.high:
		right = market.Call
	case b.Close < s.low:
		right = market.Put
	default:
		return nil
	}
	expiries := ctx.Expiries(s.underlying)
	if len(expiries) == 0 {
		return nil
	}
	q, ok := strategy.ATM(ctx.Chain(s.underlying, expiries[0]), b.Close, right)
	if !ok {
		return nil
	}
	s.traded = true
	return ignoreRejection(ctx.Submit(s.tag, market.Leg{Instrument: q.Instrument, Side: market.Buy, Lots: s.lots}))
}

func (s *breakout) onMinute(ctx strategy.Context, b market.Bar) error {
	minute := minutesIntoSession(b)
	if minute <= s.rangeMins {
		if s.high == 0 {
			s.high, s.low = b.High, b.Low
		}
		s.high = max(s.high, b.High)
		s.low = min(s.low, b.Low)
		return nil
	}
	positions := ctx.Positions()
	if len(positions) == 0 || ctx.Pending() > 0 {
		return nil
	}
	q, ok := ctx.Last(positions[0].Instrument.Key())
	if !ok {
		return nil
	}
	px := float64(q.Close)
	entry := float64(s.entry)
	if s.entry == 0 {
		return nil
	}
	if px <= entry*(1-s.stopPct/100) || px >= entry*(1+s.targetPct/100) || minute >= s.exit {
		return ignoreRejection(closeAll(ctx, s.tag))
	}
	return nil
}

func (s *breakout) OnFill(_ strategy.Context, f market.Fill) error {
	if f.Kind == market.Traded && f.Side == market.Buy {
		s.entry = f.Price
	}
	return nil
}
