package analytics

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/engine"
	"serene/internal/margin"
	"serene/internal/market"
	"serene/internal/portfolio"
	"serene/internal/sim"
	"serene/internal/strategy"
	"serene/internal/synth"
)

func TestMeasureByHand(t *testing.T) {
	d, _ := market.ParseDate("2026-11-02")
	r := engine.Result{
		Capital: market.Rupees(1000),
		Equity: []engine.EquityPoint{
			{Date: d, Equity: market.Rupees(1100)},
			{Date: d.AddDays(1), Equity: market.Rupees(990)},
			{Date: d.AddDays(2), Equity: market.Rupees(1188)},
		},
		Charges:     market.Rupees(10),
		MaxDrawdown: market.Rupees(110),
		Trades: []portfolio.Trade{
			{Tag: "a", Gross: market.Rupees(120), Charges: market.Rupees(5)}, // legs of
			{Tag: "a", Gross: market.Rupees(-20), Charges: 0},                // one structure
			{Tag: "b", Gross: market.Rupees(-50), Charges: 0},
			{Tag: "c", Gross: market.Rupees(150), Charges: market.Rupees(5), Settled: true},
		},
	}
	m := Measure(r, 2)
	if m.Net != market.Rupees(188) || m.Gross != market.Rupees(198) || m.ReturnPct != 18.8 {
		t.Errorf("pnl: %+v", m)
	}
	if m.Trades != 3 || math.Abs(m.WinRatePct-200.0/3) > 1e-9 {
		t.Errorf("trade count should group by tag: %+v", m)
	}
	// Tags net +95, -50, +145.
	if math.Abs(m.ProfitFactor-4.8) > 1e-9 || m.AvgWin != market.Rupees(120) || m.AvgLoss != market.Rupees(50) {
		t.Errorf("win/loss: %+v", m)
	}
	if m.MaxDrawdownPct != 11 || m.SettledLegs != 1 || m.Unfilled != 2 {
		t.Errorf("other: %+v", m)
	}
	// Returns +10%, -10%, +20%: mean 6.67%, sample sd 15.27%.
	if math.Abs(m.Sharpe-6.93) > 0.01 {
		t.Errorf("sharpe %f", m.Sharpe)
	}
}

func TestGrid(t *testing.T) {
	g := Grid(strategy.Params{"lots": 2}, map[string][]float64{"a": {1, 2}, "b": {3, 4, 5}})
	if len(g) != 6 || g[0]["lots"] != 2 || g[5]["a"] != 2 || g[5]["b"] != 5 {
		t.Fatalf("got %v", g)
	}
}

func TestBestSkipsCombinationsThatNeverTraded(t *testing.T) {
	best, ok := Best([]Trial{
		{Params: strategy.Params{"x": 1}, Metrics: Metrics{Trades: 0, Sharpe: 0}},
		{Params: strategy.Params{"x": 2}, Metrics: Metrics{Trades: 5, Sharpe: -1}},
	}, "sharpe")
	if !ok || best.Params["x"] != 2 {
		t.Fatalf("got %+v", best)
	}
}

func TestGroupIndianDigits(t *testing.T) {
	for n, want := range map[int64]string{5: "5", 999: "999", 1000: "1,000", 123456: "1,23,456", 12345678: "1,23,45,678"} {
		if got := group(n); got != want {
			t.Errorf("%d: %s", n, got)
		}
	}
}

func fixture(t *testing.T, days int) (*data.Store, Spec) {
	t.Helper()
	ctx := context.Background()
	s, err := data.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	from, _ := market.ParseDate("2026-10-05")
	cfg := synth.Default(from, from.AddDays(days), 11)
	cfg.StrikesEachSide = 4
	cfg.Expiries = 1
	if _, err := synth.Generate(ctx, s, cfg); err != nil {
		t.Fatal(err)
	}
	costs, err := cost.Load("../../configs/costs.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return s, Spec{Strategy: "short_straddle", Underlying: "NIFTY", From: from, To: from.AddDays(days),
		Capital: market.Rupees(1e6), Sim: sim.Config{SlippageBps: 30, MinTicks: 2, Tick: 5, MaxWait: 3 * time.Minute},
		Margin: margin.Model{NakedPct: 12}, Costs: costs}
}

func TestEndToEndOnSyntheticData(t *testing.T) {
	ctx := context.Background()
	store, spec := fixture(t, 20)
	for _, name := range []string{"short_straddle", "breakout"} {
		t.Run(name, func(t *testing.T) {
			s := spec
			s.Strategy = name
			a, err := Run(ctx, store, s)
			if err != nil {
				t.Fatal(err)
			}
			if a.Metrics.Trades == 0 {
				t.Fatal("no trades")
			}
			// Cash must reconcile to the sum of trades.
			var net market.Paise
			for _, tr := range a.Result.Trades {
				net += tr.Net()
			}
			if net != a.Metrics.Net {
				t.Errorf("trades net %d, equity net %d", net, a.Metrics.Net)
			}
			b, err := Run(ctx, store, s)
			if err != nil {
				t.Fatal(err)
			}
			if a.Metrics != b.Metrics {
				t.Error("same input gave different results")
			}
		})
	}
}

func TestSlippageNeverHelps(t *testing.T) {
	store, spec := fixture(t, 12)
	rows, err := SlippageSensitivity(context.Background(), store, spec, []float64{0, 1, 3})
	if err != nil {
		t.Fatal(err)
	}
	if !(rows[0].Metrics.Net > rows[1].Metrics.Net && rows[1].Metrics.Net > rows[2].Metrics.Net) {
		t.Errorf("net should fall as slippage rises: %s %s %s",
			rows[0].Metrics.Net, rows[1].Metrics.Net, rows[2].Metrics.Net)
	}
}

func TestSweepAndWalkForward(t *testing.T) {
	ctx := context.Background()
	store, spec := fixture(t, 20)
	combos := Grid(nil, map[string][]float64{"stop_pct": {20, 40}})
	trials, err := Sweep(ctx, store, spec, combos)
	if err != nil {
		t.Fatal(err)
	}
	if len(trials) != 2 || trials[0].Params["stop_pct"] != 20 {
		t.Fatalf("trials %+v", trials)
	}
	wf, err := RunWalkForward(ctx, store, spec, combos, "sharpe", 8, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(wf.Windows) < 2 {
		t.Fatalf("windows %d", len(wf.Windows))
	}
	var sum market.Paise
	for i, w := range wf.Windows {
		if !w.TrainTo.Before(w.TestFrom) {
			t.Errorf("window %d tests on training days", i)
		}
		sum += w.OutOfSample.Net
	}
	if sum != wf.Combined.Net {
		t.Errorf("combined net %d, windows sum %d", wf.Combined.Net, sum)
	}
	if _, err := RunWalkForward(ctx, store, spec, combos, "sharpe", 500, 3); err == nil {
		t.Error("oversized window accepted")
	}
}

func TestReportWrites(t *testing.T) {
	ctx := context.Background()
	store, spec := fixture(t, 8)
	o, err := Run(ctx, store, spec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "r", "report.html")
	if err := WriteReport(path, o, []SlippageRow{{1, o.Metrics}}, Provenance{Trials: 7, Unsealed: true}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"short_straddle on NIFTY", "Sensitivity to slippage", "const points = [",
		"Trial 7 of this strategy", "This run read held-back data"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("report missing %q", want)
		}
	}
}
