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
	"time"

	"serene/internal/analytics"
	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/indicator"
	"serene/internal/market"
	"serene/internal/swing"
)

// gridCell is one combination of the choices besides the exit rule.
type gridCell struct {
	tf     market.Timeframe
	after  bool // entries from 10:00
	perDay bool // first trade a day only
	fade   bool // buy the other option
}

func (c gridCell) String() string {
	s := fmt.Sprintf("%2dm", int(c.tf))
	s += map[bool]string{false: "  all day", true: "  10:00+ "}[c.after]
	s += map[bool]string{false: "  every", true: "  1/day"}[c.perDay]
	s += map[bool]string{false: "  breakout", true: "  fade    "}[c.fade]
	return s
}

type gridStats struct {
	n         int
	win       float64 // percent
	net, pf   float64
	avgPct    float64
	maxDD     float64
	h1n, h2n  int
	h1, h2    float64 // net rupees
	h1p, h2p  float64 // profit factor
	firstHalf bool
}

func stats(ts []stopTrade) (n int, win, net, pf, avgPct, maxDD float64) {
	var w, l, peak, sumPct float64
	wins := 0
	for _, s := range ts {
		sumPct += s.t.PnLPct()
		if s.net > 0 {
			wins++
			w += s.net
		} else {
			l -= s.net
		}
		net += s.net
		peak = max(peak, net)
		maxDD = max(maxDD, peak-net)
	}
	n = len(ts)
	pf = math.Inf(1)
	if l > 0 {
		pf = w / l
	}
	if n > 0 {
		win = float64(wins) / float64(n) * 100
		avgPct = sumPct / float64(n)
	}
	return
}

// resample turns 1-minute session bars of one instrument into tf bars.
func resample(bars []market.Bar, tf market.Timeframe) []market.Bar {
	if tf == market.Minute1 || len(bars) == 0 {
		return bars
	}
	r, _ := market.NewResampler(market.NSESession, tf)
	var out []market.Bar
	for _, b := range bars {
		done, err := r.Add(b)
		if err != nil {
			continue
		}
		out = append(out, done...)
	}
	if b, ok := r.FlushUntil(bars[len(bars)-1].End.Add(24 * time.Hour)); ok {
		out = append(out, b)
	}
	return out
}

