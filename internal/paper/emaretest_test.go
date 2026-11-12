package paper

import (
	"testing"
	"time"

	"serene/internal/indicator"
	"serene/internal/market"
)

var (
	prevDay = market.Date{Year: 2026, Month: time.November, Day: 12}
	day     = market.Date{Year: 2026, Month: time.November, Day: 13}
	ce      = market.Option(market.NFO, "NIFTY", market.Date{Year: 2026, Month: time.November, Day: 17}, market.Rupees(25900), market.Call, 65)
	pe      = market.Option(market.NFO, "NIFTY", market.Date{Year: 2026, Month: time.November, Day: 17}, market.Rupees(25900), market.Put, 65)
)

// bar is one 1-minute bar's open, low and close in rupees; the high is above
// both open and close.
type bar struct{ o, l, c float64 }

func mk(d market.Date, hh, mm int, bs []bar) []market.Bar {
	t0 := d.At(hh, mm)
	out := make([]market.Bar, len(bs))
	for i, b := range bs {
		t := t0.Add(time.Duration(i) * time.Minute)
		out[i] = market.Bar{Timeframe: market.Minute1, Time: t, End: t.Add(time.Minute),
			Open: market.Rupees(b.o), Low: market.Rupees(b.l), Close: market.Rupees(b.c),
			High: market.Rupees(max(b.o, b.c) + 1)}
	}
	return out
}

// flat is n bars at p, each with a low a rupee under it, so none is a swing low.
func flat(p float64, n int) []bar {
	out := make([]bar, n)
	for i := range out {
		out[i] = bar{p, p - 1, p}
	}
	return out
}

// legBars warms on 40 flat bars at 100 the day before, then today's bars.
func legBars(today []bar) []market.Bar {
	return append(mk(prevDay, 14, 50, flat(100, 40)), mk(day, 9, 15, today)...)
}

// dip then rally: a swing low of 95 at 09:17, a cross up on the rally, then a
// fall back through the slow EMA.
func rally() []bar {
	return []bar{
		{100, 99, 99}, {99, 97, 97}, {97, 95, 96}, {96, 96, 97}, {97, 96.5, 98},
		{98, 97.5, 104}, {104, 103, 108}, {108, 107, 112}, {112, 111, 114}, {114, 113, 115},
	}
}

