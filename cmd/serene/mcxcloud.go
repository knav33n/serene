package main

import (
	"context"
	"flag"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"serene/internal/analytics"
	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/market"
	"serene/internal/mcx"
	"serene/internal/strategy"
)

// runMCXCloud backtests the user's Trade Cloud indicator on MCX futures:
// enter on 1H when most of its seven readings agree, exit on 30m.
func runMCXCloud(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcxcloud", flag.ContinueOnError)
	names := fs.String("names", strings.Join(mcx.PulseNames, ","), "commodities")
	from := fs.String("from", "2026-01-01", "first session traded")
	to := fs.String("to", "2026-11-10", "last session traded")
	dir := fs.String("data", "data/mcx", "data directory")
	holdout := fs.String("holdout", "configs/holdout.yaml", "data held back from research")
	costsFile := fs.String("costs", "configs/costs_mcx.yaml", "charges schedule")
	n := fs.Int("n", 5, "readings of 7 that must agree")
	exit := fs.String("exit", mcx.ExitSwing30, "exit rule: "+strings.Join(mcx.ExitRules, ", "))
	nineK := fs.Float64("nine-k", 1, "skip 10:00 entries after a 9:00 hour beyond this × ATR14 their way (0 off)")
	lookback := fs.Duration("lookback", 30*24*time.Hour, "history the zigzag sees")
	multiple := fs.Float64("multiple", mcx.SwingMultiple, "swing reversal as a multiple of the median bar range")
	slipTicks := fs.Int64("slip-ticks", 1, "ticks against every fill")
	table := fs.String("table", "", "print the last 1H bar's readings for this commodity and stop")
	grid := fs.Bool("grid", false, "run N × exit × 9AM filter variations")
	tradesOut := fs.String("trades", "reports/mcxcloud_trades.csv", "every trade")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !slices.Contains(mcx.ExitRules, *exit) {
		return fmt.Errorf("unknown exit %q", *exit)
	}
	f, err := market.ParseDate(*from)
	if err != nil {
		return err
	}
	t, err := market.ParseDate(*to)
	if err != nil {
		return err
	}
	store, err := data.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()

	if *table != "" {
		bars, err := loadMCXBars(ctx, store, []string{*table}, market.DateOf(time.Now()).AddDays(-60), market.DateOf(time.Now()))
		if err != nil {
			return err
		}
		key, bar, r, ok := mcx.PrepareCloud(bars[*table]).LastReading()
		if !ok {
			return fmt.Errorf("no recent bars for %s", *table)
		}
		bull, bear := r.Count()
		fmt.Printf("%s 1H bar %s IST, close %s: %d bullish, %d bearish\n\n%s", key,
			bar.Time.In(market.IST).Format("Mon 2 Jan 15:04"), mcx.Price(bar.Close), bull, bear, mcx.FormatReading(r))
		return nil
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
	list := strings.Split(*names, ",")
	bars, err := loadMCXBars(ctx, store, list, f.AddDays(-int(lookback.Hours()/24)-5), t)
	if err != nil {
		return err
	}
	prep := map[string]*mcx.CloudData{}
	for _, name := range list {
		prep[name] = mcx.PrepareCloud(bars[name])
	}
	runAll := func(n int, exit string, nineK float64) ([]mcx.Trade, error) {
		var all []mcx.Trade
		for _, name := range list {
			cfg := mcx.CloudConfig{Name: name, Spec: mcx.Specs[name], N: n, Exit: exit, NineK: nineK,
				Lookback: *lookback, Multiple: *multiple, Slip: *slipTicks, Costs: costs, From: f, To: t}
			ts, err := mcx.CloudBacktest(prep[name], cfg)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			all = append(all, ts...)
		}
		return all, nil
	}
	record := func(count int, params strategy.Params, trades []mcx.Trade, total int, net market.Paise) error {
		err := analytics.AppendTrials(analytics.LedgerPath(*dir), []analytics.TrialRecord{{
			Time: time.Now(), Command: "research mcxcloud", Strategy: "mcx_cloud", Underlying: *names,
			Params: params, From: f, To: t, Count: count, Net: net, Trades: total,
		}})
		if err != nil {
			return fmt.Errorf("trial ledger: %w", err)
		}
		return nil
	}
	if *grid {
		return cloudGrid(list, runAll, f, t, *slipTicks, record)
	}

	trades, err := runAll(*n, *exit, *nineK)
	if err != nil {
		return err
	}
	fmt.Printf("MCX Trade Cloud: %d of 7 agree on 1H, exit %s on 30m, 9AM filter %s, %s to %s, 1 lot, %d tick slippage + charges\n\n",
		*n, *exit, nineText(*nineK), f, t, *slipTicks)
	pick := func(ts []mcx.Trade, keep func(mcx.Trade) bool) []mcx.Trade {
		var out []mcx.Trade
		for _, tr := range ts {
			if keep(tr) {
				out = append(out, tr)
			}
		}
		return out
	}
	fmt.Printf("%-11s %-11s %6s %6s %6s %7s %12s %12s\n", "", "kind", "trades", "win%", "PF", "avg R", "net ₹", "max DD ₹")
	row := func(name, kind string, ts []mcx.Trade) {
		s := summarize(ts)
		fmt.Printf("%-11s %-11s %6d %5.0f%% %6s %7.2f %12.0f %12.0f\n", name, kind, s.n, s.win*100, pf(s.pf), s.avgR, s.net, s.dd)
	}
	kinds := []string{"breakout", "support", "breakdown", "resistance"}
	for _, name := range list {
		mine := pick(trades, func(tr mcx.Trade) bool { return tr.Name == name })
		for _, k := range kinds {
			if ts := pick(mine, func(tr mcx.Trade) bool { return tr.Kind == k }); len(ts) > 0 {
				row(name, k, ts)
			}
		}
		row(name, "all", mine)
		fmt.Println()
	}
	for _, k := range kinds {
		row("ALL", k, pick(trades, func(tr mcx.Trade) bool { return tr.Kind == k }))
	}
	row("ALL", "10:00 entry", pick(trades, func(tr mcx.Trade) bool { return tr.Entry.In(market.IST).Hour() == 10 }))
	row("ALL", "all", trades)
	if err := writeMCXTrades(*tradesOut, trades); err != nil {
		return err
	}
	var net market.Paise
	for _, tr := range trades {
		net += tr.Net
	}
	if err := record(1, cloudParams(*n, *exit, *nineK, *lookback, *multiple, *slipTicks), trades, len(trades), net); err != nil {
		return err
	}
	fmt.Printf("\ntrades: %s (recorded in the trial ledger)\n", *tradesOut)
	return nil
}

func nineText(k float64) string {
	if k <= 0 {
		return "off"
	}
	return fmt.Sprintf("%.1f×ATR", k)
}

func cloudParams(n int, exit string, nineK float64, lookback time.Duration, mult float64, slip int64) strategy.Params {
	p := strategy.Params{"n": float64(n), "nine_k": nineK, "lookback_days": lookback.Hours() / 24,
		"multiple": mult, "slip_ticks": float64(slip)}
	p["exit_"+exit] = 1
	return p
}

// cloudGrid ranks every variation by average R; rupees are left aside since
// GOLD's lot swamps the rest.
func cloudGrid(list []string, runAll func(int, string, float64) ([]mcx.Trade, error), f, t market.Date, slip int64,
	record func(int, strategy.Params, []mcx.Trade, int, market.Paise) error) error {
	split := market.Date{Year: 2026, Month: 9, Day: 1}
	type agg struct {
		n int
		r float64
	}
	add := func(a *agg, r float64) { a.n++; a.r += r }
	avg := func(a agg) float64 {
		if a.n == 0 {
			return 0
		}
		return a.r / float64(a.n)
	}
	type result struct {
		label                      string
		all, h1, h2, brk, sup, t10 agg
		win, pos                   int
		exGold                     market.Paise
	}
	var res []result
	var net market.Paise
	total := 0
	for _, n := range []int{5, 6, 7} {
		for _, exit := range mcx.ExitRules {
			for _, k := range []float64{0, 1, 1.5} {
				ts, err := runAll(n, exit, k)
				if err != nil {
					return err
				}
				r := result{label: fmt.Sprintf("%d/7  %-7s %-7s", n, exit, nineText(k))}
				byName := map[string]float64{}
				for _, tr := range ts {
					add(&r.all, tr.R)
					if market.DateOf(tr.Entry).Before(split) {
						add(&r.h1, tr.R)
					} else {
						add(&r.h2, tr.R)
					}
					if tr.Kind == "breakout" || tr.Kind == "breakdown" {
						add(&r.brk, tr.R)
					} else {
						add(&r.sup, tr.R)
					}
					if tr.Entry.In(market.IST).Hour() == 10 {
						add(&r.t10, tr.R)
					}
					if tr.R > 0 {
						r.win++
					}
					if tr.Name != "GOLD" {
						r.exGold += tr.Net
					}
					byName[tr.Name] += tr.R
					net += tr.Net
				}
				for _, v := range byName {
					if v > 0 {
						r.pos++
					}
				}
				total += len(ts)
				res = append(res, r)
			}
		}
	}
	sort.Slice(res, func(i, j int) bool { return avg(res[i].all) > avg(res[j].all) })
	fmt.Printf("MCX Trade Cloud variations, %s to %s, 1 lot, %d tick slippage + charges; ranked by avg R\n", f, t, slip)
	fmt.Printf("entry on 1H, exit on 30m; halves split at %s; brk = breakout/breakdown, sup = support/resistance\n\n", split)
	fmt.Printf("%-4s %-7s %-7s %6s %5s %7s %7s %7s %7s %7s %7s %11s %6s %12s\n", "agree", "exit", "9AM", "trades", "win%",
		"avg R", "sum R", "R <Sep", "R ≥Sep", "brk R", "sup R", "10:00 R (n)", "+ of 7", "net ex-GOLD")
	for _, r := range res {
		fmt.Printf("%s %6d %4.0f%% %7.3f %7.1f %7.3f %7.3f %7.3f %7.3f %6.2f (%3d) %6d %12.0f\n", r.label, r.all.n,
			100*float64(r.win)/float64(max(r.all.n, 1)), avg(r.all), r.all.r, avg(r.h1), avg(r.h2), avg(r.brk), avg(r.sup),
			avg(r.t10), r.t10.n, r.pos, r.exGold.Rupees())
	}
	if err := record(len(res), strategy.Params{"grid": 1, "slip_ticks": float64(slip)}, nil, total, net); err != nil {
		return err
	}
	fmt.Printf("\n%d variations recorded in the trial ledger as one record\n", len(res))
	return nil
}
