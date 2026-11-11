package strategies

import (
	"fmt"
	"math"
	"time"

	"serene/internal/market"
	"serene/internal/strategy"
)

// pdBreakTrail buys a NIFTY option when the index breaks its previous day's
// range and the option breaks its own previous day high.
//
// The strike is the one nearest the previous day's index low plus
// offset_pct, in the nearest expiry; the call and the put at it are both
// watched. The call is bought on the first 1-minute close where the index
// closes above its previous day high and the call closes above the call's
// previous day high; the put where the index closes below its previous day
// low and the put closes above the put's previous day high. Each side trades
// at most once a day, filling in the next minute; no entries from
// last_entry_minute.
//
// There is no stop. Once the index has closed trail_after points in the
// trade's favour (measured from the index open of the entry minute), the
// trade exits on the first index close trail points short of its best close.
// Otherwise it exits at exit_minute.
//
// The option's previous day high needs that day's bars of a strike only known
// at its close, so every contract of the two nearest expiries within
// band_strikes of the index is recorded all day.
//
// Frozen 2026-11-09 from `serene research pdbreak` (exit idx200); see
// docs/strategies/pd_break_trail.md.
type pdBreakTrail struct {
	underlying      string
	offset          float64
	step            float64
	trailAfter      market.Paise
	trail           market.Paise
	lastEntry, flat int
	lots            int64
	band            int

	day       market.Date
	idxHi     market.Paise // today so far
	idxLo     market.Paise
	pdh, pdl  market.Paise // yesterday
	haveRange bool
	highs     map[string]market.Paise // today's high of each recorded contract
	prevHighs map[string]market.Paise
	recorded  map[string]bool
	legs      map[string]*pdLeg
	chosen    bool
	idxBar    market.Bar // the latest index bar
	scanned   time.Time  // the last chain scan for contracts to record
	scanLo    float64    // the band that scan covered
	scanHi    float64
}

type pdLeg struct {
	inst    market.Instrument
	tag     string
	pdh     market.Paise
	last    market.Bar // the latest option bar
	checked time.Time  // the last minute the entry was checked
	entered bool
	ref     market.Paise // index open of the entry minute; 0 until known
	fillAt  time.Time
	best    market.Paise
	armed   bool
	sellAt  time.Time
}

var pdBreakParams = []string{"offset_pct", "strike_step", "trail_after", "trail", "last_entry_minute", "exit_minute", "lots", "band_strikes"}

func newPDBreakTrail(u string, p strategy.Params) (strategy.Strategy, error) {
	if err := p.Check(pdBreakParams...); err != nil {
		return nil, err
	}
	s := &pdBreakTrail{underlying: u,
		offset:     p.Get("offset_pct", 1),
		step:       p.Get("strike_step", 50),
		trailAfter: market.Rupees(p.Get("trail_after", 200)),
		trail:      market.Rupees(p.Get("trail", 50)),
		lastEntry:  int(p.Get("last_entry_minute", 345)),
		flat:       int(p.Get("exit_minute", 360)),
		lots:       int64(p.Get("lots", 1)),
		band:       int(p.Get("band_strikes", 40)),
	}
	if s.step <= 0 || s.trailAfter <= 0 || s.trail <= 0 || s.lastEntry < 1 || s.flat < s.lastEntry || s.lots < 1 || s.band < 1 {
		return nil, fmt.Errorf("pd_break_trail: bad parameters %+v", p)
	}
	return s, nil
}

func (s *pdBreakTrail) Init(ctx strategy.Context) error {
	return ctx.Subscribe(indexKey(s.underlying), market.Minute1)
}

func (s *pdBreakTrail) newDay(d market.Date) {
	if !s.day.IsZero() && s.idxHi > 0 {
		s.pdh, s.pdl, s.haveRange = s.idxHi, s.idxLo, true
	}
	s.prevHighs = s.highs
	s.day, s.idxHi, s.idxLo = d, 0, 0
	s.highs, s.recorded, s.legs, s.chosen = map[string]market.Paise{}, map[string]bool{}, map[string]*pdLeg{}, false
}

func (s *pdBreakTrail) OnBar(ctx strategy.Context, b market.Bar) error {
	if d := market.DateOf(b.Time); d != s.day {
		s.newDay(d)
	}
	if b.Timeframe != market.Minute1 {
		return nil
	}
	if s.recorded[b.Key] {
		s.highs[b.Key] = max(s.highs[b.Key], b.High)
	}
	if b.Key == indexKey(s.underlying) {
		return s.onIndex(ctx, b)
	}
	if leg := s.legs[b.Key]; leg != nil {
		leg.last = b
		if s.idxBar.Time.Equal(b.Time) {
			return s.check(ctx, leg)
		}
	}
	return nil
}

func (s *pdBreakTrail) onIndex(ctx strategy.Context, b market.Bar) error {
	if s.idxHi == 0 {
		s.idxHi, s.idxLo = b.High, b.Low
	}
	s.idxHi, s.idxLo = max(s.idxHi, b.High), min(s.idxLo, b.Low)
	s.idxBar = b
	if err := s.record(ctx, b.Close); err != nil {
		return err
	}
	if !s.chosen {
		s.chosen = true
		if err := s.choose(ctx); err != nil {
			return err
		}
	}
	for _, leg := range s.legs {
		if leg.entered && leg.ref == 0 && !b.Time.Before(leg.fillAt) && !leg.fillAt.IsZero() {
			leg.ref = b.Open
		}
		if err := s.check(ctx, leg); err != nil {
			return err
		}
	}
	return nil
}

