package main

import (
	"context"
	"flag"
	"fmt"
	"math"
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

// runMCXEMA ranks EMA crossovers on one MCX commodity: on its active
// contract's minute bars, and on the continuous daily series.
func runMCXEMA(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcxema", flag.ContinueOnError)
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
	frames := []time.Duration{5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour}
	ed := mcx.PrepareEMA(bars[*name], frames...)

	type row struct {
		label              string
		n, h1n, h2n        int
		win, pf, avgPct    float64
		net, h1, h2, dd    float64
		long, short        float64
		bothHalvesPositive bool
	}
	var rows []row
	var ledgerNet market.Paise
	total := 0
	for _, fr := range frames {
		for _, p := range emaPairs {
			for _, lo := range []bool{false, true} {
				ts, err := mcx.EMABacktest(ed, mcx.EMAConfig{Name: *name, Spec: spec, Frame: fr, Fast: p[0], Slow: p[1],
					LongOnly: lo, Slip: *slipTicks, Costs: costs, From: f, To: t})
				if err != nil {
					return err
				}
				mode := "L+S"
				if lo {
					mode = "long"
				}
				s := summarize(ts)
				r := row{label: fmt.Sprintf("%-4s %5s %-4s", frameText(fr), fmt.Sprintf("%d/%d", p[0], p[1]), mode),
					n: s.n, win: s.win, pf: s.pf, net: s.net, dd: s.dd, avgPct: s.avgR}
				for _, tr := range ts {
					n := tr.Net.Rupees()
					if market.DateOf(tr.Entry).Before(split) {
						r.h1 += n
						r.h1n++
					} else {
						r.h2 += n
						r.h2n++
					}
					if tr.Side == market.Buy {
						r.long += n
					} else {
						r.short += n
					}
					ledgerNet += tr.Net
				}
				r.bothHalvesPositive = r.h1 > 0 && r.h2 > 0
				total += len(ts)
				rows = append(rows, r)
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].net > rows[j].net })

	fmt.Printf("%s EMA crossovers on the active contract's minute bars, %s to %s, 1 lot (%d× quote), %d tick slippage + charges\n",
		*name, f, t, spec.Multiplier, *slipTicks)
	fmt.Printf("signal at frame close, fill next minute open; held overnight; out at roll. halves split at %s\n\n", split)
	fmt.Printf("%-15s %6s %5s %5s %7s %10s %10s %10s %10s %10s %9s  %s\n", "frame pair mode", "trades", "win%", "PF",
		"avg %", "net ₹", "<split ₹", "≥split ₹", "long ₹", "short ₹", "max DD ₹", "both+")
	plus := 0
	for _, r := range rows {
		mark := ""
		if r.bothHalvesPositive {
			mark = "yes"
			plus++
		}
		fmt.Printf("%-15s %6d %4.0f%% %5s %7.3f %10.0f %10.0f %10.0f %10.0f %10.0f %9.0f  %s\n", r.label, r.n, r.win*100,
			pf(r.pf), r.avgPct, r.net, r.h1, r.h2, r.long, r.short, r.dd, mark)
	}
	fmt.Printf("\n%d of %d variants positive in both halves\n", plus, len(rows))
	if err := analytics.AppendTrials(analytics.LedgerPath(*dir), []analytics.TrialRecord{{
		Time: time.Now(), Command: "research mcxema", Strategy: "mcx_ema_cross", Underlying: *name,
		Params: strategy.Params{"grid": 1, "slip_ticks": float64(*slipTicks)}, From: f, To: t,
		Count: len(rows), Net: ledgerNet, Trades: total,
	}}); err != nil {
		return fmt.Errorf("trial ledger: %w", err)
	}

	return dailyEMA(ctx, store, *name, spec, costs, bars[*name], t, emaPairs, indicator.EMA, "EMA")
}

func frameText(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// dailyEMA runs crossovers of pairs of the average ma on the continuous daily series. Kite's
// continuous series is not back-adjusted: each roll to the next contract's
// (usually higher) price shows up as a move, which favours longs.
func dailyEMA(ctx context.Context, store *data.Store, name string, spec mcx.Spec, costs *cost.Schedule,
	minute []market.Bar, to market.Date, pairs [][2]int, ma func([]market.Bar, int) []float64, maName string) error {
	all, err := store.Daily(ctx, name)
	if err != nil {
		return err
	}
	var days []market.Bar
	for _, b := range all {
		if !to.Before(market.DateOf(b.Time)) {
			days = append(days, b)
		}
	}
	if len(days) < 300 || len(minute) == 0 {
		return fmt.Errorf("not enough daily data for %s", name)
	}
	inst, err := market.ParseKey(minute[len(minute)-1].Key) // for charges only
	if err != nil {
		return err
	}
	mid := market.DateOf(days[len(days)/2].Time)
	slip := float64(spec.Tick.Rupees())
	charge := func(side market.Side, px market.Paise) float64 {
		c, err := costs.Charges(market.Fill{Instrument: inst, Side: side, Qty: spec.Multiplier, Price: px,
			Time: time.Date(2026, 1, 2, 10, 0, 0, 0, market.IST), Kind: market.Traded})
		if err != nil {
			return 0
		}
		return c.Rupees()
	}
	type res struct {
		label       string
		n           int
		net, h1, h2 float64
		pctSum, win float64
		long, short float64
	}
	var out []res
	for _, p := range pairs {
		for _, lo := range []bool{false, true} {
			fast, slow := ma(days, p[0]), ma(days, p[1])
			r := res{label: fmt.Sprintf("%5s %-4s", fmt.Sprintf("%d/%d", p[0], p[1]), map[bool]string{false: "L+S", true: "long"}[lo])}
			var side market.Side
			var in float64
			var inAt market.Date
			closeAt := func(px float64) {
				mv := (px-in)*float64(side) - 2*slip
				net := mv*float64(spec.Multiplier) - charge(side, market.Paise(in*100)) - charge(-side, market.Paise(px*100))
				r.n++
				r.net += net
				if inAt.Before(mid) {
					r.h1 += net
				} else {
					r.h2 += net
				}
				if side == market.Buy {
					r.long += net
				} else {
					r.short += net
				}
				if net > 0 {
					r.win++
				}
				r.pctSum += mv / in * 100
				side = 0
			}
			for i := 1; i < len(days); i++ {
				var want market.Side
				switch {
				case math.IsNaN(slow[i-1]) || math.IsNaN(fast[i-1]):
					continue
				case fast[i-1] > slow[i-1]:
					want = market.Buy
				case !lo && fast[i-1] < slow[i-1]:
					want = market.Sell
				}
				if side != want {
					px := days[i].Open.Rupees()
					if side != 0 {
						closeAt(px)
					}
					if want != 0 {
						side, in, inAt = want, px, market.DateOf(days[i].Time)
					}
				}
			}
			if side != 0 {
				closeAt(days[len(days)-1].Close.Rupees())
			}
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].net > out[j].net })
	first, last := days[0], days[len(days)-1]
	hold := (last.Close - first.Open).Rupees() * float64(spec.Multiplier)
	fmt.Printf("\n%s %s crossovers, continuous daily (not back-adjusted), %s to %s; signal at close, fill next open; halves split at %s\n",
		name, maName, market.DateOf(first.Time), market.DateOf(last.Time), mid)
	fmt.Printf("buy and hold 1 lot: ₹%.0f\n\n", hold)
	fmt.Printf("%-10s %6s %5s %8s %12s %12s %12s %12s %12s\n", "pair mode", "trades", "win%", "avg %", "net ₹", "1st half ₹",
		"2nd half ₹", "long ₹", "short ₹")
	for _, r := range out {
		fmt.Printf("%-10s %6d %4.0f%% %8.2f %12.0f %12.0f %12.0f %12.0f %12.0f\n", r.label, r.n, 100*r.win/float64(max(r.n, 1)),
			r.pctSum/float64(max(r.n, 1)), r.net, r.h1, r.h2, r.long, r.short)
	}
	return nil
}
