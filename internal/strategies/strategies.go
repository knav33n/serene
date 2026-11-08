// Package strategies holds the strategy registry, two example strategies,
// lower_high_break and rsi_divergence. The examples demonstrate the interface; none of them is a
// trading recommendation.
package strategies

import (
	"fmt"
	"sort"

	"calm/internal/market"
	"calm/internal/strategy"
)

var registry = map[string]func(underlying string, p strategy.Params) (strategy.Strategy, error){
	"short_straddle":   newShortStraddle,
	"breakout":         newBreakout,
	"lower_high_break": newLowerHighBreak,
	"rsi_divergence":   newRSIDivergence,
}

// Rejected maps each strategy that failed its test to a note saying where
// the write-up is. They stay in the registry so past results can be
// reproduced, but research runs warn, and none should go to paper or live.
var Rejected = map[string]string{
	"lower_high_break": "failed its holdout on 2026-11-08 (PF 0.44 on 2026-05-01 to 2026-11-03); see docs/strategies/rejected/lower_high_break.md",
	"rsi_divergence":   "no edge in five option trials or on daily stocks, 2026-11-08 (best PF 1.09 on 12 trades); see docs/strategies/rejected/rsi_divergence.md",
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
