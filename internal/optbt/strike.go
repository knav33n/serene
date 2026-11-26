package optbt

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"serene/internal/market"
)

// RuleKind is how a strike is chosen.
type RuleKind string

const (
	KindATM  RuleKind = "atm"    // at the money
	KindOTM  RuleKind = "otm"    // V points out of the money
	KindPct  RuleKind = "atmpct" // V% of spot out of the money
	KindSW   RuleKind = "sw"     // V × ATM straddle premium out of the money
	KindCP   RuleKind = "cp"     // premium closest to ₹V
	KindCPSP RuleKind = "cpsp"   // premium closest to V% of the ATM straddle
)

// Rule picks a strike (or, for a hedge, a wing beyond the sold strike).
type Rule struct {
	Name string
	Kind RuleKind
	V    float64
}

// ParseRule reads atm, otm200, atmpct1, sw0.5, cp100 or cpsp25.
func ParseRule(s string) (Rule, error) {
	if s == "atm" {
		return Rule{Name: s, Kind: KindATM}, nil
	}
	for _, k := range []RuleKind{KindPct, KindCPSP, KindOTM, KindSW, KindCP} { // longer prefixes first
		if rest, ok := strings.CutPrefix(s, string(k)); ok {
			v, err := strconv.ParseFloat(rest, 64)
			if err != nil || v <= 0 {
				return Rule{}, fmt.Errorf("strike rule %q: bad value", s)
			}
			return Rule{Name: s, Kind: k, V: v}, nil
		}
	}
	return Rule{}, fmt.Errorf("strike rule %q: want atm, otm<pts>, atmpct<x>, sw<k>, cp<₹> or cpsp<pct>", s)
}

type indexInfo struct {
	Exch     market.Exchange
	IndexKey string
	Step     market.Paise // listed strike interval
}

var indices = map[string]indexInfo{
	"NIFTY":  {market.NFO, "NSE:NIFTY", market.Rupees(50)},
	"SENSEX": {market.BFO, "BSE:SENSEX", market.Rupees(100)},
}

const searchSteps = 60 // how far from ATM a closest-premium search goes, in grid steps

func roundTo(x, step market.Paise) market.Paise {
	return market.Paise(math.Round(float64(x)/float64(step))) * step
}

type pickKey struct {
	rule  Rule
	round int
	right market.Right
	exp   market.Date
	slot  int
}

type hedgeKey struct {
	rule   Rule
	strike market.Paise
	right  market.Right
	exp    market.Date
	slot   int
}

// pick returns the option to sell for one side, which must have a bar at slot.
// Results are cached per day.
func (d *Day) pick(r Rule, round int, right market.Right, exp market.Date, slot int) *opt {
	k := pickKey{r, round, right, exp, slot}
	d.mu.RLock()
	o, ok := d.picks[k]
	d.mu.RUnlock()
	if ok {
		return o
	}
	o = d.pickUncached(r, round, right, exp, slot)
	d.mu.Lock()
	d.picks[k] = o
	d.mu.Unlock()
	return o
}

func (d *Day) pickUncached(r Rule, round int, right market.Right, exp market.Date, slot int) *opt {
	if !d.Index.has[slot] {
		return nil
	}
	spot := d.Index.o[slot]
	step := d.info.Step
	grid := step
	if round > 0 {
		grid = market.Rupees(float64(round))
	}
	atm := roundTo(spot, step)
	dir := market.Paise(1)
	if right == market.Put {
		dir = -1
	}
	at := func(strike market.Paise) *opt {
		o := d.opts[optKey{exp, strike, right}]
		if o == nil || !o.s.has[slot] {
			return nil
		}
		return o
	}
	straddle := func() (market.Paise, bool) {
		ce, pe := d.opts[optKey{exp, atm, market.Call}], d.opts[optKey{exp, atm, market.Put}]
		if ce == nil || pe == nil || !ce.s.has[slot] || !pe.s.has[slot] {
			return 0, false
		}
		return ce.s.o[slot] + pe.s.o[slot], true
	}
	closest := func(target market.Paise) *opt {
		var best *opt
		var bd market.Paise = -1
		base := roundTo(atm, grid)
		for s := 0; s <= searchSteps; s++ {
			o := at(base + dir*market.Paise(s)*grid)
			if o == nil {
				continue
			}
			dd := o.s.o[slot] - target
			if dd < 0 {
				dd = -dd
			}
			if bd < 0 || dd < bd {
				best, bd = o, dd
			}
		}
		return best
	}
	switch r.Kind {
	case KindATM:
		return at(roundTo(spot, grid))
	case KindOTM:
		return at(roundTo(atm+dir*market.Rupees(r.V), grid))
	case KindPct:
		return at(roundTo(spot+dir*market.Paise(float64(spot)*r.V/100), grid))
	case KindSW:
		sp, ok := straddle()
		if !ok {
			return nil
		}
		return at(roundTo(atm+dir*market.Paise(float64(sp)*r.V), grid))
	case KindCP:
		return closest(market.Rupees(r.V))
	case KindCPSP:
		sp, ok := straddle()
		if !ok {
			return nil
		}
		return closest(market.Paise(float64(sp) * r.V / 100))
	}
	return nil
}

// hedge returns the wing to buy beyond a sold strike: otm<pts> further out, or
// the strike further out whose premium is closest to cp<₹>.
func (d *Day) hedge(r Rule, sold market.Paise, right market.Right, exp market.Date, slot int) *opt {
	k := hedgeKey{r, sold, right, exp, slot}
	d.mu.RLock()
	o, ok := d.hedges[k]
	d.mu.RUnlock()
	if ok {
		return o
	}
	dir := market.Paise(1)
	if right == market.Put {
		dir = -1
	}
	at := func(strike market.Paise) *opt {
		o := d.opts[optKey{exp, strike, right}]
		if o == nil || !o.s.has[slot] {
			return nil
		}
		return o
	}
	step := d.info.Step
	switch r.Kind {
	case KindOTM:
		o = at(roundTo(sold+dir*market.Rupees(r.V), step))
	case KindCP:
		target := market.Rupees(r.V)
		var bd market.Paise = -1
		for s := 1; s <= 2*searchSteps; s++ {
			c := at(sold + dir*market.Paise(s)*step)
			if c == nil {
				continue
			}
			dd := c.s.o[slot] - target
			if dd < 0 {
				dd = -dd
			}
			if bd < 0 || dd < bd {
				o, bd = c, dd
			}
		}
	}
	d.mu.Lock()
	d.hedges[k] = o
	d.mu.Unlock()
	return o
}