func run(t *testing.T, cfg Config, legs ...Leg) *Trade {
	t.Helper()
	tr, err := EMARetest(legs, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// firstCross is the index of the first bar of today whose close crosses up.
func firstCross(bars []market.Bar) int {
	ef, es := indicator.EMA(bars, 13), indicator.EMA(bars, 34)
	for i := 40; i < len(bars); i++ {
		if ef[i] > es[i] && !(ef[i-1] > es[i-1]) {
			return i
		}
	}
	return -1
}

func TestFillsOnRetestOfTheSlowEMA(t *testing.T) {
	bs := legBars(append(rally(), bar{114, 101, 102}, bar{102, 101, 103}))
	i := firstCross(bs)
	if i < 0 || i >= len(bs)-2 {
		t.Fatalf("cross at %d", i)
	}
	tr := run(t, DefaultEMARetest(day), Leg{ce, bs})
	if tr == nil {
		t.Fatal("no trade")
	}
	es := indicator.EMA(bs, 34)
	fill := len(bs) - 2
	want := floorTick(market.Rupees(es[fill-1]), 5)
	if !tr.EntryTime.Equal(bs[fill].Time) || tr.Entry != want {
		t.Fatalf("entry %s @ %s, want %s @ %s", tr.EntryTime, tr.Entry, bs[fill].Time, want)
	}
	if tr.Stop != market.Rupees(95) {
		t.Errorf("stop %s", tr.Stop)
	}
	if want := int64(market.Rupees(100000)) / (int64(tr.Entry) * 65); tr.Lots != want || tr.Qty != want*65 {
		t.Errorf("lots %d qty %d", tr.Lots, tr.Qty)
	}
	if !tr.Open {
		t.Errorf("exited: %+v", tr)
	}
}

func TestGapThroughTheLimitFillsAtTheOpen(t *testing.T) {
	bs := legBars(append(rally(), bar{99, 98, 99}))
	tr := run(t, DefaultEMARetest(day), Leg{ce, bs})
	if tr == nil || tr.Entry != market.Rupees(99) {
		t.Fatalf("trade %+v", tr)
	}
}

func TestNoFillWithoutARetest(t *testing.T) {
	bs := legBars(append(rally(), bar{115, 114, 116}, bar{116, 115, 117}))
	if tr := run(t, DefaultEMARetest(day), Leg{ce, bs}); tr != nil {
		t.Fatalf("trade %+v", tr)
	}
}

func TestNoSwingLowBelowSkipsTheArm(t *testing.T) {
	// Straight up from the open: no swing low today.
	today := []bar{{100, 99, 104}, {104, 103, 108}, {108, 107, 112}, {112, 111, 114}, {114, 101, 102}}
	if tr := run(t, DefaultEMARetest(day), Leg{ce, legBars(today)}); tr != nil {
		t.Fatalf("trade %+v", tr)
	}
}

func TestStopAndTrail(t *testing.T) {
	// Fill, then a higher swing low of 100 at the 3rd bar after, then a fall.
	after := []bar{{102, 101, 106}, {106, 100.5, 103}, {103, 100, 104}, {104, 102, 108}, {108, 106, 109}, {109, 99, 99.5}}
	bs := legBars(append(append(rally(), bar{114, 101, 102}), after...))
	tr := run(t, DefaultEMARetest(day), Leg{ce, bs})
	if tr == nil || tr.Open {
		t.Fatalf("trade %+v", tr)
	}
	if tr.LastStop != market.Rupees(100) || tr.Reason != "trailed stop" || tr.Exit != market.Rupees(100) {
		t.Fatalf("exit %+v", tr)
	}
	if !tr.ExitTime.Equal(bs[len(bs)-1].Time) {
		t.Errorf("exit time %s", tr.ExitTime)
	}
}

func TestStopGapFillsAtTheOpenAndNotOnTheFillBar(t *testing.T) {
	// The fill bar's own low is under the stop: no exit on it. The next bar
	// opens under the stop.
	bs := legBars(append(rally(), bar{114, 94, 102}, bar{90, 89, 90}))
	tr := run(t, DefaultEMARetest(day), Leg{ce, bs})
	if tr == nil || tr.Open || tr.Reason != "stop" || tr.Exit != market.Rupees(90) {
		t.Fatalf("trade %+v", tr)
	}
}

func TestTimeExit(t *testing.T) {
	bs := legBars(append(rally(), bar{114, 101, 102}))
	pad := mk(day, 9, 15+len(bs)-40, flat(130, 400))
	bs = append(bs, pad...)
	tr := run(t, DefaultEMARetest(day), Leg{ce, bs})
	if tr == nil || tr.Open || tr.Reason != "15:15" || tr.Exit != market.Rupees(130) ||
		!tr.ExitTime.Equal(day.At(15, 15)) {
		t.Fatalf("trade %+v", tr)
	}
}

func TestFirstLegToFillIsTheTrade(t *testing.T) {
	early := legBars(append(rally(), bar{114, 101, 102}))
	late := legBars(append(rally(), bar{115, 114, 116}, bar{114, 101, 102}))
	tr := run(t, DefaultEMARetest(day), Leg{ce, late}, Leg{pe, early})
	if tr == nil || tr.Instrument.Right != market.Put {
		t.Fatalf("trade %+v", tr)
	}
}

func TestNoEntryFromThreePM(t *testing.T) {
	cfg := DefaultEMARetest(day)
	cfg.LastEntry = 10 * time.Minute // 09:25, the bar after the rally ends
	bs := legBars(append(rally(), bar{114, 101, 102}))
	if tr := run(t, cfg, Leg{ce, bs}); tr != nil {
		t.Fatalf("trade %+v", tr)
	}
}

func TestTooLittleCapitalSkips(t *testing.T) {
	cfg := DefaultEMARetest(day)
	cfg.Capital = market.Rupees(1000)
	if tr := run(t, cfg, Leg{ce, legBars(append(rally(), bar{114, 101, 102}))}); tr != nil {
		t.Fatalf("trade %+v", tr)
	}
}
