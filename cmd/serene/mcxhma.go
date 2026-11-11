package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"time"

	"serene/internal/analytics"
	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/indicator"
	"serene/internal/market"
	"serene/internal/mcx"
	"serene/internal/strategy"
)

// hmaPairs are the Hull crossovers tried, fast/slow.
var hmaPairs = [][2]int{{5, 13}, {9, 21}, {13, 34}, {16, 49}, {20, 50}, {21, 55}, {34, 89}}

// hmaSlopes are the single Hull lengths traded on their own turn: long while
// HMA is above its value two frame bars back, short while below.
var hmaSlopes = []int{9, 16, 21, 34, 55}

// runMCXHMA ranks Hull moving average crossovers and Hull turns on one MCX
// commodity's active contract, using the same engine as mcxema, plus the
// crossovers on the continuous daily series.
func runMCXHMA(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcxhma", flag.ContinueOnError)
	name := fs.String("name", "GOLDM", "commodity")
	from := fs.String("from", "2026-06-15", "first session traded on minute bars")
	to := fs.String("to", "2026-11-10", "last session traded")
	splitS := fs.String("split", "2026-09-01", "halves split for the minute grid")
	dir := fs.String("data", "data/mcx", "data directory")
	holdout := fs.String("holdout", "configs/holdout.yaml", "data held back from research")
	costsFile := fs.String("costs", "configs/costs_mcx.yaml", "charges schedule")
	slipTicks := fs.Int64("slip-ticks", 1, "ticks against every fill")
	if err := fs.Parse(args); err != nil {
		return err
	}
	f, err := market.ParseDate(*from)
	if err != nil {
		return err
	}
	t, err := market.ParseDate(*to)
	if err != nil {
		return err
	}
	split, err := market.ParseDate(*splitS)
	if err != nil {
		return err
	}
	spec, ok := mcx.Specs[*name]
	if !ok {
		return fmt.Errorf("no contract spec for %s", *name)
	}
	h, err := analytics.LoadHoldouts(*holdout)
	if err != nil {
		return err
	}
	if _, err := h.Check(*dir, t, false); err != nil {
		return err
	}
	costs, err := cost.Load(*costsFile)
	if err != nil {
		return err
	}
	store, err := data.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()

	bars, err := loadMCXBars(ctx, store, []string{*name}, f.AddDays(-30), t)
	if err != nil {
		return err
	}
	frames := []time.Duration{5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour}
	ed := mcx.PrepareEMA(bars[*name], frames...)

	type signal struct {
		label      string
		fast, slow int
		slope      bool
	}
	var signals []signal
	for _, p := range hmaPairs {
		signals = append(signals, signal{label: fmt.Sprintf("%d/%d", p[0], p[1]), fast: p[0], slow: p[1]})
	}
	for _, n := range hmaSlopes {
		signals = append(signals, signal{label: fmt.Sprintf("turn %d", n), fast: n, slope: true})
	}

	type row struct {
		label              string
		n                  int
		win, pf, avgPct    float64
		net, h1, h2, dd    float64
		long, short        float64
		bothHalvesPositive bool
	}
	build := func(label string, ts []mcx.Trade) row {
		s := summarize(ts)
		r := row{label: label, n: s.n, win: s.win, pf: s.pf, net: s.net, dd: s.dd, avgPct: s.avgR}
		for _, tr := range ts {
			n := tr.Net.Rupees()
			if market.DateOf(tr.Entry).Before(split) {
				r.h1 += n
			} else {
				r.h2 += n
			}
			if tr.Side == market.Buy {
				r.long += n
			} else {
				r.short += n
			}
		}
		r.bothHalvesPositive = r.h1 > 0 && r.h2 > 0
		return r
	}
	var rows []row
	var ledgerNet market.Paise
	total := 0
	for _, fr := range frames {
		for _, sg := range signals {
			for _, lo := range []bool{false, true} {
				ts, err := mcx.EMABacktest(ed, mcx.EMAConfig{Name: *name, Spec: spec, Frame: fr, Fast: sg.fast, Slow: sg.slow,
					Slope: sg.slope, MA: indicator.HMA, LongOnly: lo, Slip: *slipTicks, Costs: costs, From: f, To: t})
				if err != nil {
					return err
				}
				mode := map[bool]string{false: "L+S", true: "long"}[lo]
				rows = append(rows, build(fmt.Sprintf("%-4s %-7s %-4s", frameText(fr), sg.label, mode), ts))
				for _, tr := range ts {
					ledgerNet += tr.Net
				}
				total += len(ts)
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].net > rows[j].net })
	base, err := mcx.EMABacktest(ed, mcx.EMAConfig{Name: *name, Spec: spec, Frame: 15 * time.Minute, Fast: 13, Slow: 34,
		Slip: *slipTicks, Costs: costs, From: f, To: t})
	if err != nil {
		return err
	}

	fmt.Printf("%s Hull MA crossovers and turns on the active contract's minute bars, %s to %s, 1 lot (%d× quote), %d tick slippage + charges\n",
		*name, f, t, spec.Multiplier, *slipTicks)
	fmt.Printf("signal at frame close, fill next minute open; held overnight; out at roll. halves split at %s\n", split)
	fmt.Printf("frames of 1h and more count from the 09:00 open. \"turn n\": HMA n against itself two frame bars back\n\n")
	head := func() {
		fmt.Printf("%-18s %6s %5s %5s %7s %10s %10s %10s %10s %10s %9s  %s\n", "frame signal mode", "trades", "win%", "PF",
			"avg %", "net ₹", "<split ₹", "≥split ₹", "long ₹", "short ₹", "max DD ₹", "both+")
	}
	line := func(r row) {
		mark := ""
		if r.bothHalvesPositive {
			mark = "yes"
		}
		fmt.Printf("%-18s %6d %4.0f%% %5s %7.3f %10.0f %10.0f %10.0f %10.0f %10.0f %9.0f  %s\n", r.label, r.n, r.win*100,
			pf(r.pf), r.avgPct, r.net, r.h1, r.h2, r.long, r.short, r.dd, mark)
	}
	head()
	line(build("EMA 15m 13/34 L+S", base))
	fmt.Println()
	plus := 0
	for _, r := range rows {
		line(r)
		if r.bothHalvesPositive {
			plus++
		}
	}
	fmt.Printf("\n%d of %d variants positive in both halves\n", plus, len(rows))

	fmt.Printf("\nby frame (L+S only): variants positive overall / in both halves, median net ₹\n")
	for _, fr := range frames {
		var nets []float64
		pos, both := 0, 0
		for _, r := range rows {
			if r.label[:4] != fmt.Sprintf("%-4s", frameText(fr)) || r.label[len(r.label)-4:] != "L+S " {
				continue
			}
			nets = append(nets, r.net)
			if r.net > 0 {
				pos++
			}
			if r.bothHalvesPositive {
				both++
			}
		}
		sort.Float64s(nets)
		med := 0.0
		if len(nets) > 0 {
			med = nets[len(nets)/2]
		}
		fmt.Printf("  %-4s %2d/%d  %2d/%d  %10.0f\n", frameText(fr), pos, len(nets), both, len(nets), med)
	}

	if err := analytics.AppendTrials(analytics.LedgerPath(*dir), []analytics.TrialRecord{{
		Time: time.Now(), Command: "research mcxhma", Strategy: "mcx_hma_cross", Underlying: *name,
		Params: strategy.Params{"grid": 1, "slip_ticks": float64(*slipTicks)}, From: f, To: t,
		Count: len(rows), Net: ledgerNet, Trades: total,
	}}); err != nil {
		return fmt.Errorf("trial ledger: %w", err)
	}

	return dailyEMA(ctx, store, *name, spec, costs, bars[*name], t, hmaPairs, indicator.HMA, "Hull MA")
}