// runGrid replays every combination of timeframe, entry window, trades per
// day, side and exit over stored Upstox history.
func runGrid(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("grid", flag.ContinueOnError)
	underlying := fs.String("underlying", "NIFTY", "underlying name")
	from := fs.String("from", "2024-12-27", "first day")
	to := fs.String("to", "2026-04-30", "last day")
	split := fs.String("split", "2025-09-01", "first day of the second half, which picks are checked on")
	dir := fs.String("data", "data/upstox", "data directory")
	holdout := fs.String("holdout", "configs/holdout.yaml", "data held back from research")
	costsFile := fs.String("costs", "configs/costs.yaml", "charges schedule")
	out := fs.String("out", "reports/grid.csv", "every combination's results")
	tradesOut := fs.String("trades", "", "also write every trade of every combination to this CSV")
	step := fs.Float64("step", 100, "strike rounding of the day's open")
	k := fs.Int("k", 2, "bars on each side of a swing high")
	pullback := fs.Float64("min-pullback", 10, "percent pullback a break needs")
	emaN := fs.Int("ema", 21, "entry close above this EMA")
	exitK := fs.Int("exit-k", 1, "bars on each side of a higher low for the trailing stop")
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
	mid, err := market.ParseDate(*split)
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
	insts, err := store.Contracts(ctx, *underlying)
	if err != nil {
		return err
	}
	lots := map[string]market.Instrument{}
	for _, in := range insts {
		lots[in.Key()] = in
	}
	days, err := store.Days(*underlying, f, t)
	if err != nil {
		return err
	}

	tfs := []market.Timeframe{1, 3, 5, 15}
	rules := append(append([]stopRule(nil), stopRules...), scalpRules...)
	var cells []gridCell
	for _, tf := range tfs {
		for _, after := range []bool{false, true} {
			for _, perDay := range []bool{false, true} {
				for _, fade := range []bool{false, true} {
					cells = append(cells, gridCell{tf, after, perDay, fade})
				}
			}
		}
	}
	cellIndex := map[gridCell]int{}
	for i, c := range cells {
		cellIndex[c] = i
	}
	results := make([][][]stopTrade, len(cells))
	for i := range results {
		results[i] = make([][]stopTrade, len(rules))
	}

	flat := 15*time.Hour + 15*time.Minute
	ten := 10 * time.Hour
	indexKey := market.Index(market.NSE, *underlying).Key()
	var prev map[string][]market.Bar
	start := time.Now()
	for di, d := range days {
		all, err := store.DayBars(ctx, *underlying, d)
		if err != nil {
			return err
		}
		byKey := map[string][]market.Bar{}
		for _, b := range all {
			byKey[b.Key] = append(byKey[b.Key], b)
		}
		idx := byKey[indexKey]
		if len(idx) == 0 {
			prev = byKey
			continue
		}
		strike := market.Rupees(math.Round(idx[0].Open.Rupees() / *step) * *step)
		exp, ok := nearestExpiry(lots, byKey, d, strike, *underlying)
		if !ok {
			prev = byKey
			continue
		}
		keys := map[market.Right]string{}
		for _, r := range []market.Right{market.Call, market.Put} {
			keys[r] = market.Option(market.NFO, *underlying, exp, strike, r, 0).Key()
		}
		for _, tf := range tfs {
			bars := map[market.Right][]market.Bar{}
			warm := map[market.Right][]market.Bar{}
			for r, key := range keys {
				bars[r] = resample(byKey[key], tf)
				warm[r] = resample(prev[key], tf)
			}
			for _, after := range []bool{false, true} {
				// day[fade][rule] collects both rights' trades before the per-day cut.
				var day [2][][]stopTrade
				for i := range day {
					day[i] = make([][]stopTrade, len(rules))
				}
				for _, right := range []market.Right{market.Call, market.Put} {
					b := bars[right]
					if len(b) < 30/int(tf)+5 {
						continue
					}
					series := append(append([]market.Bar(nil), warm[right]...), b...)
					fl := filter{emaN: *emaN, ema: indicator.EMA(series, *emaN)[len(warm[right]):], vwap: indicator.VWAP(b)}
					atr := indicator.ATR(series, 14)[len(warm[right]):]
					x := swing.Exit{TrailK: *exitK, FlatAt: flat}
					if after {
						x.EntryFrom = ten
					}
					other := opposite(right)
					for ri, rule := range rules {
						x.Initial, x.TargetPct, x.MaxHold = rule.initial(b, atr), rule.target, rule.hold
						for _, tr := range swing.Trades(b, *k, *pullback, fl.ok(b), x) {
							net, err := netRupees(costs, lots[keys[right]], tr)
							if err != nil {
								return err
							}
							day[0][ri] = append(day[0][ri], stopTrade{d, right, tr, net})
							if m, ok := mirror(tr, bars[other]); ok {
								net, err := netRupees(costs, lots[keys[other]], m)
								if err != nil {
									return err
								}
								day[1][ri] = append(day[1][ri], stopTrade{d, other, m, net})
							}
						}
					}
				}
				for fi, fade := range []bool{false, true} {
					for ri := range rules {
						ts := day[fi][ri]
						sort.SliceStable(ts, func(i, j int) bool { return ts[i].t.Entry.Bar.Time.Before(ts[j].t.Entry.Bar.Time) })
						for _, perDay := range []bool{false, true} {
							keep := ts
							if perDay && len(keep) > 1 {
								keep = keep[:1]
							}
							c := cellIndex[gridCell{tf, after, perDay, fade}]
							results[c][ri] = append(results[c][ri], keep...)
						}
					}
				}
			}
		}
		prev = byKey
		if (di+1)%50 == 0 {
			fmt.Fprintf(os.Stderr, "  %d/%d days, %s\n", di+1, len(days), time.Since(start).Round(time.Second))
		}
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
	w.Write([]string{"timeframe", "entries", "per_day", "side", "exit", "trades", "win_pct", "net_rs", "pf", "avg_pct", "max_dd_rs",
		"h1_trades", "h1_net_rs", "h1_pf", "h2_trades", "h2_net_rs", "h2_pf"})

	fmt.Printf("NIFTY %s to %s, %d days. First half to %s picks each row's best exit; second half checks it.\n",
		f, t, len(days), mid.AddDays(-1))
	fmt.Printf("One lot per trade, after charges, no slippage. Base exit: pullback-low stop + trail.\n\n")
	fmt.Printf("  %-34s | %-19s | %-34s %-19s %-19s\n", "", "base exit, full", "best exit on first half", "first half", "second half")
	fmt.Printf("  %-34s | %6s %5s %6s | %-34s %6s %5s %6s %6s %5s %6s\n",
		"tf  entries   trades/day side", "trades", "PF", "net", "", "trades", "PF", "net", "trades", "PF", "net")
	fmt.Printf("  %-34s | %6s %5s %6s | %-34s %6s %5s %6s %6s %5s %6s\n",
		"", "", "", "Rs", "", "", "", "Rs", "", "", "Rs")
	for ci, c := range cells {
		best, bestNet := -1, math.Inf(-1)
		var rows [][2]gridStats
		for ri, rule := range rules {
			ts := results[ci][ri]
			var a, b []stopTrade
			for _, s := range ts {
				if s.day.Before(mid) {
					a = append(a, s)
				} else {
					b = append(b, s)
				}
			}
			n, win, net, pf, avgPct, dd := stats(ts)
			an, _, anet, apf, _, _ := stats(a)
			bn, _, bnet, bpf, _, _ := stats(b)
			rows = append(rows, [2]gridStats{{n: n, net: net, pf: pf}, {h1n: an, h1: anet, h1p: apf, h2n: bn, h2: bnet, h2p: bpf}})
			side := map[bool]string{false: "breakout", true: "fade"}[c.fade]
			entries := map[bool]string{false: "all day", true: "from 10:00"}[c.after]
			perDay := map[bool]string{false: "every", true: "first"}[c.perDay]
			w.Write([]string{strconv.Itoa(int(c.tf)), entries, perDay, side, rule.name, strconv.Itoa(n),
				fmt.Sprintf("%.1f", win), fmt.Sprintf("%.0f", net), fmt.Sprintf("%.3f", pf), fmt.Sprintf("%.2f", avgPct), fmt.Sprintf("%.0f", dd),
				strconv.Itoa(an), fmt.Sprintf("%.0f", anet), fmt.Sprintf("%.3f", apf),
				strconv.Itoa(bn), fmt.Sprintf("%.0f", bnet), fmt.Sprintf("%.3f", bpf)})
			if an >= 20 && anet > bestNet {
				best, bestNet = ri, anet
			}
		}
		base := rows[1][0] // "pullback low"
		fmt.Printf("  %-34s | %6d %5.2f %6.0f | ", c, base.n, base.pf, base.net)
		if best < 0 {
			fmt.Println("too few trades")
			continue
		}
		r := rows[best][1]
		fmt.Printf("%-34s %6d %5.2f %6.0f %6d %5.2f %6.0f\n", rules[best].name, r.h1n, r.h1p, r.h1, r.h2n, r.h2p, r.h2)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	if *tradesOut != "" {
		if err := writeGridTrades(*tradesOut, cells, rules, results, lots); err != nil {
			return err
		}
		fmt.Printf("every trade: %s\n", *tradesOut)
	}
	fmt.Printf("\nall %d combinations: %s\n", len(cells)*len(rules), *out)
	return nil
}

// writeGridTrades writes one row per trade, so sizing and account curves can
// be studied outside this command.
func writeGridTrades(path string, cells []gridCell, rules []stopRule, results [][][]stopTrade, lots map[string]market.Instrument) error {
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	w := csv.NewWriter(fh)
	w.Write([]string{"timeframe", "entries", "per_day", "side", "exit", "day", "right", "entry_time", "entry_price",
		"exit_time", "exit_price", "reason", "lot_size", "net_1lot_rs"})
	for ci, c := range cells {
		side := map[bool]string{false: "breakout", true: "fade"}[c.fade]
		entries := map[bool]string{false: "all day", true: "from 10:00"}[c.after]
		perDay := map[bool]string{false: "every", true: "first"}[c.perDay]
		for ri, rule := range rules {
			for _, s := range results[ci][ri] {
				lot := lots[s.t.Entry.Bar.Key].LotSize
				w.Write([]string{strconv.Itoa(int(c.tf)), entries, perDay, side, rule.name, s.day.String(), string(s.right),
					s.t.Entry.Bar.End.Format("15:04"), s.t.EntryPrice.String(), s.t.Exit.End.Format("15:04"), s.t.ExitPrice.String(),
					s.t.Reason.String(), strconv.FormatInt(lot, 10), fmt.Sprintf("%.2f", s.net)})
			}
		}
	}
	w.Flush()
	return w.Error()
}
