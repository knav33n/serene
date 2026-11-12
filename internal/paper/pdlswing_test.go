package paper

import (
	"testing"
	"time"

	"serene/internal/market"
)

func pdlBars(d market.Date, hm [][2]int, ohlc [][4]float64) []market.Bar {
	var out []market.Bar
	for i, x := range hm {
		t := d.At(x[0], x[1])
		o := ohlc[i]
		out = append(out, market.Bar{Time: t, End: t.Add(time.Minute), Open: market.Rupees(o[0]),
			High: market.Rupees(o[1]), Low: market.Rupees(o[2]), Close: market.Rupees(o[3])})
	}
	return out
}

func TestPDLBreakFillExit(t *testing.T) {
	d := market.Date{Year: 2026, Month: 11, Day: 13}
	idx := pdlBars(d, [][2]int{{9, 15}, {9, 16}, {9, 17}, {14, 59}},
		[][4]float64{{101, 102, 100.5, 101}, {101, 101, 100, 100.2}, {100.2, 100.3, 99.5, 99.8}, {99, 99, 90, 91}})
	if i := PDLBreak(idx, d, 100); i != 2 {
		t.Fatalf("break at %d, want 2 (a low equal to the level is not a break)", i)
	}
	// The 14:59 bar ends at 15:00: too late.
	if i := PDLBreak(idx[3:], d, 95); i != -1 {
		t.Fatalf("late break at %d", i)
	}
	opt := pdlBars(d, [][2]int{{9, 17}, {9, 20}, {15, 14}, {15, 16}},
		[][4]float64{{300, 301, 299, 300}, {310, 312, 309, 311}, {350, 351, 349, 350}, {355, 356, 354, 355}})
	// The signal bar 9:17 ends 9:18; the next option trade is 9:20, within 3 minutes.
	if j := PDLFill(opt, d.At(9, 18)); j != 1 {
		t.Fatalf("fill at %d, want 1", j)
	}
	if j := PDLFill(opt, d.At(9, 16)); j != 0 {
		t.Fatalf("fill at %d, want 0", j)
	}
	if j := PDLFill(opt[2:], d.At(14, 0)); j != -1 {
		t.Fatalf("stale fill at %d", j)
	}
	// No bar at 15:15: the first one after it.
	if px, at, ok := PDLExit(opt, d, false); !ok || px != market.Rupees(355) || !at.Equal(d.At(15, 16)) {
		t.Fatalf("exit %v %v %v", px, at, ok)
	}
	if _, _, ok := PDLExit(opt[:3], d, false); ok {
		t.Fatal("no exit before a 15:15 bar unless final")
	}
	if px, _, ok := PDLExit(opt[:3], d, true); !ok || px != market.Rupees(350) {
		t.Fatalf("final exit %v %v", px, ok)
	}
}
