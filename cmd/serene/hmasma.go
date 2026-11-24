package main

import (
	"time"

	"serene/internal/cost"
	"serene/internal/indicator"
	"serene/internal/market"
	"serene/internal/mcx"
)

// The 10-minute HMA-34 / SMA-89 crossover, long and short, picked on
// 2026-11-24 from `serene research mcxma10` (best by worst metal of 198
// variants on GOLDM and SILVERM, then checked once on the held-back
// 2026-11-11..23 sessions). It trades live on Zerodha (serene live mcx);
// see docs/strategies/hmasma_10m.md. None of these may change: a change is a
// new strategy.
const (
	hsFrame = 10 * time.Minute
	hsFast  = 34 // HMA of closes
	hsSlow  = 89 // SMA of closes
)

var hsForward = market.Date{Year: 2026, Month: time.November, Day: 24}

// hsStrategies maps each hmasma strategy to its commodity.
var hsStrategies = map[string]string{"goldm_hmasma_10m": gemName, "silverm_hmasma_10m": smName}

// hsConfig is the frozen rule on name.
func hsConfig(name string, spec mcx.Spec, costs *cost.Schedule, slip int64, from, to market.Date) mcx.EMAConfig {
	return mcx.EMAConfig{Name: name, Spec: spec, Frame: hsFrame, Fast: hsFast, Slow: hsSlow,
		MA: indicator.HMA, SlowMA: indicator.SMA, Slip: slip, Costs: costs, From: from, To: to}
}
