package main

import (
	"math"
	"testing"

	"serene/internal/market"
)

func pdlDays(ohlc ...[4]float64) []pdlDay {
	var out []pdlDay
	d := market.Date{Year: 2025, Month: 1, Day: 1}
	for i, b := range ohlc {
		out = append(out, pdlDay{D: d.AddDays(i), O: b[0], H: b[1], L: b[2], C: b[3]})
	}
	return out
}

func flatEMA(n int, v float64) []float64 {
	e := make([]float64, n)
	for i := range e {
		e[i] = v
	}
	return e
}

func TestPDLSetup(t *testing.T) {
	base := [][4]float64{{100, 104, 96, 100}, {100, 104, 96, 100}, {100, 104, 96, 100}}
	mk := func(y [4]float64) []pdlDay { return pdlDays(append(append([][4]float64{}, base...), y)...) }
	ema := flatEMA(4, 105)
	// A small green day below the EMA sets up.
	if !pdlSetup(mk([4]float64{99, 103, 97, 99.5}), ema, 3) {
		t.Fatal("small green day should set up")
	}
	// A green body of 1% or more does not, even though it is inside.
	if pdlSetup(mk([4]float64{99, 103.5, 98, 100.5}), ema, 3) {
		t.Fatal("1% green day should be skipped")
	}
	// A wide red day is neither narrow nor inside.
	if pdlSetup(mk([4]float64{100, 105, 94, 95}), ema, 3) {
		t.Fatal("wide red day should not set up")
	}
	// A narrow red day sets up.
	if !pdlSetup(mk([4]float64{100, 101, 98, 99}), ema, 3) {
		t.Fatal("narrow red day should set up")
	}
	// Above the 20 EMA: no setup.
	if pdlSetup(mk([4]float64{99, 103, 97, 99.5}), flatEMA(4, 99), 3) {
		t.Fatal("above the EMA should be filtered")
	}
	if pdlSetup(mk([4]float64{99, 103, 97, 99.5}), flatEMA(4, math.NaN()), 3) {
		t.Fatal("no EMA yet should not set up")
	}
}

func TestPDLPivot(t *testing.T) {
	lows := []float64{100, 99, 95, 98, 99, 97, 90, 96, 97, 98, 99}
	var o [][4]float64
	for _, l := range lows {
		o = append(o, [4]float64{l + 2, l + 3, l, l + 1})
	}
	days := pdlDays(o...)
	// Day 6 (low 90) is confirmed by the close of day 8.
	if p := pdlPivot(days, 8, 100); p != 90 {
		t.Fatalf("pivot = %v, want 90", p)
	}
	// Not yet confirmed at day 7: falls back to day 2 (95).
	if p := pdlPivot(days, 7, 100); p != 95 {
		t.Fatalf("pivot = %v, want 95", p)
	}
	// Below the level only.
	if p := pdlPivot(days, 8, 92); p != 90 {
		t.Fatalf("pivot = %v, want 90", p)
	}
	if p := pdlPivot(days, 8, 89); p != 0 {
		t.Fatalf("pivot = %v, want 0", p)
	}
}

func TestPDLIndexTrades(t *testing.T) {
	// Days 0-3 flat, day 3 small green; day 4 gaps below day 3's low and falls.
	days := pdlDays(
		[4]float64{100, 104, 96, 100}, [4]float64{100, 104, 96, 100}, [4]float64{100, 104, 96, 100},
		[4]float64{99, 101, 98, 99.5},
		[4]float64{97, 97.5, 95, 95.5}, [4]float64{95, 96, 93, 94}, [4]float64{94, 95, 92, 93}, [4]float64{93, 94, 91, 92},
	)
	tr := pdlIndexTrades(days, flatEMA(len(days), 105), days[0].D, days[len(days)-1].D, 3)
	if len(tr) != 3 {
		t.Fatalf("trades: %+v", tr)
	}
	// Entry at the open 97 (below the previous low 98).
	if tr[0].exit != "day" || tr[0].pts != 97-95.5 {
		t.Fatalf("day: %+v", tr[0])
	}
	if tr[1].exit != "3d" || tr[1].pts != 97-92 {
		t.Fatalf("3d: %+v", tr[1])
	}
	// The only pivot below 97 would be the flat days' 96, which is not a fractal: time exit.
	if tr[2].why != "time" || tr[2].pts != 97-92 {
		t.Fatalf("3d_pivot: %+v", tr[2])
	}
}
