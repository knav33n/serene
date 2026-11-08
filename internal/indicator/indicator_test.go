package indicator

import (
	"math"
	"testing"
	"time"

	"calm/internal/market"
)

func bar(day, minute int, h, l, c float64, v int64) market.Bar {
	t := time.Date(2026, 11, day, 9, 15+minute, 0, 0, market.IST)
	return market.Bar{Time: t, End: t.Add(time.Minute), High: market.Rupees(h), Low: market.Rupees(l), Close: market.Rupees(c), Volume: v}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestEMA(t *testing.T) {
	bars := []market.Bar{bar(6, 0, 0, 0, 1, 0), bar(6, 1, 0, 0, 2, 0), bar(6, 2, 0, 0, 3, 0), bar(6, 3, 0, 0, 6, 0)}
	got := EMA(bars, 3)
	if !math.IsNaN(got[0]) || !math.IsNaN(got[1]) || !near(got[2], 2) || !near(got[3], 4) {
		t.Fatalf("ema %v", got)
	}
}

func TestVWAPRestartsEachDay(t *testing.T) {
	bars := []market.Bar{bar(5, 0, 12, 6, 9, 100), bar(6, 0, 3, 3, 3, 0), bar(6, 1, 12, 6, 9, 100), bar(6, 2, 6, 6, 6, 200)}
	got := VWAP(bars)
	if !near(got[0], 9) || !near(got[1], 3) || !near(got[2], 9) || !near(got[3], 7) {
		t.Fatalf("vwap %v", got)
	}
}

func TestATR(t *testing.T) {
	// True ranges: 2, then max(3-2, |3-1|, |2-1|) = 2, then 4 (gap up from 2.5).
	bars := []market.Bar{bar(6, 0, 2, 0, 1, 0), bar(6, 1, 3, 2, 2.5, 0), bar(6, 2, 6.5, 6, 6, 0)}
	got := ATR(bars, 2)
	if !math.IsNaN(got[0]) || !near(got[1], 2) || !near(got[2], 3) {
		t.Fatalf("atr %v", got)
	}
}

func TestADX(t *testing.T) {
	// A steady climb is all trend: every move is up, so ADX is 100.
	var up, chop []market.Bar
	for i := 0; i < 40; i++ {
		f := float64(i)
		up = append(up, bar(6, i, 101+f, 99+f, 100.5+f, 0))
		// Alternating up and down bars of the same size cancel out.
		s := 1.0
		if i%2 == 1 {
			s = -1
		}
		chop = append(chop, bar(6, i, 101+s, 99+s, 100+s, 0))
	}
	a := ADX(up, 5)
	if !math.IsNaN(a[8]) || math.IsNaN(a[9]) || !near(a[9], 100) || !near(a[39], 100) {
		t.Fatalf("rising: adx[8]=%v adx[9]=%v adx[39]=%v", a[8], a[9], a[39])
	}
	if c := ADX(chop, 5); c[39] > 20 {
		t.Errorf("choppy series has ADX %.1f", c[39])
	}
}
