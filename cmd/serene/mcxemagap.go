package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"serene/internal/analytics"
	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/market"
	"serene/internal/mcx"
	"serene/internal/strategy"
)

// emaGaps are the minimum EMA gaps tried, in percent of the slow EMA. 0 is
// the plain cross.
var emaGaps = []float64{0, 0.01, 0.02, 0.03, 0.05, 0.075, 0.1, 0.15, 0.2}

// runMCXEMAGap reruns the best `research mcxema` long+short crosses with a
// minimum gap: the side flips only when the fast EMA clears the slow one by
// that much, so thin crosses that soon reverse are not traded.
func runMCXEMAGap(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcxemagap", flag.ContinueOnError)
	name := fs.String("name", "GOLDM", "commodity")
	from := fs.String("from", "2026-06-15", "first session traded")
	to := fs.String("to", "2026-11-10", "last session traded")
	splitS := fs.String("split", "2026-09-01", "halves split")
	dir := fs.String("data", "data/mcx", "data directory")
	holdout := fs.String("holdout", "configs/holdout.yaml", "data held back from research")
	costsFile := fs.String("costs", "configs/costs_mcx.yaml", "charges schedule")
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
	type combo struct {
		frame      time.Duration
		fast, slow int
	}
	// The frozen 15m 13/34 first, then the other crosses positive in both halves.
	combos := []combo{{15 * time.Minute, 13, 34}, {15 * time.Minute, 8, 21}, {15 * time.Minute, 9, 21},
		{30 * time.Minute, 5, 13}, {30 * time.Minute, 5, 20}, {30 * time.Minute, 9, 21}}
	ed := mcx.PrepareEMA(bars[*name], 15*time.Minute, 30*time.Minute)

	fmt.Printf("%s EMA crosses with a minimum gap (%% of slow EMA), long+short, %s to %s, 1 lot, charges; halves split at %s\n",
		*name, f, t, split)
	fmt.Printf("inside the gap the side already held is kept\n")
	var ledgerNet market.Paise
	total, count, plus := 0, 0, 0
	for _, c := range combos {
		fmt.Printf("\n%s %d/%d\n%7s %6s %5s %5s %7s %10s %10s %10s %10s %10s %9s %11s  %s\n", frameText(c.frame), c.fast, c.slow,
			"gap %", "trades", "win%", "PF", "avg %", "net ₹", "<split ₹", "≥split ₹", "long ₹", "short ₹", "max DD ₹", "net 3tk ₹", "both+")
		for _, g := range emaGaps {
			run := func(slip int64) ([]mcx.Trade, error) {
				return mcx.EMABacktest(ed, mcx.EMAConfig{Name: *name, Spec: spec, Frame: c.frame, Fast: c.fast, Slow: c.slow,
					MinGap: g, Slip: slip, Costs: costs, From: f, To: t})
			}
			ts, err := run(1)
			if err != nil {
				return err
			}
			ts3, err := run(3)
			if err != nil {
				return err
			}
			s, s3 := summarize(ts), summarize(ts3)
			var h1, h2, long, short float64
			for _, tr := range ts {
				n := tr.Net.Rupees()
				if market.DateOf(tr.Entry).Before(split) {
					h1 += n
				} else {
					h2 += n
				}
				if tr.Side == market.Buy {
					long += n
				} else {
					short += n
				}
				ledgerNet += tr.Net
			}
			mark := ""
			if h1 > 0 && h2 > 0 {
				mark = "yes"
				plus++
			}
			total += len(ts)
			count++
			fmt.Printf("%7.3f %6d %4.0f%% %5s %7.3f %10.0f %10.0f %10.0f %10.0f %10.0f %9.0f %11.0f  %s\n", g, s.n, s.win*100,
				pf(s.pf), s.avgR, s.net, h1, h2, long, short, s.dd, s3.net, mark)
		}
	}
	fmt.Printf("\n%d of %d variants positive in both halves\n", plus, count)
	return analytics.AppendTrials(analytics.LedgerPath(*dir), []analytics.TrialRecord{{
		Time: time.Now(), Command: "research mcxemagap", Strategy: "mcx_ema_cross_gap", Underlying: *name,
		Params: strategy.Params{"grid": 1, "slip_ticks": 1}, From: f, To: t,
		Count: count, Net: ledgerNet, Trades: total,
	}})
}
