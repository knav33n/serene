package swing

import (
	"testing"
	"time"

	"serene/internal/market"
)

// series builds 1-minute bars from (high, low, close) rupee triples.
func series(hlc ...[3]float64) []market.Bar {
	t0 := time.Date(2026, 11, 6, 9, 15, 0, 0, market.IST)
	out := make([]market.Bar, len(hlc))
	for i, v := range hlc {
		t := t0.Add(time.Duration(i) * time.Minute)
		out[i] = market.Bar{Timeframe: market.Minute1, Time: t, End: t.Add(time.Minute),
			High: market.Rupees(v[0]), Low: market.Rupees(v[1]), Close: market.Rupees(v[2])}
		out[i].Open = out[i].Close
		if i > 0 {
			out[i].Open = out[i-1].Close
		}
	}
	return out
}

func TestPivots(t *testing.T) {
	bars := series([3]float64{10, 9, 9}, [3]float64{12, 10, 11}, [3]float64{11, 9, 10},
		[3]float64{10, 8, 9}, [3]float64{11, 9, 10}, [3]float64{10, 9, 9})
	p := Pivots(bars, 1)
	if len(p) != 2 || p[0].Index != 1 || p[1].Index != 4 {
		t.Fatalf("pivots %+v", p)
	}
	if !p[0].Confirmed.Equal(bars[2].End) {
		t.Errorf("confirmed %s", p[0].Confirmed)
	}
}

func TestEqualHighsAreNotPivots(t *testing.T) {
	bars := series([3]float64{10, 9, 9}, [3]float64{12, 10, 11}, [3]float64{12, 10, 11}, [3]float64{10, 9, 9})
	if p := Pivots(bars, 1); len(p) != 0 {
		t.Fatalf("pivots %+v", p)
	}
}

// The NIFTY 24500 CE's first 45 minutes on 2026-11-06: (high, low, close).
var ce20261106 = series(
	[3]float64{139.30, 93.50, 114.20},  // 09:15
	[3]float64{131.90, 101.85, 131.60}, // 09:16
	[3]float64{156.20, 127, 145.05},    // 09:17
	[3]float64{151.75, 129.10, 151.10}, // 09:18
	[3]float64{164.80, 147.15, 163},    // 09:19
	[3]float64{179.65, 158.55, 170.65}, // 09:20
	[3]float64{172, 155.20, 166.50},    // 09:21
	[3]float64{170.80, 157, 169.25},    // 09:22
	[3]float64{175.85, 158.05, 161.10}, // 09:23
	[3]float64{161.55, 145, 147.70},    // 09:24
	[3]float64{154, 138.15, 138.90},    // 09:25
	[3]float64{151.95, 134.35, 151.25}, // 09:26
	[3]float64{165, 143.90, 157.80},    // 09:27
	[3]float64{168.85, 151.95, 157.80}, // 09:28
	[3]float64{167.60, 153.15, 154.05}, // 09:29
	[3]float64{166.45, 151.40, 165.80}, // 09:30
	[3]float64{168, 160, 162.65},       // 09:31
	[3]float64{167.70, 157.20, 166.45}, // 09:32
	[3]float64{175, 165, 175},          // 09:33
	[3]float64{178, 168, 175.90},       // 09:34
	[3]float64{189, 172.45, 187.10},    // 09:35
	[3]float64{198, 182, 194.10},       // 09:36
	[3]float64{195.65, 170.75, 174.70}, // 09:37
	[3]float64{180.55, 164, 166.80},    // 09:38
	[3]float64{183.60, 166.70, 181.55}, // 09:39
	[3]float64{189, 178.30, 181.55},    // 09:40
	[3]float64{183.50, 173.95, 177.30}, // 09:41
	[3]float64{189, 176.75, 184.70},    // 09:42
	[3]float64{190, 179, 189.60},       // 09:43
	[3]float64{194, 186.55, 189.25},    // 09:44
	[3]float64{201.90, 182.05, 199.55}, // 09:45
	[3]float64{215.85, 196.55, 215.75}, // 09:46
	[3]float64{218.60, 204.05, 216.95}, // 09:47
	[3]float64{235.80, 216.30, 229.15}, // 09:48
	[3]float64{234, 218, 234},          // 09:49
	[3]float64{245, 231.95, 239.50},    // 09:50
	[3]float64{242.75, 233.80, 235},    // 09:51
	[3]float64{249.85, 227.10, 245.50}, // 09:52
	[3]float64{251.80, 243.15, 251.40}, // 09:53
	[3]float64{256.90, 240.30, 250.25}, // 09:54
	[3]float64{254, 238.65, 243.35},    // 09:55
	[3]float64{252, 234.50, 237},       // 09:56
	[3]float64{242.75, 223.85, 228.05}, // 09:57
	[3]float64{232.65, 219.70, 228.60}, // 09:58
	[3]float64{229.10, 221.50, 228.05}, // 09:59
)

