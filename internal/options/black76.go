// Package options prices European index options with Black-76.
package options

import (
	"errors"
	"math"
	"time"

	"serene/internal/market"
)

// Input describes an option in floats, in rupees and years.
type Input struct {
	Forward, Strike float64
	Years           float64 // time to expiry
	Rate            float64 // continuously compounded
	Call            bool
}

// Greeks are per unit of underlying. Vega is per 1.00 of volatility and Theta
// is per year.
type Greeks struct {
	Delta, Gamma, Vega, Theta float64
}

func normCDF(x float64) float64 { return 0.5 * math.Erfc(-x/math.Sqrt2) }
func normPDF(x float64) float64 { return math.Exp(-x*x/2) / math.Sqrt(2*math.Pi) }

func (in Input) intrinsic() float64 {
	if in.Call {
		return math.Max(in.Forward-in.Strike, 0)
	}
	return math.Max(in.Strike-in.Forward, 0)
}

func d1d2(in Input, vol float64) (float64, float64) {
	sd := vol * math.Sqrt(in.Years)
	d1 := (math.Log(in.Forward/in.Strike) + 0.5*sd*sd) / sd
	return d1, d1 - sd
}

// Price returns the option's Black-76 value.
func Price(in Input, vol float64) float64 {
	df := math.Exp(-in.Rate * in.Years)
	if in.Years <= 0 || vol <= 0 {
		return df * in.intrinsic()
	}
	d1, d2 := d1d2(in, vol)
	if in.Call {
		return df * (in.Forward*normCDF(d1) - in.Strike*normCDF(d2))
	}
	return df * (in.Strike*normCDF(-d2) - in.Forward*normCDF(-d1))
}

// Sensitivities returns the option's Greeks.
func Sensitivities(in Input, vol float64) Greeks {
	if in.Years <= 0 || vol <= 0 {
		var g Greeks
		if in.intrinsic() > 0 {
			g.Delta = 1
			if !in.Call {
				g.Delta = -1
			}
		}
		return g
	}
	df := math.Exp(-in.Rate * in.Years)
	d1, _ := d1d2(in, vol)
	sq := math.Sqrt(in.Years)
	g := Greeks{
		Gamma: df * normPDF(d1) / (in.Forward * vol * sq),
		Vega:  df * in.Forward * normPDF(d1) * sq,
	}
	if in.Call {
		g.Delta = df * normCDF(d1)
	} else {
		g.Delta = -df * normCDF(-d1)
	}
	g.Theta = -df*in.Forward*normPDF(d1)*vol/(2*sq) + in.Rate*Price(in, vol)
	return g
}

// ErrNoVol means no volatility reproduces the price, typically a stale quote
// below intrinsic value.
var ErrNoVol = errors.New("no implied volatility for this price")

// ImpliedVol solves for volatility by bisection, which stays stable near
// expiry where Newton steps can diverge.
func ImpliedVol(in Input, price float64) (float64, error) {
	lo, hi := 1e-4, 5.0
	if price < Price(in, lo)-1e-6 || price > Price(in, hi)+1e-6 || in.Years <= 0 {
		return 0, ErrNoVol
	}
	for range 200 {
		mid := (lo + hi) / 2
		if Price(in, mid) < price {
			lo = mid
		} else {
			hi = mid
		}
		if hi-lo < 1e-7 {
			break
		}
	}
	return (lo + hi) / 2, nil
}

// YearsToExpiry measures calendar time to the 15:30 IST close on expiry.
func YearsToExpiry(now time.Time, expiry market.Date) float64 {
	return expiry.At(15, 30).Sub(now).Hours() / (365 * 24)
}
