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
	"strconv"
	"strings"
	"time"

	"serene/internal/analytics"
	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/market"
	"serene/internal/mcx"
	"serene/internal/strategy"
)

// runMCXDow backtests Dow structure on MCX futures: a limit at the retest of
// a broken swing, out when the swing on the other side breaks.
func runMCXDow(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcxdow", flag.ContinueOnError)
	names := fs.String("names", strings.Join(mcx.PulseNames, ","), "commodities")
	from := fs.String("from", "2026-01-01", "first session traded")
	to := fs.String("to", "2026-11-10", "last session traded")
	dir := fs.String("data", "data/mcx", "data directory")
	holdout := fs.String("holdout", "configs/holdout.yaml", "data held back from research")
	costsFile := fs.String("costs", "configs/costs_mcx.yaml", "charges schedule")
	frame := fs.Duration("frame", time.Hour, "swing bar length")
	lookback := fs.Duration("lookback", 30*24*time.Hour, "history the zigzag sees")
	multiple := fs.Float64("multiple", mcx.SwingMultiple, "reversal as a multiple of the median bar range")
	slipTicks := fs.Int64("slip-ticks", 1, "ticks against every fill")
	tradesOut := fs.String("trades", "reports/mcxdow_trades.csv", "every trade")
	grid := fs.Bool("grid", false, "run the variation grid instead: frame × multiple × entry × target")
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

	list := strings.Split(*names, ",")
	bars, err := loadMCXBars(ctx, store, list, f.AddDays(-int(lookback.Hours()/24)-5), t)
	if err != nil {
		return err
	}
	runAll := func(mult float64) ([]mcx.Trade, map[string]mcx.Orders, error) {
		var all []mcx.Trade
		orders := map[string]mcx.Orders{}
		for _, name := range list {
			cfg := mcx.BacktestConfig{Name: name, Spec: mcx.Specs[name], Frame: *frame, Lookback: *lookback,
				Multiple: mult, Slip: *slipTicks, Costs: costs, From: f, To: t}
			tr, st, err := mcx.Backtest(bars[name], cfg)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", name, err)
			}
			all = append(all, tr...)
			orders[name] = st
		}
		return all, orders, nil
	}
	if *grid {
		return mcxGrid(*dir, *names, list, bars, costs, *lookback, *slipTicks, f, t)
	}
	trades, orders, err := runAll(*multiple)
	if err != nil {
		return err
	}

	fmt.Printf("MCX Dow structure, %s swings, reversal %.1f× median range, %s lookback, %s to %s, 1 lot, %d tick slippage + Zerodha charges\n\n",
		*frame, *multiple, *lookback, f, t, *slipTicks)
	fmt.Printf("%-11s %-5s %-6s %6s %6s %6s %7s %12s %12s\n", "", "side", "kind", "trades", "win%", "PF", "avg R", "net ₹", "max DD ₹")
	row := func(name, side, kind string, ts []mcx.Trade) {
		s := summarize(ts)
		fmt.Printf("%-11s %-5s %-6s %6d %5.0f%% %6s %7.2f %12.0f %12.0f\n", name, side, kind, s.n, s.win*100, pf(s.pf), s.avgR, s.net, s.dd)
	}
	pick := func(ts []mcx.Trade, keep func(mcx.Trade) bool) []mcx.Trade {
		var out []mcx.Trade
		for _, tr := range ts {
			if keep(tr) {
				out = append(out, tr)
			}
		}
		return out
	}
	for _, name := range list {
		mine := pick(trades, func(tr mcx.Trade) bool { return tr.Name == name })
		for _, side := range []market.Side{market.Buy, market.Sell} {
			for _, kind := range []string{"BOS", "CHoCH", "NEW"} {
				ts := pick(mine, func(tr mcx.Trade) bool { return tr.Side == side && tr.Kind == kind })
				if len(ts) > 0 {
					row(name, sideName(side), kind, ts)
				}
			}
		}
		row(name, "all", "", mine)
		o := orders[name]
		fmt.Printf("%-11s orders %d placed, %d filled, %d pullback turned short, %d replaced, %d rolled\n\n", "", o.Placed, o.Filled, o.Ran, o.Replaced, o.Rolled)
	}
	for _, side := range []market.Side{market.Buy, market.Sell} {
		row("ALL", sideName(side), "", pick(trades, func(tr mcx.Trade) bool { return tr.Side == side }))
	}
	for _, kind := range []string{"BOS", "CHoCH", "NEW"} {
		row("ALL", "all", kind, pick(trades, func(tr mcx.Trade) bool { return tr.Kind == kind }))
	}
	row("ALL", "all", "", trades)

	fmt.Println("\nSensitivity, pooled (not a choice):")
	sens := []float64{2, 3}
	for _, m := range sens {
		ts, _, err := runAll(m)
		if err != nil {
			return err
		}
		row(fmt.Sprintf("mult %.1f", m), "all", "", ts)
	}

	if err := writeMCXTrades(*tradesOut, trades); err != nil {
		return err
	}
	var net market.Paise
	for _, tr := range trades {
		net += tr.Net
	}
	err = analytics.AppendTrials(analytics.LedgerPath(*dir), []analytics.TrialRecord{{
		Time: time.Now(), Command: "research mcxdow", Strategy: "mcx_dow", Underlying: *names,
		Params: strategy.Params{"frame_min": frame.Minutes(), "lookback_days": lookback.Hours() / 24, "multiple": *multiple, "slip_ticks": float64(*slipTicks)},
		From:   f, To: t, Count: 1 + len(sens), Net: net, Trades: len(trades),
	}})
	if err != nil {
		return fmt.Errorf("trial ledger: %w", err)
	}
	fmt.Printf("\ntrades: %s (recorded in the trial ledger)\n", *tradesOut)
	return nil
}

