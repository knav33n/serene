package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"serene/internal/analytics"
	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/market"
	"serene/internal/paper"
	"serene/internal/strategy"
)

// hmaVariant is one set of nifty_hma_retest rules replayed by the backtest.
type hmaVariant struct {
	name              string
	firstEntry, first time.Duration // paper.Config FirstEntry, FirstArm
	fast, slow        int           // zero: the default's
	immediate         bool
}

// runHMARetestBacktest replays paper.HMARetest, the rules the live system
// trades (cross_from_0930) and two variants, over the stored Upstox 1-minute bars: each day the nearest-expiry
// CE and PE at the NIFTY 09:15 open rounded to --step, warmed with the
// previous trading day's bars, ₹1L sized. It writes every trade with charges,
// and again with --slip-bps taken off each side.
func runHMARetestBacktest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("hmaretest", flag.ContinueOnError)
	underlying := fs.String("underlying", "NIFTY", "underlying name")
	from := fs.String("from", "2025-11-17", "first day")
	to := fs.String("to", "2026-11-16", "last day")
	dir := fs.String("data", "data/upstox", "data directory")
	holdout := fs.String("holdout", "configs/holdout.yaml", "data held back from research")
	unseal := fs.Bool("unseal", false, "allow a range reaching the holdout; recorded")
	costsFile := fs.String("costs", "configs/costs.yaml", "charges schedule")
	slipBps := fs.Float64("slip-bps", 30, "slippage per side, at least 2 ticks")
	step := fs.Float64("step", 100, "the index open is rounded to this for the strike")
	out := fs.String("trades", "reports/hmaretest_backtest.csv", "every trade of every variant")
	tf := fs.Int("tf", 1, "bar size in minutes; 1-minute bars are resampled to it")
	warmDays := fs.Int("warm", 1, "prior trading days of bars to warm the averages")
	immediate := fs.Bool("immediate", false, "buy at the open of the bar after the cross, not at the slow-HMA retest")
	grid := fs.Bool("grid", false, "run a fast x slow HMA grid, crosses from 09:30, instead of the three timing variants")
	split := fs.String("split", "2026-05-01", "first day of the second half, for the H1/H2 columns")
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
	h, err := analytics.LoadHoldouts(*holdout)
	if err != nil {
		return err
	}
	if _, err := h.Check(*dir, t, *unseal); err != nil {
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
	insts, err := store.Contracts(ctx, *underlying)
	if err != nil {
		return err
	}
	lots := map[string]market.Instrument{}
	for _, in := range insts {
		lots[in.Key()] = in
	}
	days, err := store.Days(*underlying, f.AddDays(-10), t)
	if err != nil {
		return err
	}
	half, err := market.ParseDate(*split)
	if err != nil {
		return err
	}
	variants := []hmaVariant{
		{name: "original"},
		{name: "fill_from_0930", firstEntry: 15 * time.Minute},
		{name: "cross_from_0930", firstEntry: 15 * time.Minute, first: 15 * time.Minute}, // live from 2026-11-17
	}
	if *grid {
		variants = nil
		for _, fa := range []int{5, 8, 9, 13, 21} {
			for _, sl := range []int{13, 21, 34, 50} {
				if sl > fa {
					variants = append(variants, hmaVariant{name: fmt.Sprintf("hma_%d_%d", fa, sl),
						firstEntry: 15 * time.Minute, first: 15 * time.Minute, fast: fa, slow: sl})
				}
			}
		}
	}
	for i := range variants {
		variants[i].immediate = *immediate
	}
	slip := func(p market.Paise, buy bool) market.Paise {
		s := max(market.Paise(math.Round(float64(p)**slipBps/1e4)), 10)
		if buy {
			return p + s
		}
		return max(p-s, 5)
	}
	net := func(inst market.Instrument, qty int64, e, x market.Paise, in, outT time.Time) (float64, error) {
		cb, err := costs.Charges(market.Fill{Instrument: inst, Side: market.Buy, Qty: qty, Price: e, Time: in, Kind: market.Traded})
		if err != nil {
			return 0, err
		}
		cs, err := costs.Charges(market.Fill{Instrument: inst, Side: market.Sell, Qty: qty, Price: x, Time: outT, Kind: market.Traded})
		if err != nil {
			return 0, err
		}
		return ((x-e)*market.Paise(qty) - cb - cs).Rupees(), nil
	}

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	fh, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer fh.Close()
	w := csv.NewWriter(fh)
	w.Write([]string{"variant", "day", "instrument", "armed", "entry_time", "entry", "qty", "stop",
		"exit_time", "exit", "reason", "net", "net_slip"})

	type stat struct {
		n, wins        int
		net, slip, max float64
		gross, loss    float64
		dd, peak, cum  float64
		months         map[string]float64
		reasons        map[string]int
		early, earlyPL float64 // trades filled before 09:30 and their P&L
		h1, h2         float64
		dayLow         int
		nets           []float64
	}
	stats := make([]stat, len(variants))
	for i := range stats {
		stats[i].months, stats[i].reasons = map[string]float64{}, map[string]int{}
	}
	indexKey := market.Index(market.NSE, *underlying).Key()
	var warmBuf []map[string][]market.Bar // the last --warm stored days, oldest first
	traded, skipped := 0, 0
	for _, d := range days {
		all, err := store.DayBars(ctx, *underlying, d)
		if err != nil {
			return err
		}
		byKey := map[string][]market.Bar{}
		for _, b := range all {
			byKey[b.Key] = append(byKey[b.Key], b)
		}
		warm := warmBuf
		warmBuf = append(warmBuf, byKey)
		if len(warmBuf) > *warmDays {
			warmBuf = warmBuf[len(warmBuf)-*warmDays:]
		}
		if d.Before(f) {
			continue
		}
		idx := byKey[indexKey]
		if len(idx) == 0 || !idx[0].Time.Equal(d.At(9, 15)) {
			skipped++
			continue
		}
		strike := market.Rupees(math.Round(idx[0].Open.Rupees() / *step) * *step)
		exp, ok := nearestExpiry(lots, byKey, d, strike, *underlying)
		if !ok {
			skipped++
			continue
		}
		var legs []paper.Leg
		for _, r := range []market.Right{market.Call, market.Put} {
			k := market.Option(market.NFO, *underlying, exp, strike, r, 0).Key()
			inst, ok := lots[k]
			if !ok || len(byKey[k]) == 0 {
				continue
			}
			var bars []market.Bar
			for _, w := range warm {
				bars = append(bars, w[k]...)
			}
			bars = append(bars, byKey[k]...)
			if *tf > 1 {
				if bars, err = market.Resample(bars, market.NSESession, market.Timeframe(*tf)); err != nil {
					return err
				}
			}
			legs = append(legs, paper.Leg{Instrument: inst, Bars: bars})
		}
		if len(legs) < 2 {
			skipped++
			continue
		}
		traded++
		for vi, v := range variants {
			cfg := paper.DefaultHMARetest(d)
			cfg.FirstEntry, cfg.FirstArm = v.firstEntry, v.first
			cfg.Immediate = v.immediate
			if v.fast > 0 {
				cfg.Fast, cfg.Slow = v.fast, v.slow
			}
			tr, err := paper.HMARetest(legs, cfg)
			if err != nil {
				return err
			}
			if tr == nil {
				continue
			}
			if tr.Open { // no bar at the time exit: close at the last bar
				for _, l := range legs {
					if l.Instrument.Key() == tr.Instrument.Key() {
						last := l.Bars[len(l.Bars)-1]
						tr.Open, tr.ExitTime, tr.Exit, tr.Reason = false, last.Time, last.Close, "last bar"
					}
				}
			}
			n1, err := net(tr.Instrument, tr.Qty, tr.Entry, tr.Exit, tr.EntryTime, tr.ExitTime)
			if err != nil {
				return err
			}
			n2, err := net(tr.Instrument, tr.Qty, slip(tr.Entry, true), slip(tr.Exit, false), tr.EntryTime, tr.ExitTime)
			if err != nil {
				return err
			}
			s := &stats[vi]
			s.n++
			s.net += n1
			s.slip += n2
			if n1 > 0 {
				s.wins++
				s.gross += n1
			} else {
				s.loss -= n1
			}
			s.cum += n1
			s.peak = max(s.peak, s.cum)
			s.dd = max(s.dd, s.peak-s.cum)
			s.months[d.String()[:7]] += n1
			s.reasons[tr.Reason]++
			s.nets = append(s.nets, n1)
			if d.Before(half) {
				s.h1 += n1
			} else {
				s.h2 += n1
			}
			if tr.DayLowStop {
				s.dayLow++
			}
			if tr.EntryTime.Before(d.At(9, 30)) {
				s.early++
				s.earlyPL += n1
			}
			w.Write([]string{v.name, d.String(), tr.Instrument.Key(), tr.Armed.Format("15:04"), tr.EntryTime.Format("15:04"),
				fmt.Sprint(tr.Entry), fmt.Sprint(tr.Qty), fmt.Sprint(tr.Stop), tr.ExitTime.Format("15:04"), fmt.Sprint(tr.Exit),
				tr.Reason, fmt.Sprintf("%.0f", n1), fmt.Sprintf("%.0f", n2)})
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}

	fmt.Printf("%s %s..%s: %d days replayed, %d skipped (no 09:15 bar or no CE/PE data); %dm bars, immediate %v\n",
		*underlying, f, t, traded, skipped, *tf, *immediate)
	if *grid {
		fmt.Printf("\n%-10s %6s %5s %10s %10s %5s %9s %9s %9s %10s %6s\n", "pair", "trades", "win%", "net", "at slip", "PF", "max DD", "H1", "H2", "ex best 5", "daylow")
		for vi, v := range variants {
			s := stats[vi]
			pf := math.Inf(1)
			if s.loss > 0 {
				pf = s.gross / s.loss
			}
			ns := append([]float64{}, s.nets...)
			sort.Float64s(ns)
			ex := s.net
			for i := len(ns) - 1; i >= 0 && i >= len(ns)-5; i-- {
				ex -= ns[i]
			}
			fmt.Printf("%-10s %6d %5.0f %10.0f %10.0f %5.2f %9.0f %9.0f %9.0f %10.0f %6d\n", v.name, s.n,
				100*float64(s.wins)/float64(max(s.n, 1)), s.net, s.slip, pf, s.dd, s.h1, s.h2, ex, s.dayLow)
		}
	}
	for vi, v := range variants {
		if *grid {
			break
		}
		s := stats[vi]
		pf := math.Inf(1)
		if s.loss > 0 {
			pf = s.gross / s.loss
		}
		fmt.Printf("\n%-16s trades %d  win %.0f%%  net ₹%.0f  at %.0f bps ₹%.0f  PF %.2f  avg ₹%.0f  max DD ₹%.0f  filled before 09:30: %.0f (₹%.0f)\n",
			v.name, s.n, 100*float64(s.wins)/float64(max(s.n, 1)), s.net, *slipBps, s.slip, pf, s.net/float64(max(s.n, 1)), s.dd, s.early, s.earlyPL)
		var ms []string
		for m := range s.months {
			ms = append(ms, m)
		}
		sort.Strings(ms)
		for _, m := range ms {
			fmt.Printf("  %s %9.0f\n", m, s.months[m])
		}
		fmt.Printf("  exits: %v\n", s.reasons)
	}
	var total market.Paise
	n := 0
	for _, s := range stats {
		total += market.Rupees(s.slip)
		n += s.n
	}
	err = analytics.AppendTrials(analytics.LedgerPath(*dir), []analytics.TrialRecord{{
		Time: time.Now(), Command: "research hmaretest", Strategy: paper.HMARetestName, Underlying: *underlying,
		Params: strategy.Params{"slip_bps": *slipBps, "step": *step, "tf": float64(*tf), "immediate": b2f(*immediate)}, From: f, To: t, Count: len(variants),
		Unsealed: *unseal, Net: total, Trades: n,
	}})
	if err != nil {
		return fmt.Errorf("trial ledger: %w", err)
	}
	fmt.Printf("\ntrades: %s (recorded in the trial ledger)\n", *out)
	return nil
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
