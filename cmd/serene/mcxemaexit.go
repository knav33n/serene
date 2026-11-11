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
	"serene/internal/market"
	"serene/internal/mcx"
	"serene/internal/strategy"
)

// runMCXEMAExit tries exit rules on the GOLDM 15m 13/34 long+short cross and
// reports the mark-to-market drawdown beside the closed-trade one.
func runMCXEMAExit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcxemaexit", flag.ContinueOnError)
	name := fs.String("name", "GOLDM", "commodity")
	from := fs.String("from", "2026-06-15", "first session traded")
	to := fs.String("to", "2026-11-10", "last session traded")
	splitS := fs.String("split", "2026-09-01", "halves split")
	frameM := fs.Int("frame", 15, "signal frame, minutes")
	fastN := fs.Int("fast", 13, "fast EMA")
	slowN := fs.Int("slow", 34, "slow EMA")
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
	frame := time.Duration(*frameM) * time.Minute
	ed := mcx.PrepareEMA(bars[*name], frame)

	type variant struct {
		label string
		ex    mcx.EMAExits
	}
	vs := []variant{{"cross only (frozen)", mcx.EMAExits{}}}
	add := func(label string, ex mcx.EMAExits) { vs = append(vs, variant{label, ex}) }
	for _, re := range []bool{false, true} {
		sfx := ""
		if re {
			sfx = " +re"
		}
		for _, p := range []float64{0.5, 0.75, 1, 1.5, 2} {
			add(fmt.Sprintf("stop %.2g%%%s", p, sfx), mcx.EMAExits{StopPct: p, ReEnter: re})
		}
		for _, k := range []float64{2, 3, 4, 6} {
			add(fmt.Sprintf("stop %gATR%s", k, sfx), mcx.EMAExits{StopATR: k, ReEnter: re})
		}
		for _, k := range []float64{3, 4, 6, 8, 12} {
			add(fmt.Sprintf("trail %gATR%s", k, sfx), mcx.EMAExits{TrailATR: k, ReEnter: re})
		}
		for _, p := range []float64{0.5, 1, 1.5} {
			add(fmt.Sprintf("trail %.2g%%%s", p, sfx), mcx.EMAExits{TrailPct: p, ReEnter: re})
		}
		for _, p := range []float64{1, 2, 3} {
			add(fmt.Sprintf("target %g%%%s", p, sfx), mcx.EMAExits{TargetPct: p, ReEnter: re})
		}
		add("close<slow"+sfx, mcx.EMAExits{CloseSlow: true, ReEnter: re})
		for _, n := range []int{10, 20, 40} {
			add(fmt.Sprintf("donchian %d%s", n, sfx), mcx.EMAExits{Donchian: n, ReEnter: re})
		}
		for _, n := range []int{4, 8, 16} {
			add(fmt.Sprintf("time %d bars%s", n, sfx), mcx.EMAExits{TimeBars: n, ReEnter: re})
		}
		add("stop 1% + trail 6ATR"+sfx, mcx.EMAExits{StopPct: 1, TrailATR: 6, ReEnter: re})
		add("stop 3ATR + BE 0.5%"+sfx, mcx.EMAExits{StopATR: 3, BreakevenPct: 0.5, ReEnter: re})
		add("BE 0.5% + target 2%"+sfx, mcx.EMAExits{BreakevenPct: 0.5, TargetPct: 2, ReEnter: re})
	}
	add("close<fast +re", mcx.EMAExits{CloseFast: true, ReEnter: true})
	for _, p := range []float64{0.3, 0.5, 1} {
		add(fmt.Sprintf("breakeven %.2g%%", p), mcx.EMAExits{BreakevenPct: p})
	}
	add("flat EOD", mcx.EMAExits{FlatEOD: true})
	add("flat EOD + stop 1%", mcx.EMAExits{FlatEOD: true, StopPct: 1})
	add("flat EOD + trail 6ATR", mcx.EMAExits{FlatEOD: true, TrailATR: 6})

	type row struct {
		label                     string
		n                         int
		win, pf, net, h1, h2      float64
		dd, mtm, worst, net3, avg float64
		both                      bool
	}
	var rows []row
	var ledgerNet market.Paise
	total := 0
	for _, v := range vs {
		cfg := mcx.EMAConfig{Name: *name, Spec: spec, Frame: frame, Fast: *fastN, Slow: *slowN, Slip: 1, Costs: costs, From: f, To: t}
		r1, err := mcx.EMAExitBacktest(ed, cfg, v.ex)
		if err != nil {
			return err
		}
		cfg.Slip = 3
		r3, err := mcx.EMAExitBacktest(ed, cfg, v.ex)
		if err != nil {
			return err
		}
		s := summarize(r1.Trades)
		r := row{label: v.label, n: s.n, win: s.win, pf: s.pf, net: s.net, dd: s.dd, mtm: r1.MTMDD,
			net3: summarize(r3.Trades).net, worst: math.Inf(1)}
		for _, tr := range r1.Trades {
			n := tr.Net.Rupees()
			if market.DateOf(tr.Entry).Before(split) {
				r.h1 += n
			} else {
				r.h2 += n
			}
			r.worst = math.Min(r.worst, n)
			ledgerNet += tr.Net
		}
		r.both = r.h1 > 0 && r.h2 > 0
		total += len(r1.Trades)
		rows = append(rows, r)
	}
	base := rows[0]
	sort.SliceStable(rows[1:], func(i, j int) bool {
		a, b := rows[1+i], rows[1+j]
		return a.net/math.Max(a.mtm, 1) > b.net/math.Max(b.mtm, 1)
	})
	fmt.Printf("%s %s %d/%d long+short with exit rules, %s to %s, 1 lot (%d× quote), 1 tick + charges; halves split at %s\n",
		*name, frameText(frame), *fastN, *slowN, f, t, spec.Multiplier, split)
	fmt.Printf("closed DD: on booked trades; MTM DD: booked + open P&L each minute. +re: back in on a close beyond the fast EMA\n")
	fmt.Printf("ranked by net / MTM DD\n\n")
	fmt.Printf("%-24s %6s %5s %5s %10s %10s %10s %10s %10s %10s %6s %10s  %s\n", "exit", "trades", "win%", "PF", "net ₹",
		"<split ₹", "≥split ₹", "closed DD", "MTM DD", "worst ₹", "net/DD", "net 3tk ₹", "both+")
	plus := 0
	for i, r := range rows {
		if i == 1 {
			fmt.Println()
		}
		mark := ""
		if r.both {
			mark = "yes"
			plus++
		}
		fmt.Printf("%-24s %6d %4.0f%% %5s %10.0f %10.0f %10.0f %10.0f %10.0f %10.0f %6.2f %10.0f  %s\n", r.label, r.n, r.win*100,
			pf(r.pf), r.net, r.h1, r.h2, r.dd, r.mtm, r.worst, r.net/math.Max(r.mtm, 1), r.net3, mark)
	}
	beat := 0
	for _, r := range rows[1:] {
		if r.net/math.Max(r.mtm, 1) > base.net/math.Max(base.mtm, 1) && r.h2 > base.h2 {
			beat++
		}
	}
	fmt.Printf("\n%d of %d variants positive in both halves; %d beat the frozen net/MTM DD and its ≥split net\n", plus, len(rows), beat)
	return analytics.AppendTrials(analytics.LedgerPath(*dir), []analytics.TrialRecord{{
		Time: time.Now(), Command: "research mcxemaexit", Strategy: "mcx_ema_cross_exit", Underlying: *name,
		Params: strategy.Params{"frame_min": float64(*frameM), "fast": float64(*fastN), "slow": float64(*slowN), "slip_ticks": 1},
		From:   f, To: t, Count: len(rows), Net: ledgerNet, Trades: total,
	}})
}
