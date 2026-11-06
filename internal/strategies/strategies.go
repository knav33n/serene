// Package strategies holds the strategy registry and two example strategies.
// The examples demonstrate the interface; they are not trading recommendations.
package strategies

import (
	"fmt"
	"sort"

	"calm/internal/market"
	"calm/internal/strategy"
)

var registry = map[string]func(underlying string, p strategy.Params) (strategy.Strategy, error){
	"short_straddle": newShortStraddle,
	"breakout":       newBreakout,
}

// New builds a strategy by name.
func New(name, underlying string, p strategy.Params) (strategy.Strategy, error) {
	f, ok := registry[name]
	if !ok {
		names := make([]string, 0, len(registry))
		for n := range registry {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("unknown strategy %q (known: %v)", name, names)
	}
	return f(underlying, p)
}

func indexKey(underlying string) string {
	if underlying == "SENSEX" || underlying == "BANKEX" {
		return market.Index(market.BSE, underlying).Key()
	}
	return market.Index(market.NSE, underlying).Key()
}

func minutesIntoSession(b market.Bar) int {
	return int(b.End.Sub(market.NSESession.Open(market.DateOf(b.Time))).Minutes())
}

// closeAll submits an order flattening every open position.
func closeAll(ctx strategy.Context, tag string) error {
	var legs []market.Leg
	for _, p := range ctx.Positions() {
		side, qty := market.Sell, p.Qty
		if p.Qty < 0 {
			side, qty = market.Buy, -p.Qty
		}
		legs = append(legs, market.Leg{Instrument: p.Instrument, Side: side, Lots: qty / p.Instrument.LotSize})
	}
	return ctx.Submit(tag, legs...)
}
