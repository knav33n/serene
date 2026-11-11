// Package margin estimates option margin for backtests. It is an approximation
// and not SPAN: live trading must use the broker's own margin figures.
package margin

import (
	"fmt"
	"sort"

	"serene/internal/market"
)

// Holding is a signed quantity of a contract.
type Holding struct {
	Instrument market.Instrument
	Qty        int64
}

// Model holds the margin assumptions.
type Model struct {
	// NakedPct is the margin for an unhedged short, as a percent of index
	// notional.
	NakedPct float64
}

type group struct {
	underlying string
	expiry     market.Date
	right      market.Right
}

// Required returns the estimated margin. Long options need none. Each short
// is paired with the closest long of the same expiry and right, and pays that
// spread's maximum loss, capped at the naked margin. Any remainder is naked.
func (m Model) Required(hs []Holding, spot func(underlying string) (market.Paise, bool)) (market.Paise, error) {
	shorts := map[group][]Holding{}
	longs := map[group][]Holding{}
	for _, h := range hs {
		if h.Instrument.Kind != market.KindOption {
			return 0, fmt.Errorf("margin model covers options only, got %s", h.Instrument.Key())
		}
		g := group{h.Instrument.Underlying, h.Instrument.Expiry, h.Instrument.Right}
		if h.Qty < 0 {
			shorts[g] = append(shorts[g], Holding{h.Instrument, -h.Qty})
		} else if h.Qty > 0 {
			longs[g] = append(longs[g], h)
		}
	}
	var total market.Paise
	for g, ss := range shorts {
		s, ok := spot(g.underlying)
		if !ok {
			return 0, fmt.Errorf("no index price for %s", g.underlying)
		}
		perUnitNaked := market.Paise(float64(s) * m.NakedPct / 100)
		ls := longs[g]
		sort.Slice(ss, func(i, j int) bool { return ss[i].Instrument.Strike < ss[j].Instrument.Strike })
		sort.Slice(ls, func(i, j int) bool { return ls[i].Instrument.Strike < ls[j].Instrument.Strike })
		for _, short := range ss {
			remaining := short.Qty
			for remaining > 0 {
				i := closest(ls, short.Instrument.Strike)
				if i < 0 {
					total += perUnitNaked * market.Paise(remaining)
					break
				}
				q := min(remaining, ls[i].Qty)
				risk := ls[i].Instrument.Strike - short.Instrument.Strike
				if g.right == market.Put {
					risk = -risk
				}
				total += min(max(risk, 0), perUnitNaked) * market.Paise(q)
				remaining -= q
				ls[i].Qty -= q
			}
		}
	}
	return total, nil
}

func closest(ls []Holding, strike market.Paise) int {
	best, bestDist := -1, market.Paise(0)
	for i, l := range ls {
		if l.Qty == 0 {
			continue
		}
		dist := l.Instrument.Strike - strike
		if dist < 0 {
			dist = -dist
		}
		if best < 0 || dist < bestDist {
			best, bestDist = i, dist
		}
	}
	return best
}