type tradeSummary struct {
	n             int
	win, pf, avgR float64
	net, dd       float64 // rupees
}

func summarize(ts []mcx.Trade) tradeSummary {
	s := tradeSummary{n: len(ts)}
	if len(ts) == 0 {
		return s
	}
	ts = append([]mcx.Trade(nil), ts...)
	sort.Slice(ts, func(i, j int) bool { return ts[i].Exit.Before(ts[j].Exit) })
	var wins, gw, gl, sumR, eq, peak float64
	for _, tr := range ts {
		n := tr.Net.Rupees()
		if n > 0 {
			wins++
			gw += n
		} else {
			gl -= n
		}
		sumR += tr.R
		eq += n
		peak = math.Max(peak, eq)
		s.dd = math.Max(s.dd, peak-eq)
	}
	s.win, s.avgR, s.net = wins/float64(len(ts)), sumR/float64(len(ts)), eq
	s.pf = math.Inf(1)
	if gl > 0 {
		s.pf = gw / gl
	}
	return s
}

func pf(v float64) string {
	if math.IsInf(v, 1) {
		return "∞"
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

func sideName(s market.Side) string {
	if s == market.Buy {
		return "long"
	}
	return "short"
}

func writeMCXTrades(path string, trades []mcx.Trade) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(fh)
	w.Write([]string{"name", "contract", "side", "kind", "signal", "level", "stop", "entry", "in", "exit", "out", "reason", "r", "gross", "net"})
	ts := func(t time.Time) string { return t.In(market.IST).Format("2006-01-02 15:04") }
	for _, tr := range trades {
		w.Write([]string{tr.Name, tr.Key, sideName(tr.Side), tr.Kind, ts(tr.Signal), mcx.Price(tr.Level), mcx.Price(tr.Stop),
			ts(tr.Entry), mcx.Price(tr.In), ts(tr.Exit), mcx.Price(tr.Out), tr.Reason,
			strconv.FormatFloat(tr.R, 'f', 2, 64), mcx.Price(tr.Gross), mcx.Price(tr.Net)})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		fh.Close()
		return err
	}
	return fh.Close()
}

// mcxGrid runs every variation on the same data and ranks them by average R.
// Rupees are left aside: GOLD's lot is worth so much that it swamps the rest.
func mcxGrid(dir, names string, list []string, bars map[string][]market.Bar, costs *cost.Schedule,
	lookback time.Duration, slip int64, f, t market.Date) error {
	split := market.Date{Year: 2026, Month: 9, Day: 1}
	type result struct {
		label             string
		n, pos            int
		win, sumR, h1, h2 float64
		n1, n2            int
		exGold            market.Paise
	}
	var res []result
	var net market.Paise
	trades := 0
	for _, frame := range []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour} {
		for _, mult := range []float64{2, 2.5, 3} {
			for _, mkt := range []bool{false, true} {
				for _, tgt := range []float64{0, 2} {
					entry, exit := "retest", "trail"
					if mkt {
						entry = "market"
					}
					if tgt > 0 {
						exit = "2R"
					}
					r := result{label: fmt.Sprintf("%-5s %4.1f  %-6s %-5s", strings.TrimSuffix(frame.String(), "0s"), mult, entry, exit)}
					for _, name := range list {
						cfg := mcx.BacktestConfig{Name: name, Spec: mcx.Specs[name], Frame: frame, Lookback: lookback,
							Multiple: mult, Slip: slip, Costs: costs, From: f, To: t, Market: mkt, TargetR: tgt}
						ts, _, err := mcx.Backtest(bars[name], cfg)
						if err != nil {
							return fmt.Errorf("%s %s: %w", r.label, name, err)
						}
						var sum float64
						for _, tr := range ts {
							sum += tr.R
							if tr.R > 0 {
								r.win++
							}
							if market.DateOf(tr.Entry).Before(split) {
								r.h1 += tr.R
								r.n1++
							} else {
								r.h2 += tr.R
								r.n2++
							}
							if name != "GOLD" {
								r.exGold += tr.Net
							}
							net += tr.Net
						}
						if sum > 0 {
							r.pos++
						}
						r.n += len(ts)
						r.sumR += sum
						trades += len(ts)
					}
					res = append(res, r)
				}
			}
		}
	}
	sort.Slice(res, func(i, j int) bool {
		return res[i].sumR/float64(max(res[i].n, 1)) > res[j].sumR/float64(max(res[j].n, 1))
	})
	avg := func(s float64, n int) float64 { return s / float64(max(n, 1)) }
	fmt.Printf("MCX Dow variations, %s to %s, %s lookback, 1 lot, %d tick slippage + charges; ranked by avg R\n", f, t, lookback, slip)
	fmt.Printf("halves split at %s (entries before / from)\n\n", split)
	fmt.Printf("%-5s %4s  %-6s %-5s %6s %5s %7s %7s %8s %8s %6s %12s\n", "frame", "mult", "entry", "exit", "trades", "win%", "avg R", "sum R", "R <Sep", "R ≥Sep", "+ of 7", "net ex-GOLD")
	for _, r := range res {
		fmt.Printf("%s %6d %4.0f%% %7.3f %7.1f %8.3f %8.3f %6d %12.0f\n", r.label, r.n, 100*r.win/float64(max(r.n, 1)),
			avg(r.sumR, r.n), r.sumR, avg(r.h1, r.n1), avg(r.h2, r.n2), r.pos, r.exGold.Rupees())
	}
	err := analytics.AppendTrials(analytics.LedgerPath(dir), []analytics.TrialRecord{{
		Time: time.Now(), Command: "research mcxdow --grid", Strategy: "mcx_dow", Underlying: names,
		Params: strategy.Params{"lookback_days": lookback.Hours() / 24, "slip_ticks": float64(slip)},
		From:   f, To: t, Count: len(res), Net: net, Trades: trades,
	}})
	if err != nil {
		return fmt.Errorf("trial ledger: %w", err)
	}
	fmt.Printf("\n%d variations recorded in the trial ledger as one record\n", len(res))
	return nil
}

// loadMCXBars reads every contract's minute bars for each commodity.
func loadMCXBars(ctx context.Context, store *data.Store, list []string, from, to market.Date) (map[string][]market.Bar, error) {
	bars := map[string][]market.Bar{}
	for _, name := range list {
		if _, ok := mcx.Specs[name]; !ok {
			return nil, fmt.Errorf("no contract spec for %s", name)
		}
		days, err := store.Days(name, from, to)
		if err != nil {
			return nil, err
		}
		for _, d := range days {
			b, err := store.DayBars(ctx, name, d)
			if err != nil {
				return nil, err
			}
			bars[name] = append(bars[name], b...)
		}
	}
	return bars, nil
}
