package etf

import (
	"math"
	"slices"
	"testing"
	"time"

	"serene/internal/market"
)

func day(s string) time.Time {
	t, err := time.ParseInLocation(time.DateOnly, s, market.IST)
	if err != nil {
		panic(err)
	}
	return t
}

func TestBuildUniverseFoldsRenames(t *testing.T) {
	ls := []listing{
		// SBI gold: SBIGETS, then SETFGOLD, a 1:100 split onto a new ISIN, then SBIGOLD.
		{"SBIGETS", "INF200K01099", day("2011-06-22"), day("2015-11-23")},
		{"SETFGOLD", "INF200K01099", day("2015-11-24"), day("2022-01-06")},
		{"SETFGOLD", "INF200KA16D8", day("2022-01-07"), day("2026-10-29")},
		{"SBIGOLD", "INF200KA16D8", day("2026-10-30"), day("2026-11-06")},
		// A live ETF that once used the same ISIN must not be folded in.
		{"OTHER", "INF000000001", day("2020-01-01"), day("2026-11-06")},
		{"OLDNAME", "INF000000001", day("2018-01-01"), day("2019-12-31")},
		// Delisted.
		{"GONE", "INF000000002", day("2015-01-01"), day("2020-01-01")},
		// An alias with no ISIN link: N100 relisted as MON100 on a new ISIN.
		{"N100", "INF247L01031", day("2011-06-22"), day("2021-06-15")},
		{"MON100", "INF247L01AP3", day("2021-06-16"), day("2026-11-06")},
	}
	got := buildUniverse(ls, day("2026-10-01"))
	var syms []string
	for _, e := range got {
		syms = append(syms, e.Symbol)
	}
	if want := []string{"MON100", "OTHER", "SBIGOLD"}; !slices.Equal(syms, want) {
		t.Fatalf("universe %v, want %v", syms, want)
	}
	mon, other, gold := got[0], got[1], got[2]
	if mon.Country != "us" || !mon.First.Equal(day("2011-06-22")) || mon.ISIN() != "INF247L01AP3" {
		t.Errorf("MON100 = %+v", mon)
	}
	if !slices.Equal(other.Symbols, []string{"OTHER", "OLDNAME"}) {
		t.Errorf("OTHER symbols %v", other.Symbols)
	}
	if !gold.First.Equal(day("2011-06-22")) || !slices.Equal(gold.Symbols, []string{"SBIGOLD", "SBIGETS", "SETFGOLD"}) ||
		!slices.Equal(gold.ISINs, []string{"INF200K01099", "INF200KA16D8"}) {
		t.Errorf("SBIGOLD = %+v", gold)
	}
}

func TestBuildUniverseKeepsLiveNeighboursApart(t *testing.T) {
	// Two live ETFs share an old symbol's ISIN chain only through each other.
	ls := []listing{
		{"A", "INF1", day("2020-01-01"), day("2026-11-06")},
		{"B", "INF2", day("2020-01-01"), day("2026-11-06")},
		{"A", "INF2", day("2019-01-01"), day("2019-06-01")}, // A once traded on B's ISIN
	}
	got := buildUniverse(ls, day("2026-10-01"))
	if len(got) != 2 {
		t.Fatalf("got %d ETFs", len(got))
	}
	for _, e := range got {
		if slices.Contains(e.Symbols[1:], "A") || slices.Contains(e.Symbols[1:], "B") {
			t.Errorf("%s folded in a live ETF: %v", e.Symbol, e.Symbols)
		}
	}
}

func bar(d string, c float64) Bar { return Bar{day(d), c, c, c, c, 100, "bhav"} }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestAdjustSplitsMarkedByPrevClose(t *testing.T) {
	// 1:100 on day 2, where NSE's prev_close (42.85) is off the true 42.60.
	bars := []Bar{bar("2022-01-04", 4254.5), bar("2022-01-05", 4259.95), bar("2022-01-06", 42.35)}
	adjustSplits(bars, []float64{4275.55, 4254.5, 42.85})
	if !near(bars[0].Close, 42.545) || !near(bars[1].Close, 42.5995) || bars[2].Close != 42.35 {
		t.Errorf("closes %v %v %v", bars[0].Close, bars[1].Close, bars[2].Close)
	}
	if bars[0].Source != "bhav_adj" || bars[0].Volume != 10000 || bars[2].Source != "bhav" {
		t.Errorf("sources %s %s, volume %d", bars[0].Source, bars[2].Source, bars[0].Volume)
	}
}

func TestAdjustSplitsOnNewISIN(t *testing.T) {
	// prev_close still at the old basis: only the fall itself shows the split.
	bars := []Bar{bar("2021-02-16", 370), bar("2021-02-17", 372.44), bar("2021-02-18", 39.47)}
	adjustSplits(bars, []float64{369, 370, 372.44})
	if !near(bars[1].Close, 37.244) {
		t.Errorf("close before the split %v, want 37.244", bars[1].Close)
	}
}

func TestAdjustSplitsLeavesCrashes(t *testing.T) {
	// A 21% fall is a market, not a split.
	bars := []Bar{bar("2020-03-12", 28.95), bar("2020-03-13", 22.95)}
	adjustSplits(bars, []float64{29, 28.95})
	if bars[0].Close != 28.95 {
		t.Errorf("crash adjusted to %v", bars[0].Close)
	}
}

func TestFillScalesAnUnseenSplit(t *testing.T) {
	kite := []Bar{{day("2021-06-16"), 28.5, 28.5, 28.5, 28.5, 1, "kite"}}
	bh := []Bar{bar("2021-06-15", 286.9), bar("2021-06-16", 285.0)}
	out, n := fill(kite, bh)
	if n != 1 || len(out) != 2 || !near(out[1].Close, 28.69) || out[1].Source != "bhav_adj" {
		t.Errorf("fill = %d %+v", n, out)
	}
}

func TestMergeKeepsLaterAndDropsZero(t *testing.T) {
	bs := []Bar{bar("2024-01-02", 10), bar("2024-01-01", 0), bar("2024-01-02", 11)}
	out, dropped := merge(bs)
	if len(out) != 1 || out[0].Close != 11 || dropped != 1 {
		t.Errorf("merge = %+v, dropped %d", out, dropped)
	}
}

func TestMonthsQueueSharesFromBothEnds(t *testing.T) {
	e := ETF{First: day("2010-01-01")}
	q := newMonths(e, time.Time{}, day("2022-04-15"))
	a, _ := q.oldest()
	b, _ := q.newest()
	c, _ := q.oldest()
	d, _ := q.newest()
	if _, ok := q.oldest(); ok {
		t.Fatal("queue not empty after four months")
	}
	if !a.Equal(day("2022-01-01")) || !b.Equal(day("2022-04-01")) || !c.Equal(day("2022-02-01")) || !d.Equal(day("2022-03-01")) {
		t.Errorf("order %v %v %v %v", a, b, c, d)
	}
}
