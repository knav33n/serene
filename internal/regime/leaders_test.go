package regime

import (
	"math"
	"testing"
)

// peakThenFall rises 1% a day to day peak, then falls 0.5% a day.
func peakThenFall(start float64, peak int) func(int) float64 {
	return func(i int) float64 {
		if i <= peak {
			return start * math.Pow(1.01, float64(i))
		}
		return start * math.Pow(1.01, float64(peak)) * math.Pow(0.995, float64(i-peak))
	}
}

func TestWindowForUsesAnOldPeak(t *testing.T) {
	b := make([]float64, 400)
	for i := range b {
		b[i] = peakThenFall(100, 300)(i)
	}
	w := WindowFor(b, 0, 399)
	if !w.SincePeak || w.From != 300 || w.To != 399 {
		t.Errorf("%+v", w)
	}
}

func TestWindowForFallsBackAtHighs(t *testing.T) {
	b := make([]float64, 400)
	for i := range b {
		b[i] = peakThenFall(100, 390)(i)
	}
	w := WindowFor(b, 0, 399)
	if w.SincePeak || w.From != 399-leaderFallback {
		t.Errorf("%+v", w)
	}
	// A short history falls back to its first day.
	w = WindowFor(b[:50], 0, 49)
	if w.SincePeak || w.From != 0 {
		t.Errorf("%+v", w)
	}
}

func TestLeadersFlagsStrengthDuringAFall(t *testing.T) {
	n := 400
	p := build(n, map[string]func(int) float64{
		LeaderBench: peakThenFall(1000, 100), // falls ~78% after day 100
		Market:      peakThenFall(1000, 100),
		"Strong":    grow(100, 0.1),         // keeps making highs
		"Weak":      peakThenFall(100, 100), // falls with the market
		"Held":      peakThenFall(100, 395), // rose through the fall, just off its high
		"Broad":     grow(100, 0.2),         // not a screened group
	}, map[string]Meta{"Strong": {Group: "sectoral"}, "Weak": {Group: "sectoral"},
		"Held": {Group: "thematic"}, "Broad": {Group: "broad"}})
	i := len(p.Dates) - 1
	rep, ok := Leaders(p, i, LeaderBench, "sectoral", "thematic")
	if !ok {
		t.Fatal("no report")
	}
	if !rep.Window.SincePeak || p.Dates[rep.Window.From] != p.Dates[100] {
		t.Errorf("window %+v", rep.Window)
	}
	if len(rep.Bench) != 2 || rep.Bench[0].Name != LeaderBench || math.Abs(rep.Bench[0].Excess) > 1e-9 {
		t.Errorf("bench %+v", rep.Bench)
	}
	got := map[string]LeaderRow{}
	for _, r := range rep.Sectors {
		got[r.Name] = r
	}
	if len(got) != 3 {
		t.Fatalf("sectors %v", rep.Sectors)
	}
	if s := got["Strong"]; !s.Leader || !s.NewHigh || s.FromATH != 0 || !s.RSHigh || s.Excess <= 0 {
		t.Errorf("strong %+v", s)
	}
	if w := got["Weak"]; w.Leader || math.Abs(w.Excess) > 1e-3 || w.FromATH > -70 {
		t.Errorf("weak %+v", w)
	}
	if h := got["Held"]; !h.Leader || h.NewHigh != (h.ATHAge < newHighDays) || h.ATHAge != 4 {
		t.Errorf("held %+v", h)
	}
	if rep.Sectors[len(rep.Sectors)-1].Name != "Weak" {
		t.Errorf("order %v", rep.Sectors)
	}
}

func TestLeaderStatsShortHistory(t *testing.T) {
	c := []float64{10, 11, 12, 11}
	r := LeaderStats("x", c, c, 0, 3, LeaderWindow{From: 0, To: 3})
	if !math.IsNaN(r.From1Y) || !math.IsNaN(r.R3M) || r.RSHigh || r.ATHAge != 1 {
		t.Errorf("%+v", r)
	}
	if math.Abs(r.FromATH-(11.0/12-1)*100) > 1e-9 || r.Leader {
		t.Errorf("%+v", r)
	}
}

func TestSortLeadersPutsNaNLast(t *testing.T) {
	rows := []LeaderRow{{Name: "a", Excess: math.NaN()}, {Name: "b", Excess: 1}, {Name: "c", Excess: 5}}
	SortLeaders(rows)
	if rows[0].Name != "c" || rows[1].Name != "b" || rows[2].Name != "a" {
		t.Errorf("%v", rows)
	}
}
