package mcx

import (
	"testing"
	"time"

	"serene/internal/cost"
	"serene/internal/market"
)

// path makes one-minute bars from closes, each with a range of exactly ₹1,
// so the median range is 1 and the reversal is Multiple rupees.
func path(key string, d market.Date, vol int64, closes ...float64) []market.Bar {
	var out []market.Bar
	t := d.At(10, 0)
	for _, c := range closes {
		out = append(out, market.Bar{Key: key, Time: t, End: t.Add(time.Minute), Open: market.Rupees(c),
			High: market.Rupees(c + 0.5), Low: market.Rupees(c - 0.5), Close: market.Rupees(c), Volume: vol})
		t = t.Add(time.Minute)
	}
	return out
}

func mirror(closes []float64) []float64 {
	out := make([]float64, len(closes))
	for i, c := range closes {
		out[i] = 200 - c
	}
	return out
}

var (
	day1 = market.Date{Year: 2026, Month: 9, Day: 1}
	day2 = market.Date{Year: 2026, Month: 9, Day: 2}
	day3 = market.Date{Year: 2026, Month: 9, Day: 3}
	keyA = "MCX:CRUDEOIL:2026-09-18:FUT"
	keyB = "MCX:CRUDEOIL:2026-10-19:FUT"
)

func testConfig() BacktestConfig {
	return BacktestConfig{Name: "CRUDEOIL", Spec: Spec{Multiplier: 10, Tick: 5}, Frame: time.Minute,
		Lookback: 30 * 24 * time.Hour, Multiple: 3, Costs: cost.Zero(), From: day1, To: day3}
}

// Up to a swing high of 105.5 and a higher low of 100.5; 106 breaks the high.
var setup = []float64{100, 101, 102, 103, 104, 105, 104, 103, 102, 101, 102, 103, 104, 105, 106}

// Retest: 105 trades through 105.5 and fills at its open; 107 confirms the
// 104.5 low, trailing the stop to 104.45; 104 takes it out.
var retest = []float64{107, 106, 105, 106, 107, 108, 109, 110, 109, 108, 107, 106, 105, 104}

