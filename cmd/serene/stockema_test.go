package main

import (
	"math"
	"testing"

	"serene/internal/data"
	"serene/internal/market"
)

func n500Days(px ...[2]float64) []data.PanelDay {
	d, _ := market.ParseDate("2020-01-01")
	out := make([]data.PanelDay, len(px))
	for i, p := range px {
		out[i] = data.PanelDay{Symbol: "X", Date: d.AddDays(i), O: p[0], H: p[1], L: p[0], C: p[1]}
	}
	return out
}

func TestCrossRunFillsNextOpen(t *testing.T) {
	days := n500Days([2]float64{100, 100}, [2]float64{100, 110}, [2]float64{120, 121}, [2]float64{130, 130})
	// Long at day 0's close is filled at day 1's open; out at day 2's close, filled at day 3's open.
	fast := []float64{2, 2, 0, 0}
	slow := []float64{1, 1, 1, 1}
	st := crossRun(days, fast, slow, 1, 4, 0.01, false)
	want := (0.99*110/100.0)*(121/110.0)*(0.99*130/121.0) - 1
	if st.trades != 1 || math.Abs(st.total-want) > 1e-12 {
		t.Fatalf("trades %d total %v, want 1 and %v", st.trades, st.total, want)
	}
	bh := crossRun(days, nil, nil, 1, 4, 0.01, false)
	if w := 0.99*130/100.0*0.99 - 1; math.Abs(bh.total-w) > 1e-12 {
		t.Fatalf("buy and hold %v, want %v", bh.total, w)
	}
}

func TestJoinRenamesRefusesJump(t *testing.T) {
	a, b := n500Days([2]float64{100, 100}), n500Days([2]float64{0, 0}, [2]float64{101, 102})
	b = b[1:]
	got, joined, err := joinRenames([][]data.PanelDay{b, a})
	if err != nil || !joined || len(got) != 2 || got[0].C != 100 {
		t.Fatalf("join: %v %v %v", got, joined, err)
	}
	b[0].O = 150
	if _, _, err := joinRenames([][]data.PanelDay{a, b}); err == nil {
		t.Fatal("a 50% jump at the rename was joined")
	}
}

func TestCrossRunReversesToShort(t *testing.T) {
	days := n500Days([2]float64{100, 100}, [2]float64{100, 110}, [2]float64{120, 121}, [2]float64{130, 130})
	fast := []float64{2, 2, 0, 0}
	slow := []float64{1, 1, 1, 1}
	st := crossRun(days, fast, slow, 1, 4, 0.01, true)
	// Long from day 1's open, reversed at day 3's open, short closed at day 3's close.
	want := (0.99*110/100.0)*(121/110.0)*(130/121.0*0.99)*(0.99*(1-(130/130.0-1))*0.99) - 1
	if st.trades != 2 || math.Abs(st.total-want) > 1e-12 {
		t.Fatalf("trades %d total %v, want 2 and %v", st.trades, st.total, want)
	}
}
