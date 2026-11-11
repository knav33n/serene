package main

import (
	"context"
	"flag"
	"fmt"
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

// metalsThin trade too little to fill a crossover cleanly; shown for reference.
var metalsThin = map[string]bool{"LEAD": true, "LEADMINI": true, "NICKEL": true}

// metalsSignal is one moving-average rule of the base-metal grid.
type metalsSignal struct {
	ma         string // EMA or HMA
	frame      time.Duration
	label      string
	fast, slow int
	slope      bool
	longOnly   bool
}

func (s metalsSignal) String() string {
	return fmt.Sprintf("%s %-3s %-7s %-4s", s.ma, frameText(s.frame), s.label, map[bool]string{false: "L+S", true: "long"}[s.longOnly])
}

func (s metalsSignal) config(name string, spec mcx.Spec, slip int64, costs *cost.Schedule, f, t market.Date) mcx.EMAConfig {
	c := mcx.EMAConfig{Name: name, Spec: spec, Frame: s.frame, Fast: s.fast, Slow: s.slow, Slope: s.slope,
		LongOnly: s.longOnly, Slip: slip, Costs: costs, From: f, To: t}
	if s.ma == "HMA" {
		c.MA = indicator.HMA
	}
	return c
}

// metalsResult is one signal on one metal.
type metalsResult struct {
	n                  int
	win, pf, net, dd   float64
	h1, h2, long, shrt float64
	pct                float64 // net as a percent of one lot's average notional
	both               bool
}

// metalsRun is one metal's prepared data and window.
type metalsRun struct {
	name     string
	spec     mcx.Spec
	ed       *mcx.EMAData
	from, to market.Date
	split    market.Date
	notional float64 // rupees, one lot at the window's average close
	sessions int
	minute   []market.Bar
}

// runMCXMetals runs the EMA and Hull crossover grids of mcxema and mcxhma on
// the MCX base metals' futures, applies the two frozen GOLDM rules to them
// unchanged, and counts which rules hold up across metals.
func runMCXMetals(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcxmetals", flag.ContinueOnError)
	names := fs.String("names", "COPPER,ZINCMINI,ALUMINI,ZINC,ALUMINIUM,LEAD,LEADMINI,NICKEL", "commodities")
	fromS := fs.String("from", "", "first session traded (default: each metal's first bar day plus --warmup sessions)")
	warmup := fs.Int("warmup", 10, "sessions of bars before the first traded one")
	to := fs.String("to", "2026-11-10", "last session traded")
	splitS := fs.String("split", "", "halves split (default: each metal's middle traded session)")
	dir := fs.String("data", "data/mcx", "data directory")
	holdout := fs.String("holdout", "configs/holdout.yaml", "data held back from research")
	costsFile := fs.String("costs", "configs/costs_mcx.yaml", "charges schedule")
	slipTicks := fs.Int64("slip-ticks", 1, "ticks against every fill")
	coreS := fs.String("core", "COPPER,ZINCMINI,ALUMINI", "metals counted for cross-metal consistency, one contract per metal")
	if err := fs.Parse(args); err != nil {
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

	frames := []time.Duration{5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour}
	var signals []metalsSignal
	for _, fr := range frames {
		for _, lo := range []bool{false, true} {
			for _, p := range emaPairs {
				signals = append(signals, metalsSignal{ma: "EMA", frame: fr, label: fmt.Sprintf("%d/%d", p[0], p[1]), fast: p[0], slow: p[1], longOnly: lo})
			}
			for _, p := range hmaPairs {
				signals = append(signals, metalsSignal{ma: "HMA", frame: fr, label: fmt.Sprintf("%d/%d", p[0], p[1]), fast: p[0], slow: p[1], longOnly: lo})
			}
			for _, n := range hmaSlopes {
				signals = append(signals, metalsSignal{ma: "HMA", frame: fr, label: fmt.Sprintf("turn %d", n), fast: n, slope: true, longOnly: lo})
			}
		}
	}
	frozen := []struct {
		name string
		sig  metalsSignal
	}{
		{"goldm_ema_15m", metalsSignal{ma: "EMA", frame: 15 * time.Minute, label: "13/34", fast: 13, slow: 34}},
		{"goldm_hma_1h", metalsSignal{ma: "HMA", frame: time.Hour, label: "turn 21", fast: 21, slope: true}},
	}

	var runs []*metalsRun
	for _, name := range strings.Split(*names, ",") {
		spec, ok := mcx.Specs[name]
		if !ok {
			return fmt.Errorf("no contract spec for %s", name)
		}
		days, err := store.Days(name, market.Date{Year: 2000, Month: 1, Day: 1}, t)
		if err != nil {
			return err
		}
		if len(days) <= *warmup+10 {
			return fmt.Errorf("%s: only %d sessions of bars", name, len(days))
		}
		r := &metalsRun{name: name, spec: spec, to: t, from: days[*warmup]}
		if *fromS != "" {
			if r.from, err = market.ParseDate(*fromS); err != nil {
				return err
			}
		}
		var traded []market.Date
		for _, d := range days {
			if !d.Before(r.from) {
				traded = append(traded, d)
			}
		}
		r.sessions = len(traded)
		r.split = traded[len(traded)/2]
		if *splitS != "" {
			if r.split, err = market.ParseDate(*splitS); err != nil {
				return err
			}
		}
		bars, err := loadMCXBars(ctx, store, []string{name}, days[0], t)
		if err != nil {
			return err
		}
		r.minute = bars[name]
		r.ed = mcx.PrepareEMA(r.minute, frames...)
		runs = append(runs, r)
	}

	eval := func(r *metalsRun, s metalsSignal, slip int64) (metalsResult, error) {
		ts, err := mcx.EMABacktest(r.ed, s.config(r.name, r.spec, slip, costs, r.from, r.to))
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

	// Fix each metal's notional from its first frozen rule's entries so every
	// variant on that metal is scaled the same.
	frozenRes := map[string][]metalsResult{}
	for _, r := range runs {
		for _, fz := range frozen {
			m, err := eval(r, fz.sig, *slipTicks)
			if err != nil {
				return err
			}
			frozenRes[r.name] = append(frozenRes[r.name], m)
		}
	}

	grid := map[string][]metalsResult{} // metal → per signal
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
			Time: time.Now(), Command: "research mcxmetals", Strategy: "mcx_metals_ma_cross", Underlying: r.name,
			Params: strategy.Params{"grid": 1, "slip_ticks": float64(*slipTicks)}, From: r.from, To: r.to,
			Count: len(signals), Net: net, Trades: total,
		})
	}

	fmt.Printf("EMA and Hull MA crossovers on MCX futures (active contract, minute bars), 1 lot, %d tick slippage + charges\n", *slipTicks)
	fmt.Printf("signal at frame close, fill next minute open; held overnight; out at roll. frames of 1h+ count from 09:00\n")
	fmt.Printf("futures only: no MCX option bars are stored, Kite keeps no candles for expired contracts, and MCX options outside crude and gold trade thinly\n")
	fmt.Printf("%% = net as a percent of one lot's average notional. thin = illiquid, reference only\n\n")
	fmt.Printf("%-10s %-10s %-10s %-10s %8s %12s\n", "metal", "from", "to", "split", "sessions", "lot notional")
	for _, r := range runs {
		fmt.Printf("%-10s %-10s %-10s %-10s %8d %12.0f %s\n", r.name, r.from, r.to, r.split, r.sessions, r.notional, thinMark(r.name))
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

	fmt.Printf("\n== frozen GOLDM rules, applied unchanged\n")
	for i, fz := range frozen {
		fmt.Printf("\n%s (%s)\n", fz.name, fz.sig)
		resHead("metal")
		for _, r := range runs {
			resLine(r.name+" "+thinMark(r.name), frozenRes[r.name][i])
		}
	}

	fmt.Printf("\n== top 10 of %d variants per metal, and by frame (L+S only: positive / both halves positive / median net ₹)\n", len(signals))
	for _, r := range runs {
		idx := make([]int, len(signals))
		for i := range idx {
			idx[i] = i
		}
		res := grid[r.name]
		sort.Slice(idx, func(a, b int) bool { return res[idx[a]].net > res[idx[b]].net })
		both := 0
		for _, m := range res {
			if m.both {
				both++
			}
		}
		fmt.Printf("\n%s %s: %d of %d variants positive in both halves\n", r.name, thinMark(r.name), both, len(res))
		resHead("variant")
		for _, i := range idx[:10] {
			resLine(signals[i].String(), res[i])
		}
		for _, maName := range []string{"EMA", "HMA"} {
			fmt.Printf("  %s", maName)
			for _, fr := range frames {
				var nets []float64
				pos, bp := 0, 0
				for i, s := range signals {
					if s.ma != maName || s.frame != fr || s.longOnly {
						continue
					}
					nets = append(nets, res[i].net)
					if res[i].net > 0 {
						pos++
					}
					if res[i].both {
						bp++
					}
				}
				sort.Float64s(nets)
				fmt.Printf("  %s %d/%d/%d %.0f", frameText(fr), pos, bp, len(nets), nets[len(nets)/2])
			}
			fmt.Println()
		}
	}

	metalsCore := strings.Split(*coreS, ",")
	var core []*metalsRun
	for _, r := range runs {
		for _, c := range metalsCore {
			if r.name == c {
				core = append(core, r)
			}
		}
	}
	type cross struct {
		i         int
		both, pos int
		med       float64
	}
	var xs []cross
	for i := range signals {
		x := cross{i: i}
		var pcts []float64
		for _, r := range core {
			m := grid[r.name][i]
			if m.both {
				x.both++
			}
			if m.net > 0 {
				x.pos++
			}
			pcts = append(pcts, m.pct)
		}
		if len(pcts) > 0 {
			sort.Float64s(pcts)
			x.med = pcts[len(pcts)/2]
		}
		xs = append(xs, x)
	}
	sort.Slice(xs, func(a, b int) bool {
		if xs[a].both != xs[b].both {
			return xs[a].both > xs[b].both
		}
		return xs[a].med > xs[b].med
	})
	fmt.Printf("\n== cross-metal consistency on %s: metals positive in both halves, then median %%; %% per metal\n", strings.Join(metalsCore, ", "))
	dist := map[int]int{}
	for _, x := range xs {
		dist[x.both]++
	}
	fmt.Printf("variants by number of core metals positive in both halves:")
	for k := len(core); k >= 0; k-- {
		fmt.Printf("  %d: %d", k, dist[k])
	}
	fmt.Println()
	fmt.Printf("%-24s %5s %5s %7s", "variant", "both+", "net+", "med %")
	for _, r := range runs {
		fmt.Printf(" %9s", r.name)
	}
	fmt.Println()
	top := xs[:min(25, len(xs))]
	for _, x := range top {
		fmt.Printf("%-24s %3d/%d %3d/%d %6.2f%%", signals[x.i], x.both, len(core), x.pos, len(core), x.med)
		for _, r := range runs {
			fmt.Printf(" %8.2f%%", grid[r.name][x.i].pct)
		}
		fmt.Println()
	}

	if *slipTicks < 3 {
		fmt.Printf("\n== top 10 consistent variants at 3 ticks: %% per metal\n")
		fmt.Printf("%-24s", "variant")
		for _, r := range runs {
			fmt.Printf(" %9s", r.name)
		}
		fmt.Println()
		for _, x := range top[:min(10, len(top))] {
			fmt.Printf("%-24s", signals[x.i])
			for _, r := range runs {
				m, err := eval(r, signals[x.i], 3)
				if err != nil {
					return err
				}
				fmt.Printf(" %8.2f%%", m.pct)
			}
			fmt.Println()
		}
	}

	if err := analytics.AppendTrials(analytics.LedgerPath(*dir), ledger); err != nil {
		return fmt.Errorf("trial ledger: %w", err)
	}

	fmt.Printf("\n== continuous daily (not back-adjusted, rolls favour longs): best pair against buy and hold, 1 lot\n")
	fmt.Printf("%-10s %-23s %12s %-4s %-16s %12s %12s %12s\n", "metal", "span", "buy&hold ₹", "MA", "best", "net ₹", "1st half ₹", "2nd half ₹")
	for _, r := range runs {
		for _, ma := range []struct {
			name  string
			fn    func([]market.Bar, int) []float64
			pairs [][2]int
		}{{"EMA", indicator.EMA, emaPairs}, {"HMA", indicator.HMA, hmaPairs}} {
			d, err := dailyCross(ctx, store, r.name, r.spec, costs, r.minute, t, ma.pairs, ma.fn)
			if err != nil {
				fmt.Printf("%-10s %s\n", r.name, err)
				break
			}
			b := d.rows[0]
			fmt.Printf("%-10s %-23s %12.0f %-4s %-16s %12.0f %12.0f %12.0f\n", r.name, d.first.String()+" "+d.last.String(), d.hold,
				ma.name, fmt.Sprintf("%s (%d)", b.label, b.n), b.net, b.h1, b.h2)
		}
	}
	return nil
}

func thinMark(name string) string {
	if metalsThin[name] {
		return "thin"
	}
	return ""
}