func run(t *testing.T, cfg BacktestConfig, bars ...[]market.Bar) ([]Trade, Orders) {
	t.Helper()
	var all []market.Bar
	for _, b := range bars {
		all = append(all, b...)
	}
	trades, stats, err := Backtest(all, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return trades, stats
}

func TestBacktestLongRetestAndTrail(t *testing.T) {
	trades, stats := run(t, testConfig(), path(keyA, day1, 1, 100), path(keyA, day2, 1, append(setup, retest...)...))
	if len(trades) != 1 {
		t.Fatalf("trades %v", trades)
	}
	tr := trades[0]
	if tr.Side != market.Buy || tr.Kind != "NEW" || tr.Level != market.Rupees(105.5) || tr.Stop != market.Rupees(100.45) {
		t.Errorf("order %+v", tr)
	}
	if tr.In != market.Rupees(105) || tr.Entry != day2.At(10, 17) {
		t.Errorf("fill at %s %s, want 105 at 10:17 (the open, below the limit)", tr.In, tr.Entry)
	}
	if tr.Out != market.Rupees(104) || tr.Reason != "stop" || tr.Exit != day2.At(10, 28) {
		t.Errorf("exit %s %s at %s, want 104 stop at 10:28 (trailed to 104.45, gapped at the open)", tr.Out, tr.Reason, tr.Exit)
	}
	if tr.Gross != market.Rupees(-10) {
		t.Errorf("gross %s", tr.Gross)
	}
	if stats.Filled != 1 {
		t.Errorf("stats %+v", stats)
	}
}

func TestBacktestShortMirror(t *testing.T) {
	trades, _ := run(t, testConfig(), path(keyA, day1, 1, 100), path(keyA, day2, 1, mirror(append(setup, retest...))...))
	if len(trades) != 1 {
		t.Fatalf("trades %v", trades)
	}
	tr := trades[0]
	if tr.Side != market.Sell || tr.Level != market.Rupees(94.5) || tr.In != market.Rupees(95) || tr.Out != market.Rupees(96) || tr.Gross != market.Rupees(-10) {
		t.Errorf("short %+v", tr)
	}
}

func TestBacktestTouchDoesNotFill(t *testing.T) {
	// 106 touches 105.5 with its low but never trades below it.
	trades, _ := run(t, testConfig(), path(keyA, day1, 1, 100), path(keyA, day2, 1, append(setup, 107, 106, 107)...))
	if len(trades) != 0 {
		t.Errorf("filled on a touch: %v", trades)
	}
}

func TestBacktestRanAway(t *testing.T) {
	// The pullback turns at the level without trading through it, and the
	// higher low confirms: the order goes.
	trades, stats := run(t, testConfig(), path(keyA, day1, 1, 100), path(keyA, day2, 1, append(setup, 107, 108, 107, 106, 107, 108)...))
	if len(trades) != 0 || stats.Ran != 1 {
		t.Errorf("trades %v stats %+v", trades, stats)
	}
}

func TestBacktestRoll(t *testing.T) {
	trades, _ := run(t, testConfig(),
		path(keyA, day1, 10, 100), path(keyB, day1, 1, 50),
		path(keyA, day2, 10, append(setup, 107, 106, 105, 106, 107)...), path(keyB, day2, 1000, 50),
		path(keyA, day3, 1, 108), path(keyB, day3, 100, 50))
	if len(trades) != 1 {
		t.Fatalf("trades %v", trades)
	}
	tr := trades[0]
	if tr.Reason != "roll" || tr.Key != keyA || tr.Out != market.Rupees(108) || tr.Exit != day3.At(10, 0) {
		t.Errorf("roll %+v", tr)
	}
}

func TestBacktestNoLookahead(t *testing.T) {
	// The same bars with or without a later future must trade the same way
	// up to where they diverge.
	base := append(setup, 107, 106, 105)
	a, _ := run(t, testConfig(), path(keyA, day1, 1, 100), path(keyA, day2, 1, append(base, 110, 112)...))
	b, _ := run(t, testConfig(), path(keyA, day1, 1, 100), path(keyA, day2, 1, append(base, 90, 80)...))
	if len(a) != 1 || len(b) != 1 || a[0].Entry != b[0].Entry || a[0].In != b[0].In {
		t.Errorf("entries differ: %v vs %v", a, b)
	}
}

func TestBacktestWickIsNotASignal(t *testing.T) {
	bars := path(keyA, day2, 1, append(setup[:14], 105, 105, 104)...)
	bars[14].High = market.Rupees(106.5) // through 105.5, closed back at 105
	_, stats := run(t, testConfig(), path(keyA, day1, 1, 100), bars)
	if stats.Placed != 0 {
		t.Errorf("a wick placed an order: %+v", stats)
	}
}

func TestBacktestMarketEntry(t *testing.T) {
	cfg := testConfig()
	cfg.Market = true
	trades, _ := run(t, cfg, path(keyA, day1, 1, 100), path(keyA, day2, 1, append(setup, retest...)...))
	if len(trades) != 1 || trades[0].In != market.Rupees(107) || trades[0].Entry != day2.At(10, 15) || trades[0].Out != market.Rupees(104) {
		t.Errorf("market entry %v", trades)
	}
}

func TestBacktestTarget(t *testing.T) {
	cfg := testConfig()
	cfg.TargetR = 0.2 // risk 105 − 100.45 = 4.55, target 105.91
	trades, _ := run(t, cfg, path(keyA, day1, 1, 100), path(keyA, day2, 1, append(setup, retest...)...))
	if len(trades) == 0 || trades[0].Reason != "target" || trades[0].Out != market.Rupees(106) || trades[0].Exit != day2.At(10, 18) {
		t.Errorf("target %v", trades)
	}
}