// record subscribes to every contract of the two nearest expiries within the
// band around spot, so tomorrow knows each one's high today.
func (s *pdBreakTrail) record(ctx strategy.Context, spot market.Paise) error {
	// Chain scans every contract, so rescan only every 5 minutes or when
	// spot leaves the band already covered. A contract first trading in
	// between is picked up from its latest bar.
	atm := math.Round(spot.Rupees()/s.step) * s.step
	now := ctx.Now()
	if !s.scanned.IsZero() && market.DateOf(s.scanned) == s.day && now.Sub(s.scanned) < 5*time.Minute &&
		atm-s.scanLo >= 10*s.step && s.scanHi-atm >= 10*s.step {
		return nil
	}
	w := float64(s.band) * s.step
	s.scanned, s.scanLo, s.scanHi = now, atm-w, atm+w
	for i, exp := range ctx.Expiries(s.underlying) {
		if i == 2 {
			break
		}
		for _, q := range ctx.Chain(s.underlying, exp) {
			key := q.Instrument.Key()
			if s.recorded[key] || math.Abs(q.Instrument.Strike.Rupees()-atm) > float64(s.band)*s.step+0.5 {
				continue
			}
			if err := ctx.Subscribe(key, market.Minute1); err != nil {
				return err
			}
			s.recorded[key] = true
			// This minute's bar may have come before the index's.
			if last, ok := ctx.Last(key); ok && market.DateOf(last.Time) == s.day {
				s.highs[key] = max(s.highs[key], last.High)
			}
		}
	}
	return nil
}

// choose picks today's call and put from yesterday's index low.
func (s *pdBreakTrail) choose(ctx strategy.Context) error {
	if !s.haveRange {
		return nil
	}
	expiries := ctx.Expiries(s.underlying)
	if len(expiries) == 0 {
		return nil
	}
	strike := market.Rupees(math.Round(s.pdl.Rupees()*(1+s.offset/100)/s.step) * s.step)
	for _, q := range ctx.Chain(s.underlying, expiries[0]) {
		if q.Instrument.Strike != strike {
			continue
		}
		key := q.Instrument.Key()
		pdh, ok := s.prevHighs[key]
		if !ok {
			continue // not recorded yesterday
		}
		leg := &pdLeg{inst: q.Instrument, tag: fmt.Sprintf("pdb-%s-%s", s.day, q.Instrument.Right), pdh: pdh}
		if err := ctx.Subscribe(key, market.Minute1); err != nil {
			return err
		}
		if last, ok := ctx.Last(key); ok && last.Time.Equal(s.idxBar.Time) {
			leg.last = last
		}
		s.legs[key] = leg
	}
	return nil
}

// check acts at the close of a minute that has both an index and an option
// bar: the entry before it, the trail and time exit after.
func (s *pdBreakTrail) check(ctx strategy.Context, leg *pdLeg) error {
	ib := s.idxBar
	minute := minutesIntoSession(ib)
	held := s.held(ctx, leg)
	if leg.entered {
		if !held {
			return nil
		}
		if !leg.sellAt.IsZero() && ib.Time.Sub(leg.sellAt) <= 3*time.Minute {
			return nil // a sell is working
		}
		if minute >= s.flat {
			return s.sell(ctx, leg, ib.Time)
		}
		if leg.ref == 0 {
			return nil
		}
		move := ib.Close - leg.ref
		if leg.inst.Right == market.Put {
			move = leg.ref - ib.Close
		}
		leg.best = max(leg.best, move)
		if leg.best >= s.trailAfter {
			leg.armed = true
		}
		if leg.armed && move <= leg.best-s.trail {
			return s.sell(ctx, leg, ib.Time)
		}
		return nil
	}
	if !leg.last.Time.Equal(ib.Time) || leg.checked.Equal(ib.Time) || minute >= s.lastEntry {
		return nil
	}
	leg.checked = ib.Time
	brk := ib.Close > s.pdh
	if leg.inst.Right == market.Put {
		brk = ib.Close < s.pdl
	}
	if !brk || leg.last.Close <= leg.pdh {
		return nil
	}
	leg.entered, leg.fillAt = true, ib.End
	return ignoreRejection(ctx.Submit(leg.tag, market.Leg{Instrument: leg.inst, Side: market.Buy, Lots: s.lots}))
}

func (s *pdBreakTrail) held(ctx strategy.Context, leg *pdLeg) bool {
	for _, p := range ctx.Positions() {
		if p.Instrument.Key() == leg.inst.Key() && p.Qty > 0 {
			return true
		}
	}
	return false
}

func (s *pdBreakTrail) sell(ctx strategy.Context, leg *pdLeg, at time.Time) error {
	for _, p := range ctx.Positions() {
		if p.Instrument.Key() == leg.inst.Key() && p.Qty > 0 {
			leg.sellAt = at
			return ignoreRejection(ctx.Submit(leg.tag, market.Leg{Instrument: p.Instrument, Side: market.Sell, Lots: p.Qty / p.Instrument.LotSize}))
		}
	}
	return nil
}

func (s *pdBreakTrail) OnFill(_ strategy.Context, f market.Fill) error {
	if leg := s.legs[f.Instrument.Key()]; leg != nil && f.Kind == market.Traded && f.Side == market.Buy {
		leg.fillAt = f.Time
	}
	return nil
}