func at(hhmm string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02 15:04", "2026-11-06 "+hhmm, market.IST)
	return t
}

func TestBreaksOnTheCE(t *testing.T) {
	got := Breaks(ce20261106, 2, 10, nil)
	if len(got) < 2 {
		t.Fatalf("breaks %+v", got)
	}
	f, s := got[0], got[1]
	if f.Kind != First || !f.Bar.Time.Equal(at("09:33")) || f.Level.High != market.Rupees(168.85) {
		t.Errorf("first: %s bar %s level %s", f.Kind, f.Bar.Time.Format("15:04"), f.Level.High)
	}
	if s.Kind != Second || !s.Bar.Time.Equal(at("09:45")) || s.Level.High != market.Rupees(198) || s.Pullback != market.Rupees(164) {
		t.Errorf("second: %s bar %s level %s pullback %s", s.Kind, s.Bar.Time.Format("15:04"), s.Level.High, s.Pullback)
	}
}

func TestNoLookahead(t *testing.T) {
	for k := 1; k <= 5; k++ {
		for _, b := range Breaks(ce20261106, k, 0, nil) {
			if !b.Bar.Time.After(b.Level.Confirmed.Add(-time.Second)) {
				t.Errorf("k=%d: %s break at %s uses a pivot known only at %s", k, b.Kind, b.Bar.Time.Format("15:04"), b.Level.Confirmed.Format("15:04"))
			}
		}
	}
}

func TestShallowPullbackIsNotSignificant(t *testing.T) {
	// The 9:33 break came after a 10.3% pullback below 168.85.
	for _, b := range Breaks(ce20261106, 2, 11, nil) {
		if b.Bar.Time.Equal(at("09:33")) {
			t.Fatal("broke on a 10.3% pullback with an 11% minimum")
		}
	}
}

func TestFailedRiseResets(t *testing.T) {
	// Lower high 80 breaks at bar 7 (First), target 90 at bar 8; then the price
	// falls through the First pullback low of 60, so 90's later break is not a
	// Second.
	bars := series(
		[3]float64{90, 85, 88}, [3]float64{100, 90, 95}, [3]float64{92, 75, 78},
		[3]float64{78, 70, 72}, [3]float64{80, 72, 76}, [3]float64{74, 60, 62},
		[3]float64{70, 61, 68}, [3]float64{85, 68, 84}, [3]float64{90, 80, 85},
		[3]float64{84, 70, 72}, [3]float64{72, 50, 52}, [3]float64{95, 52, 94})
	got := Breaks(bars, 1, 10, nil)
	if len(got) != 1 || got[0].Kind != First || got[0].Bar.Time != bars[7].Time {
		t.Fatalf("breaks %+v", got)
	}
}

func TestEntryFilterDelaysTheSecondBreak(t *testing.T) {
	// Refuse the 9:45 bar: the 9:46 bar also closes above 198, so it enters.
	refused := -1
	for i, b := range ce20261106 {
		if b.Time.Equal(at("09:45")) {
			refused = i
		}
	}
	got := Breaks(ce20261106, 2, 10, func(i int) bool { return i != refused })
	if len(got) < 2 || got[1].Kind != Second || !got[1].Bar.Time.Equal(at("09:46")) {
		t.Fatalf("breaks %+v", got)
	}
	if len(got[1].Blocked) != 1 || !got[1].Blocked[0].Time.Equal(at("09:45")) {
		t.Errorf("blocked %+v", got[1].Blocked)
	}
	if !got[0].Bar.Time.Equal(at("09:33")) {
		t.Errorf("the filter moved the first break to %s", got[0].Bar.Time.Format("15:04"))
	}
}

func TestTradeExitsBelowTheHigherLow(t *testing.T) {
	got := Trades(ce20261106, 2, 10, nil, Exit{TrailK: 1})
	if len(got) != 1 {
		t.Fatalf("trades %+v", got)
	}
	tr := got[0]
	if !tr.Entry.Bar.Time.Equal(at("09:45")) || tr.EntryPrice != market.Rupees(199.55) {
		t.Errorf("entry %s at %s", tr.Entry.Bar.Time.Format("15:04"), tr.EntryPrice)
	}
	if tr.StopFrom == nil || !tr.StopFrom.Time.Equal(at("09:51")) || tr.Stop != market.Rupees(235) {
		t.Errorf("stop %s from %+v", tr.Stop, tr.StopFrom)
	}
	if tr.Reason != Stopped || !tr.Exit.Time.Equal(at("09:56")) || tr.ExitPrice != market.Rupees(235) {
		t.Errorf("exit %s at %s, reason %s", tr.Exit.Time.Format("15:04"), tr.ExitPrice, tr.Reason)
	}
}

