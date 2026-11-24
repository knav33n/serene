package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"serene/internal/analytics"
	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/indicator"
	"serene/internal/market"
	"serene/internal/mcx"
	"serene/internal/strategy"
)

// ma10Kinds are the averages tried for each line of the crossover.
var ma10Kinds = []struct {
	name string
	fn   func([]market.Bar, int) []float64
}{{"SMA", indicator.SMA}, {"EMA", indicator.EMA}, {"HMA", indicator.HMA}}

// ma10Pairs is the union of the EMA and Hull pairs of mcxema and mcxhma.
var ma10Pairs = [][2]int{{3, 8}, {5, 13}, {5, 20}, {8, 21}, {9, 21}, {12, 26}, {13, 34}, {16, 49}, {20, 50}, {21, 55}, {34, 89}}

type ma10Signal struct {
	fastK, slowK int // index into ma10Kinds
	fast, slow   int
	longOnly     bool
}

func (s ma10Signal) String() string {
	return fmt.Sprintf("%s/%s %d/%d %s", ma10Kinds[s.fastK].name, ma10Kinds[s.slowK].name, s.fast, s.slow,
		map[bool]string{false: "L+S", true: "long"}[s.longOnly])
}

// runMCXMA10 runs every SMA/EMA/HMA pairing of fast and slow line on 10-minute
// frames of the gold and silver futures and ranks them by consistency across
// halves and metals.
func runMCXMA10(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcxma10", flag.ContinueOnError)
	names := fs.String("names", "GOLDM,SILVERM", "commodities")
	froms := fs.String("from", "GOLDM=2026-06-15,SILVERM=2026-07-01", "first session traded per commodity")
	to := fs.String("to", "2026-11-23", "last session traded")
	splitS := fs.String("split", "2026-09-15", "halves split")
	frameS := fs.Duration("frame", 10*time.Minute, "frame")
	dir := fs.String("data", "data/mcx", "data directory")
	holdout := fs.String("holdout", "configs/holdout.yaml", "data held back from research")
	costsFile := fs.String("costs", "configs/costs_mcx.yaml", "charges schedule")
	slipTicks := fs.Int64("slip-ticks", 1, "ticks against every fill")
	detail := fs.String("detail", "", "only print the long/short breakdown of this variant, e.g. \"HMA/SMA 34/89 L+S\"")
	unseal := fs.Bool("unseal", false, "with --detail: run into the held-back data, once, for a pre-picked variant (recorded)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *unseal && *detail == "" {
		return fmt.Errorf("--unseal needs --detail: only a pre-picked variant may see the held-back data")
	}
	t, err := market.ParseDate(*to)
	if err != nil {
		return err
	}
	split, err := market.ParseDate(*splitS)
	if err != nil {
		return err
	}
	h, err := analytics.LoadHoldouts(*holdout)
	if err != nil {
		return err
	}
	sealed, err := h.Check(*dir, t, *unseal)
	if err != nil {
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
	fromOf := map[string]market.Date{}
	for _, kv := range strings.Split(*froms, ",") {
		k, v, _ := strings.Cut(kv, "=")
		d, err := market.ParseDate(v)
		if err != nil {
			return err
		}
		fromOf[k] = d
	}

	frame := *frameS
	var signals []ma10Signal
	for _, lo := range []bool{false, true} {
		for fk := range ma10Kinds {
			for sk := range ma10Kinds {
				for _, p := range ma10Pairs {
					signals = append(signals, ma10Signal{fastK: fk, slowK: sk, fast: p[0], slow: p[1], longOnly: lo})
				}
			}
		}
	}

	var runs []*metalsRun
	for _, name := range strings.Split(*names, ",") {
		spec, ok := mcx.Specs[name]
		if !ok {
			return fmt.Errorf("no contract spec for %s", name)
		}
		from, ok := fromOf[name]
		if !ok {
			return fmt.Errorf("no --from for %s", name)
		}
		days, err := store.Days(name, market.Date{Year: 2000, Month: 1, Day: 1}, t)
		if err != nil {
			return err
		}
		r := &metalsRun{name: name, spec: spec, from: from, to: t, split: split}
		for _, d := range days {
			if !d.Before(from) {
				r.sessions++
			}
		}
		bars, err := loadMCXBars(ctx, store, []string{name}, days[0], t)
		if err != nil {
			return err
		}
		r.minute = bars[name]
		r.ed = mcx.PrepareEMA(r.minute, frame)
		runs = append(runs, r)
	}

	eval := func(r *metalsRun, s ma10Signal, slip int64) (metalsResult, error) {
		c := mcx.EMAConfig{Name: r.name, Spec: r.spec, Frame: frame, Fast: s.fast, Slow: s.slow, LongOnly: s.longOnly,
			MA: ma10Kinds[s.fastK].fn, SlowMA: ma10Kinds[s.slowK].fn, Slip: slip, Costs: costs, From: r.from, To: r.to}
		ts, err := mcx.EMABacktest(r.ed, c)
		if err != nil {
			return metalsResult{}, err
		}
		sm := summarize(ts)
		m := metalsResult{n: sm.n, win: sm.win, pf: sm.pf, net: sm.net, dd: sm.dd}
		var px float64
		for _, tr := range ts {
			n := tr.Net.Rupees()
			if market.DateOf(tr.Entry).Before(r.split) {
				m.h1 += n
			} else {
				m.h2 += n
			}
			if tr.Side == market.Buy {
				m.long += n
			} else {
				m.shrt += n
			}
			px += tr.In.Rupees()
		}
		if r.notional == 0 && len(ts) > 0 {
			r.notional = px / float64(len(ts)) * float64(r.spec.Multiplier)
		}
		if r.notional > 0 {
			m.pct = m.net / r.notional * 100
		}
		m.both = m.h1 > 0 && m.h2 > 0
		return m, nil
	}

	if *detail != "" {
		for _, s := range signals {
			if s.String() != *detail {
				continue
			}
			fmt.Printf("%s, %s frames, %d tick slippage + charges\n", s, frameText(frame), *slipTicks)
			fmt.Printf("%-8s %-5s %6s %5s %6s %11s %11s %11s\n", "metal", "side", "trades", "won", "win%", "net ₹", "avg win ₹", "avg loss ₹")
			for _, r := range runs {
				c := mcx.EMAConfig{Name: r.name, Spec: r.spec, Frame: frame, Fast: s.fast, Slow: s.slow, LongOnly: s.longOnly,
					MA: ma10Kinds[s.fastK].fn, SlowMA: ma10Kinds[s.slowK].fn, Slip: *slipTicks, Costs: costs, From: r.from, To: r.to}
				ts, err := mcx.EMABacktest(r.ed, c)
				if err != nil {
					return err
				}
				for _, side := range []market.Side{market.Buy, market.Sell, 0} {
					var n, won int
					var net, gw, gl float64
					for _, tr := range ts {
						if side != 0 && tr.Side != side {
							continue
						}
						v := tr.Net.Rupees()
						n++
						net += v
						if v > 0 {
							won++
							gw += v
						} else {
							gl += v
						}
					}
					name := map[market.Side]string{market.Buy: "long", market.Sell: "short", 0: "all"}[side]
					fmt.Printf("%-8s %-5s %6d %5d %5.0f%% %11.0f %11.0f %11.0f\n", r.name, name, n, won,
						100*float64(won)/float64(max(n, 1)), net, gw/float64(max(won, 1)), gl/float64(max(n-won, 1)))
				}
				gapReport(r, ts)
				if sealed {
					var net market.Paise
					for _, tr := range ts {
						net += tr.Net
					}
					if err := analytics.AppendTrials(analytics.LedgerPath(*dir), []analytics.TrialRecord{{
						Time: time.Now(), Command: "research mcxma10 --unseal", Strategy: "mcx_ma_mix_cross " + s.String(), Underlying: r.name,
						Params: strategy.Params{"frame_min": frame.Minutes(), "slip_ticks": float64(*slipTicks)}, From: r.from, To: r.to,
						Count: 1, Net: net, Trades: len(ts)}}); err != nil {
						return fmt.Errorf("trial ledger: %w", err)
					}
				}
			}
			return nil
		}
		return fmt.Errorf("no variant %q", *detail)
	}

	grid := map[string][]metalsResult{}
	var ledger []analytics.TrialRecord
	for _, r := range runs {
		var net market.Paise
		total := 0
		for _, s := range signals {
			m, err := eval(r, s, *slipTicks)
			if err != nil {
				return err
			}
			grid[r.name] = append(grid[r.name], m)
			net += market.Paise(m.net * 100)
			total += m.n
		}
		ledger = append(ledger, analytics.TrialRecord{
			Time: time.Now(), Command: "research mcxma10", Strategy: "mcx_ma_mix_cross", Underlying: r.name,
			Params: strategy.Params{"frame_min": frame.Minutes(), "slip_ticks": float64(*slipTicks)}, From: r.from, To: r.to,
			Count: len(signals), Net: net, Trades: total,
		})
	}
	if err := analytics.AppendTrials(analytics.LedgerPath(*dir), ledger); err != nil {
		return fmt.Errorf("trial ledger: %w", err)
	}

	fmt.Printf("SMA/EMA/HMA crossovers on %s frames, every fast×slow average pairing, MCX futures (active contract), 1 lot, %d tick slippage + charges\n",
		frameText(frame), *slipTicks)
	fmt.Printf("always in (or flat when long-only); signal at frame close, fill next minute open; held overnight; out at roll\n")
	fmt.Printf("%d variants per metal = %d fast×slow average pairings × %d pairs × L+S/long. %% = net / one lot's average notional\n\n",
		len(signals), len(ma10Kinds)*len(ma10Kinds), len(ma10Pairs))
	fmt.Printf("%-10s %-10s %-10s %-10s %8s %12s\n", "metal", "from", "to", "split", "sessions", "lot notional")
	for _, r := range runs {
		fmt.Printf("%-10s %-10s %-10s %-10s %8d %12.0f\n", r.name, r.from, r.to, r.split, r.sessions, r.notional)
	}

	resHead := func(first string) {
		fmt.Printf("%-24s %6s %5s %5s %10s %7s %10s %10s %10s %10s %9s  %s\n", first, "trades", "win%", "PF", "net ₹", "%",
			"1st half ₹", "2nd half ₹", "long ₹", "short ₹", "max DD ₹", "both+")
	}
	resLine := func(label string, m metalsResult) {
		mark := ""
		if m.both {
			mark = "yes"
		}
		fmt.Printf("%-24s %6d %4.0f%% %5s %10.0f %6.2f%% %10.0f %10.0f %10.0f %10.0f %9.0f  %s\n", label, m.n, m.win*100, pf(m.pf),
			m.net, m.pct, m.h1, m.h2, m.long, m.shrt, m.dd, mark)
	}
	median := func(v []float64) float64 {
		if len(v) == 0 {
			return 0
		}
		v = append([]float64(nil), v...)
		sort.Float64s(v)
		return v[len(v)/2]
	}

	for _, r := range runs {
		res := grid[r.name]
		idx := make([]int, len(signals))
		for i := range idx {
			idx[i] = i
		}
		sort.Slice(idx, func(a, b int) bool { return res[idx[a]].net > res[idx[b]].net })
		pos, both := 0, 0
		for _, m := range res {
			if m.net > 0 {
				pos++
			}
			if m.both {
				both++
			}
		}
		fmt.Printf("\n== %s: %d of %d positive, %d positive in both halves; top 15\n", r.name, pos, len(res), both)
		resHead("variant")
		for _, i := range idx[:15] {
			resLine(signals[i].String(), res[i])
		}
		fmt.Printf("\nmedian %% by fast(row) × slow(col) average, L+S | long-only, and positive-in-both-halves count\n")
		fmt.Printf("%-6s", "")
		for _, k := range ma10Kinds {
			fmt.Printf(" %18s", k.name)
		}
		fmt.Println()
		for fk, fkind := range ma10Kinds {
			fmt.Printf("%-6s", fkind.name)
			for sk := range ma10Kinds {
				var ls, lo []float64
				bp := 0
				for i, s := range signals {
					if s.fastK != fk || s.slowK != sk {
						continue
					}
					if s.longOnly {
						lo = append(lo, res[i].pct)
					} else {
						ls = append(ls, res[i].pct)
					}
					if res[i].both {
						bp++
					}
				}
				fmt.Printf(" %6.2f|%5.2f %2d/%2d", median(ls), median(lo), bp, len(ls)+len(lo))
			}
			fmt.Println()
		}
		fmt.Printf("median %% by pair (all 9 pairings, L+S):")
		for _, p := range ma10Pairs {
			var v []float64
			for i, s := range signals {
				if s.fast == p[0] && s.slow == p[1] && !s.longOnly {
					v = append(v, res[i].pct)
				}
			}
			fmt.Printf("  %d/%d %.2f", p[0], p[1], median(v))
		}
		fmt.Println()
	}

	// Cross-metal: a variant has to hold on every metal.
	type cross struct {
		i        int
		both     int
		min, sum float64
	}
	var xs []cross
	for i := range signals {
		x := cross{i: i, min: 1e18}
		for _, r := range runs {
			m := grid[r.name][i]
			if m.both {
				x.both++
			}
			x.min = min(x.min, m.pct)
			x.sum += m.pct
		}
		xs = append(xs, x)
	}
	sort.Slice(xs, func(a, b int) bool {
		if xs[a].both != xs[b].both {
			return xs[a].both > xs[b].both
		}
		return xs[a].min > xs[b].min
	})
	all := 0
	for _, x := range xs {
		if x.both == len(runs) {
			all++
		}
	}
	fmt.Printf("\n== across metals: %d of %d variants positive in both halves on every metal; top 20 by worst-metal %%\n", all, len(signals))
	fmt.Printf("%-24s %5s %8s", "variant", "both+", "worst %")
	for _, r := range runs {
		fmt.Printf(" %9s %6s %9s %9s", r.name, "trades", "h1 ₹", "h2 ₹")
	}
	fmt.Printf(" %s\n", "3-tick % per metal")
	for _, x := range xs[:min(20, len(xs))] {
		fmt.Printf("%-24s %3d/%d %7.2f%%", signals[x.i], x.both, len(runs), x.min)
		for _, r := range runs {
			m := grid[r.name][x.i]
			fmt.Printf(" %8.2f%% %6d %9.0f %9.0f", m.pct, m.n, m.h1, m.h2)
		}
		for _, r := range runs {
			m, err := eval(r, signals[x.i], 3)
			if err != nil {
				return err
			}
			fmt.Printf(" %7.2f%%", m.pct)
		}
		fmt.Println()
	}
	fmt.Printf("\nfull rows of the top 3 across metals\n")
	for _, x := range xs[:min(3, len(xs))] {
		fmt.Printf("\n%s\n", signals[x.i])
		resHead("metal")
		for _, r := range runs {
			resLine(r.name, grid[r.name][x.i])
		}
	}
	return nil
}

// gapReport splits each trade's P&L into what it made across session gaps
// (last close of one session to first open of the contract's next) and the
// rest, by gap kind: a weeknight, a weekend (Fri→Mon) or a holiday (any
// other break of more than one calendar day).
func gapReport(r *metalsRun, ts []mcx.Trade) {
	type sess struct {
		day         market.Date
		first, last market.Bar
	}
	byKey := map[string][]sess{}
	for _, b := range r.minute {
		d := market.DateOf(b.Time)
		ss := byKey[b.Key]
		if n := len(ss); n > 0 && ss[n-1].day == d {
			ss[n-1].last = b
			continue
		}
		byKey[b.Key] = append(ss, sess{day: d, first: b, last: b})
	}
	kinds := []string{"weeknight", "weekend", "holiday"}
	type agg struct {
		gaps          int
		pnl, abs, big float64
		worst, best   float64
	}
	ag := map[string]*agg{}
	for _, k := range kinds {
		ag[k] = &agg{}
	}
	type tagg struct {
		n, won   int
		net, gap float64
	}
	held := map[bool]*tagg{false: {}, true: {}} // held over a weekend or holiday
	var total, gapTotal float64
	mult := float64(r.spec.Multiplier)
	for _, tr := range ts {
		sign := 1.0
		if tr.Side == market.Sell {
			sign = -1
		}
		long := false
		var tg float64
		ss := byKey[tr.Key]
		for i := 0; i+1 < len(ss); i++ {
			a, b := ss[i], ss[i+1]
			if !tr.Entry.Before(a.last.End) || !tr.Exit.After(b.first.Time) {
				continue
			}
			days := int(b.first.Time.In(market.IST).Truncate(24*time.Hour).Sub(a.last.Time.In(market.IST).Truncate(24*time.Hour)).Hours()/24 + 0.5)
			k := "weeknight"
			switch {
			case days > 1 && a.last.Time.In(market.IST).Weekday() == time.Friday && days == 3:
				k = "weekend"
			case days > 1:
				k = "holiday"
			}
			if k != "weeknight" {
				long = true
			}
			v := sign * (b.first.Open - a.last.Close).Rupees() * mult
			g := ag[k]
			g.gaps++
			g.pnl += v
			g.abs += math.Abs(v)
			g.worst = math.Min(g.worst, v)
			g.best = math.Max(g.best, v)
			tg += v
		}
		n := tr.Net.Rupees()
		total += n
		gapTotal += tg
		h := held[long]
		h.n++
		h.net += n
		h.gap += tg
		if n > 0 {
			h.won++
		}
	}
	fmt.Printf("  gaps held (close→next open, signed for the side, 1 lot):\n")
	fmt.Printf("  %-10s %5s %11s %11s %11s %11s\n", "gap", "count", "net ₹", "avg |gap| ₹", "worst ₹", "best ₹")
	for _, k := range kinds {
		g := ag[k]
		fmt.Printf("  %-10s %5d %11.0f %11.0f %11.0f %11.0f\n", k, g.gaps, g.pnl, g.abs/float64(max(g.gaps, 1)), g.worst, g.best)
	}
	fmt.Printf("  all gaps: ₹%.0f of the ₹%.0f net; P&L made while trading: ₹%.0f\n", gapTotal, total, total-gapTotal)
	for _, k := range []bool{true, false} {
		h := held[k]
		fmt.Printf("  trades %-28s %4d  won %3d (%3.0f%%)  net ₹%9.0f  of which gaps ₹%9.0f\n",
			map[bool]string{true: "held over weekend/holiday", false: "not held over one"}[k], h.n, h.won,
			100*float64(h.won)/float64(max(h.n, 1)), h.net, h.gap)
	}
}
