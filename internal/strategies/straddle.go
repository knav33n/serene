package strategies

import (
	"errors"
	"fmt"

	"calm/internal/market"
	"calm/internal/strategy"
)

// shortStraddle sells the at-the-money call and put of the nearest expiry at a
// fixed time, then exits on a premium stop or at a fixed time.
type shortStraddle struct {
	underlying  string
	entry, exit int
	stopPct     float64
	lots        int64
	day         market.Date
	traded      bool
	credit      market.Paise
	tag         string
}

func newShortStraddle(u string, p strategy.Params) (strategy.Strategy, error) {
	if err := p.Check("entry_minute", "exit_minute", "stop_pct", "lots"); err != nil {
		return nil, err
	}
	s := &shortStraddle{underlying: u,
		entry:   int(p.Get("entry_minute", 15)),
		exit:    int(p.Get("exit_minute", 360)),
		stopPct: p.Get("stop_pct", 30),
		lots:    int64(p.Get("lots", 1)),
	}
	if s.entry >= s.exit || s.lots < 1 {
		return nil, fmt.Errorf("short_straddle: bad parameters %+v", p)
	}
	return s, nil
}

func (s *shortStraddle) Init(ctx strategy.Context) error {
	return ctx.Subscribe(indexKey(s.underlying), market.Minute1)
}

func (s *shortStraddle) OnBar(ctx strategy.Context, b market.Bar) error {
	if d := market.DateOf(b.Time); d != s.day {
		s.day, s.traded, s.credit = d, false, 0
		s.tag = "straddle-" + d.String()
	}
	if ctx.Pending() > 0 {
		return nil
	}
	minute := minutesIntoSession(b)
	if positions := ctx.Positions(); len(positions) > 0 {
		var value market.Paise
		for _, p := range positions {
			if q, ok := ctx.Last(p.Instrument.Key()); ok {
				value += q.Close
			}
		}
		stopped := s.credit > 0 && float64(value) >= float64(s.credit)*(1+s.stopPct/100)
		if stopped || minute >= s.exit {
			return ignoreRejection(closeAll(ctx, s.tag))
		}
		return nil
	}
	if s.traded || minute < s.entry || minute >= s.exit {
		return nil
	}
	spot, ok := ctx.Spot(s.underlying)
	expiries := ctx.Expiries(s.underlying)
	if !ok || len(expiries) == 0 {
		return nil
	}
	ce, pe, ok := strategy.ATMPair(ctx.Chain(s.underlying, expiries[0]), spot)
	if !ok {
		return nil
	}
	s.traded = true
	return ignoreRejection(ctx.Submit(s.tag,
		market.Leg{Instrument: ce.Instrument, Side: market.Sell, Lots: s.lots},
		market.Leg{Instrument: pe.Instrument, Side: market.Sell, Lots: s.lots}))
}

func (s *shortStraddle) OnFill(_ strategy.Context, f market.Fill) error {
	if f.Kind == market.Traded && f.Side == market.Sell {
		s.credit += f.Price
	}
	return nil
}

// ignoreRejection treats a refused order as "no trade" and keeps the run going.
// Rejections are counted in the results.
func ignoreRejection(err error) error {
	if errors.Is(err, strategy.ErrRejected) {
		return nil
	}
	return err
}