func TestStopFillsAtAGapOpen(t *testing.T) {
	bars := append([]market.Bar(nil), ce20261106...)
	for i := range bars {
		if bars[i].Time.Equal(at("09:56")) {
			bars[i].Open, bars[i].Low = market.Rupees(230), market.Rupees(229)
		}
	}
	tr := Trades(bars, 2, 10, nil, Exit{TrailK: 1})[0]
	if tr.ExitPrice != market.Rupees(230) {
		t.Errorf("exit at %s, want the 230 open", tr.ExitPrice)
	}
}

func TestInitialStopIsThePullbackLow(t *testing.T) {
	bars := append([]market.Bar(nil), ce20261106...)
	for i := range bars {
		if bars[i].Time.Equal(at("09:46")) {
			bars[i].Open, bars[i].Low, bars[i].Close = market.Rupees(199), market.Rupees(160), market.Rupees(170)
		}
	}
	tr := Trades(bars, 2, 10, nil, Exit{TrailK: 1})[0]
	if !tr.Exit.Time.Equal(at("09:46")) || tr.StopFrom != nil || tr.ExitPrice != market.Rupees(164) {
		t.Errorf("exit %s at %s from %+v", tr.Exit.Time.Format("15:04"), tr.ExitPrice, tr.StopFrom)
	}
}

func TestExcursions(t *testing.T) {
	tr := Trades(ce20261106, 2, 10, nil, Exit{TrailK: 1})[0]
	// From 9:46 to 9:56 the low was 196.55 at 9:46 and the high 256.90 at 9:54.
	if tr.Worst != market.Rupees(196.55) || tr.Best != market.Rupees(256.90) {
		t.Errorf("worst %s best %s", tr.Worst, tr.Best)
	}
	if mae := tr.MAEPct(); mae < 1.49 || mae > 1.51 {
		t.Errorf("mae %.3f", mae)
	}
}

func TestFixedStopAndTimeExit(t *testing.T) {
	pct20 := func(e Break) market.Paise { return e.Bar.Close * 80 / 100 }
	tr := Trades(ce20261106, 2, 10, nil, Exit{Initial: pct20, FlatAt: 9*time.Hour + 50*time.Minute})[0]
	if tr.Initial != market.Rupees(159.64) || tr.Reason != Flat || !tr.Exit.Time.Equal(at("09:50")) || tr.ExitPrice != market.Rupees(239.50) {
		t.Errorf("initial %s, %s exit %s at %s", tr.Initial, tr.Reason, tr.Exit.Time.Format("15:04"), tr.ExitPrice)
	}
	if got := Trades(ce20261106, 2, 10, nil, Exit{FlatAt: 9*time.Hour + 40*time.Minute}); len(got) != 0 {
		t.Errorf("entered after the time exit: %+v", got)
	}
}

func TestTargetAndMaxHold(t *testing.T) {
	// Entry 199.55 at 9:45. A 10% target is 219.51: the 9:48 bar's high of
	// 235.80 reaches it after it opens at 216.95, below the target.
	tr := Trades(ce20261106, 2, 10, nil, Exit{TrailK: 1, TargetPct: 10})[0]
	if tr.Reason != Target || !tr.Exit.Time.Equal(at("09:48")) || tr.ExitPrice != market.Rupees(219.51) {
		t.Errorf("%s exit %s at %s", tr.Reason, tr.Exit.Time.Format("15:04"), tr.ExitPrice)
	}
	if h := tr.Hold(); h != 2*time.Minute {
		t.Errorf("hold %s", h)
	}
	// Held for 3 minutes: the 9:48 bar ends 3 minutes after the 9:46 entry close.
	tr = Trades(ce20261106, 2, 10, nil, Exit{TrailK: 1, MaxHold: 3 * time.Minute})[0]
	if tr.Reason != Held || !tr.Exit.Time.Equal(at("09:48")) || tr.ExitPrice != market.Rupees(229.15) {
		t.Errorf("%s exit %s at %s", tr.Reason, tr.Exit.Time.Format("15:04"), tr.ExitPrice)
	}
}

func TestEntryFrom(t *testing.T) {
	if got := Trades(ce20261106, 2, 10, nil, Exit{TrailK: 1, EntryFrom: 9*time.Hour + 46*time.Minute}); len(got) != 0 {
		t.Errorf("entered before the start time: %+v", got)
	}
	if got := Trades(ce20261106, 2, 10, nil, Exit{TrailK: 1, EntryFrom: 9*time.Hour + 45*time.Minute}); len(got) != 1 {
		t.Errorf("trades %+v", got)
	}
}

func TestPivotLows(t *testing.T) {
	bars := series([3]float64{10, 9, 9}, [3]float64{10, 7, 8}, [3]float64{11, 8, 10},
		[3]float64{10, 8, 9}, [3]float64{11, 9, 10}, [3]float64{10, 9, 9})
	p := PivotLows(bars, 1)
	// Bar 3's low equals bar 2's, so it is not a pivot.
	if len(p) != 1 || p[0].Index != 1 || p[0].Low != market.Rupees(7) || !p[0].Confirmed.Equal(bars[2].End) {
		t.Fatalf("pivot lows %+v", p)
	}
}
