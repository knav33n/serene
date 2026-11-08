// Package indicator computes bar indicators. Value i uses bars 0..i only, so it
// is known at the close of bar i.
package indicator

import (
	"math"

	"calm/internal/market"
)

// EMA returns the n-bar exponential moving average of closes in rupees. It is
// seeded with the simple average of the first n closes, as most charts do,
// and is NaN before that.
func EMA(bars []market.Bar, n int) []float64 {
	out := make([]float64, len(bars))
	alpha := 2 / float64(n+1)
	sum := 0.0
	for i, b := range bars {
		c := b.Close.Rupees()
		switch {
		case i < n-1:
			sum += c
			out[i] = math.NaN()
		case i == n-1:
			out[i] = (sum + c) / float64(n)
		default:
			out[i] = out[i-1] + alpha*(c-out[i-1])
		}
	}
	return out
}

// VWAP returns the session volume-weighted average of the typical price
// (high+low+close)/3 in rupees, restarting each day. Before any volume trades
// in a session it is that bar's typical price.
func VWAP(bars []market.Bar) []float64 {
	out := make([]float64, len(bars))
	var day market.Date
	var pv, vol float64
	for i, b := range bars {
		if d := market.DateOf(b.Time); d != day {
			day, pv, vol = d, 0, 0
		}
		tp := (b.High.Rupees() + b.Low.Rupees() + b.Close.Rupees()) / 3
		pv += tp * float64(b.Volume)
		vol += float64(b.Volume)
		if vol == 0 {
			out[i] = tp
		} else {
			out[i] = pv / vol
		}
	}
	return out
}

// ATR returns the n-bar average true range in rupees, smoothed as Wilder did
// and seeded with the simple average of the first n true ranges. It is NaN
// before that. The first bar's true range is its high minus its low.
func ATR(bars []market.Bar, n int) []float64 {
	out := make([]float64, len(bars))
	sum := 0.0
	for i, b := range bars {
		tr := b.High.Rupees() - b.Low.Rupees()
		if i > 0 {
			pc := bars[i-1].Close.Rupees()
			tr = max(tr, math.Abs(b.High.Rupees()-pc), math.Abs(b.Low.Rupees()-pc))
		}
		switch {
		case i < n-1:
			sum += tr
			out[i] = math.NaN()
		case i == n-1:
			out[i] = (sum + tr) / float64(n)
		default:
			out[i] = (out[i-1]*float64(n-1) + tr) / float64(n)
		}
	}
	return out
}

// ADX returns Wilder's n-bar average directional index, from 0 to 100: how
// strongly price is trending, in either direction. Each of the true range and
// the directional moves is smoothed as Wilder did, seeded with the sum of its
// first n values, and ADX itself is seeded with the mean of the first n
// directional index values. It is NaN for the first 2n-1 bars.
func ADX(bars []market.Bar, n int) []float64 {
	out := make([]float64, len(bars))
	for i := range out {
		out[i] = math.NaN()
	}
	var tr, plus, minus, dxSum, adx float64
	for i := 1; i < len(bars); i++ {
		h, l, pc := bars[i].High.Rupees(), bars[i].Low.Rupees(), bars[i-1].Close.Rupees()
		up, down := h-bars[i-1].High.Rupees(), bars[i-1].Low.Rupees()-l
		pdm, mdm := 0.0, 0.0
		if up > down && up > 0 {
			pdm = up
		}
		if down > up && down > 0 {
			mdm = down
		}
		t := max(h-l, math.Abs(h-pc), math.Abs(l-pc))
		if i <= n {
			tr, plus, minus = tr+t, plus+pdm, minus+mdm
		} else {
			f := float64(n-1) / float64(n)
			tr, plus, minus = tr*f+t, plus*f+pdm, minus*f+mdm
		}
		if i < n {
			continue
		}
		dx := 0.0
		if tr > 0 {
			pdi, mdi := 100*plus/tr, 100*minus/tr
			if pdi+mdi > 0 {
				dx = 100 * math.Abs(pdi-mdi) / (pdi + mdi)
			}
		}
		switch k := i - n + 1; {
		case k < n:
			dxSum += dx
		case k == n:
			adx = (dxSum + dx) / float64(n)
			out[i] = adx
		default:
			adx = (adx*float64(n-1) + dx) / float64(n)
			out[i] = adx
		}
	}
	return out
}
