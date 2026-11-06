// Package strategy is the interface every trading idea implements. A strategy
// cannot tell whether it is in a backtest, on paper or live.
package strategy

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"calm/internal/market"
	"calm/internal/portfolio"
)

// Strategy is a trading idea.
type Strategy interface {
	// Init subscribes to the data the strategy needs.
	Init(ctx Context) error
	// OnBar receives each subscribed bar once it has closed.
	OnBar(ctx Context, b market.Bar) error
	// OnFill receives each execution, including expiry settlement.
	OnFill(ctx Context, f market.Fill) error
}

// ErrRejected means an order was refused, for example for lack of margin. It
// is not fatal, so strategies can decide how to respond.
var ErrRejected = errors.New("order rejected")

// Quote is a contract with its latest closed 1-minute bar today.
type Quote struct {
	Instrument market.Instrument
	Last       market.Bar
}

// Context is everything a strategy may see or do. It only ever exposes data
// that exists at Now.
type Context interface {
	Now() time.Time
	// Subscribe requests bars of tf for an instrument, starting now.
	Subscribe(key string, tf market.Timeframe) error
	// Spot returns the index's latest close today.
	Spot(underlying string) (market.Paise, bool)
	// Expiries lists expiries with a quote today, nearest first.
	Expiries(underlying string) []market.Date
	// Chain returns contracts of an expiry that have traded today, by strike.
	Chain(underlying string, expiry market.Date) []Quote
	// Last returns an instrument's latest closed 1-minute bar.
	Last(key string) (market.Bar, bool)
	Positions() []portfolio.Position
	// Pending counts open orders.
	Pending() int
	// Submit places legs as one order. It returns ErrRejected if refused.
	Submit(tag string, legs ...market.Leg) error
}

// Params are a strategy's tunable numbers.
type Params map[string]float64

// Get returns a parameter or its default.
func (p Params) Get(name string, def float64) float64 {
	if v, ok := p[name]; ok {
		return v
	}
	return def
}

// Check rejects unknown names, so a typo in a sweep cannot silently test the
// default value over and over.
func (p Params) Check(known ...string) error {
	for name := range p {
		found := false
		for _, k := range known {
			found = found || k == name
		}
		if !found {
			sort.Strings(known)
			return fmt.Errorf("unknown parameter %q (known: %v)", name, known)
		}
	}
	return nil
}

// ATM returns the contract of a right whose strike is nearest spot.
func ATM(chain []Quote, spot market.Paise, right market.Right) (Quote, bool) {
	var best Quote
	found := false
	var bestDist market.Paise
	for _, q := range chain {
		if q.Instrument.Right != right {
			continue
		}
		d := q.Instrument.Strike - spot
		if d < 0 {
			d = -d
		}
		if !found || d < bestDist {
			best, bestDist, found = q, d, true
		}
	}
	return best, found
}

// ATMPair returns the nearest strike at which both call and put are quoted.
func ATMPair(chain []Quote, spot market.Paise) (ce, pe Quote, ok bool) {
	puts := map[market.Paise]Quote{}
	for _, q := range chain {
		if q.Instrument.Right == market.Put {
			puts[q.Instrument.Strike] = q
		}
	}
	var bestDist market.Paise
	for _, q := range chain {
		p, both := puts[q.Instrument.Strike]
		if q.Instrument.Right != market.Call || !both {
			continue
		}
		d := q.Instrument.Strike - spot
		if d < 0 {
			d = -d
		}
		if !ok || d < bestDist {
			ce, pe, bestDist, ok = q, p, d, true
		}
	}
	return ce, pe, ok
}
