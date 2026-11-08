package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"sort"
	"time"

	"calm/internal/analytics"
	"calm/internal/cost"
	"calm/internal/data"
	"calm/internal/indicator"
	"calm/internal/market"
	"calm/internal/swing"
)

// stopRule is a candidate initial stop. Every rule keeps the trailing stop
// under higher lows; only the starting stop differs.
type stopRule struct {
	name    string
	initial func(bars []market.Bar, atr []float64) func(swing.Break) market.Paise
	target  float64       // percent; 0 for none
	hold    time.Duration // 0 for none
}

func pctBelow(p float64) func([]market.Bar, []float64) func(swing.Break) market.Paise {
	return func([]market.Bar, []float64) func(swing.Break) market.Paise {
		return func(e swing.Break) market.Paise { return market.Rupees(e.Bar.Close.Rupees() * (1 - p/100)) }
	}
}

func atrBelow(m float64) func([]market.Bar, []float64) func(swing.Break) market.Paise {
	return func(_ []market.Bar, atr []float64) func(swing.Break) market.Paise {
		return func(e swing.Break) market.Paise {
			a := atr[e.BarIndex]
			if math.IsNaN(a) {
				return e.Pullback
			}
			return max(market.Rupees(e.Bar.Close.Rupees()-m*a), 5) // keep a stop above zero
		}
	}
}

func fixed(f func(swing.Break) market.Paise) func([]market.Bar, []float64) func(swing.Break) market.Paise {
	return func([]market.Bar, []float64) func(swing.Break) market.Paise { return f }
}

var stopRules = []stopRule{
	{name: "none (trail only)", initial: fixed(func(swing.Break) market.Paise { return 0 })},
	{name: "pullback low", initial: fixed(func(e swing.Break) market.Paise { return e.Pullback })},
	{name: "entry bar low", initial: fixed(func(e swing.Break) market.Paise { return e.Bar.Low })},
	{name: "broken level", initial: fixed(func(e swing.Break) market.Paise { return e.Level.High })},
	{name: "fixed 10%", initial: pctBelow(10)},
	{name: "fixed 15%", initial: pctBelow(15)},
	{name: "fixed 20%", initial: pctBelow(20)},
	{name: "fixed 25%", initial: pctBelow(25)},
	{name: "fixed 30%", initial: pctBelow(30)},
	{name: "fixed 40%", initial: pctBelow(40)},
	{name: "ATR14 x1.5", initial: atrBelow(1.5)},
	{name: "ATR14 x2", initial: atrBelow(2)},
	{name: "ATR14 x3", initial: atrBelow(3)},
}

// scalpRules pairs the pullback-low stop and the trail with each profit
// target and maximum holding time.
var scalpRules = func() []stopRule {
	var out []stopRule
	pullback := fixed(func(e swing.Break) market.Paise { return e.Pullback })
	for _, target := range []float64{0, 2, 3, 5, 10, 15, 20, 30} {
		for _, hold := range []time.Duration{0, 3 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute} {
			out = append(out, stopRule{fmt.Sprintf("target %s, hold %s", pctLabel(target), holdLabel(hold)), pullback, target, hold})
		}
	}
	return out
}()

func pctLabel(p float64) string {
	if p == 0 {
		return "none"
	}
	return fmt.Sprintf("%.0f%%", p)
}

func holdLabel(d time.Duration) string {
	if d == 0 {
		return "any"
	}
	return fmt.Sprintf("%.0fm", d.Minutes())
}

type stopTrade struct {
	day   market.Date
	right market.Right
	t     swing.Trade
	net   float64 // rupees for one lot, after charges
}

// runStops replays the lower-high breakout over stored Upstox history with each
// candidate initial stop, and measures how far trades go against the entry.
func runStops(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stops", flag.ContinueOnError)
	underlying := fs.String("underlying", "NIFTY", "underlying name")
	from := fs.String("from", "2024-12-27", "first day")
	to := fs.String("to", "2026-04-30", "last day")
	dir := fs.String("data", "data/upstox", "data directory")
	holdout := fs.String("holdout", "configs/holdout.yaml", "data held back from research")
	costsFile := fs.String("costs", "configs/costs.yaml", "charges schedule")
	step := fs.Float64("step", 100, "strike rounding of the day's open")
	k := fs.Int("k", 2, "bars on each side of a swing high")
	pullback := fs.Float64("min-pullback", 10, "percent pullback a break needs")
	emaN := fs.Int("ema", 21, "entry close above this EMA; 0 turns it off")
	useVWAP := fs.Bool("vwap", true, "entry close above the session VWAP")
	exitK := fs.Int("exit-k", 1, "bars on each side of a higher low for the trailing stop")
	flat := fs.String("flat", "15:15", "time exit; no entries from then")
	after := fs.String("after", "09:15", "no entries before this time")
	perDay := fs.Int("per-day", 0, "keep only the first N trades a day across call and put; 0 keeps all")
	fade := fs.Bool("fade", false, "on each signal buy the other option at the same strike, held for the signal trade's duration")
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
	flatAt, err := time.Parse("15:04", *flat)
	if err != nil {
		return fmt.Errorf("--flat: %w", err)
	}
	flatDur := time.Duration(flatAt.Hour())*time.Hour + time.Duration(flatAt.Minute())*time.Minute
	afterAt, err := time.Parse("15:04", *after)
	if err != nil {
		return fmt.Errorf("--after: %w", err)
	}
	afterDur := time.Duration(afterAt.Hour())*time.Hour + time.Duration(afterAt.Minute())*time.Minute
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
	indexKey := market.Index(market.NSE, *underlying).Key()
	rules := append(append([]stopRule(nil), stopRules...), scalpRules...)
	results := make([][]stopTrade, len(rules))
	var prev map[string][]market.Bar
	skipped, legs := 0, 0
	for _, d := range days {
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
			skipped++
			prev = byKey
			continue
		}
		strike := market.Rupees(math.Round(idx[0].Open.Rupees() / *step) * *step)
		exp, ok := nearestExpiry(lots, byKey, d, strike, *underlying)
		if !ok {
			skipped++
			prev = byKey
			continue
		}
		dayTrades := make([][]stopTrade, len(rules))
		for _, right := range []market.Right{market.Call, market.Put} {
			key := market.Option(market.NFO, *underlying, exp, strike, right, 0).Key()
			bars := byKey[key]
			if len(bars) < 30 {
				skipped++
				continue
			}
			legs++
			warm := prev[key]
			series := append(append([]market.Bar(nil), warm...), bars...)
			fl := filter{emaN: *emaN}
			if *emaN > 0 {
				fl.ema = indicator.EMA(series, *emaN)[len(warm):]
			}
			if *useVWAP {
				fl.vwap = indicator.VWAP(bars)
			}
			atr := indicator.ATR(series, 14)[len(warm):]
			inst := lots[key]
			other := market.Option(market.NFO, *underlying, exp, strike, opposite(right), 0).Key()
			for r, rule := range rules {
				x := swing.Exit{Initial: rule.initial(bars, atr), TrailK: *exitK, FlatAt: flatDur, EntryFrom: afterDur,
					TargetPct: rule.target, MaxHold: rule.hold}
				for _, tr := range swing.Trades(bars, *k, *pullback, fl.ok(bars), x) {
					trInst, traded := inst, opposite(right)
					if *fade {
						var ok bool
						if tr, ok = mirror(tr, byKey[other]); !ok {
							continue
						}
						trInst = lots[other]
					} else {
						traded = right
					}
					net, err := netRupees(costs, trInst, tr)
					if err != nil {
						return err
					}
					dayTrades[r] = append(dayTrades[r], stopTrade{d, traded, tr, net})
				}
			}
		}
		for r, ts := range dayTrades {
			sort.SliceStable(ts, func(i, j int) bool { return ts[i].t.Entry.Bar.Time.Before(ts[j].t.Entry.Bar.Time) })
			if *perDay > 0 && len(ts) > *perDay {
				ts = ts[:*perDay]
			}
			results[r] = append(results[r], ts...)
		}
		prev = byKey
	}
	for r := range results {
		sort.SliceStable(results[r], func(i, j int) bool {
			return results[r][i].t.Entry.Bar.Time.Before(results[r][j].t.Entry.Bar.Time)
		})
	}

	fmt.Printf("%s %s to %s: %d days, %d option-days traded, %d skipped (no index, contract or bars)\n",
		*underlying, f, t, len(days), legs, skipped)
	fmt.Printf("entry: second break, close above VWAP and EMA %d; trail under 1-bar higher lows on closes; time exit %s\n", *emaN, *flat)
	if *after != "09:15" || *perDay > 0 {
		fmt.Printf("entries from %s", *after)
		if *perDay > 0 {
			fmt.Printf(", first %d a day across call and put", *perDay)
		}
		fmt.Println()
	}
	if *fade {
		fmt.Println("FADE: each signal buys the other option at the signal bar's close and sells it at the close of the")
		fmt.Println("bar where the signal trade exits. The rule names below describe the signal trade's exit.")
	}
	fmt.Println()
	printExcursions(results[0])
	fmt.Println("Each initial stop, one lot per trade, after charges, no slippage")
	printRules(stopRules, results[:len(stopRules)])
	fmt.Println("\nScalp exits: pullback-low stop and the trail, plus a profit target and a maximum hold")
	printRules(scalpRules, results[len(stopRules):])
	return nil
}

func opposite(r market.Right) market.Right {
	if r == market.Call {
		return market.Put
	}
	return market.Call
}

// mirror turns a signal trade into a trade in the other option over the same
// minutes: bought at the close of its bar at the signal bar's time, sold at
// the close of its bar at the exit bar's time. A missing bar uses the latest
// one before it. It reports false if the other option has no bar by the entry.
func mirror(t swing.Trade, other []market.Bar) (swing.Trade, bool) {
	at := func(tm time.Time) (int, bool) {
		i := sort.Search(len(other), func(i int) bool { return other[i].Time.After(tm) }) - 1
		return i, i >= 0
	}
	in, ok := at(t.Entry.Bar.Time)
	if !ok {
		return t, false
	}
	out, _ := at(t.Exit.Time)
	if out < in {
		out = in
	}
	m := t
	m.Entry.Bar, m.EntryPrice = other[in], other[in].Close
	m.Exit, m.ExitPrice = other[out], other[out].Close
	m.Worst, m.Best = m.EntryPrice, m.EntryPrice
	for _, b := range other[in+1 : out+1] {
		m.Worst, m.Best = min(m.Worst, b.Low), max(m.Best, b.High)
	}
	m.Initial, m.Stop, m.StopFrom = 0, 0, nil
	return m, true
}

// nearestExpiry is the first expiry on or after d with a call at strike traded
// that day.
func nearestExpiry(lots map[string]market.Instrument, byKey map[string][]market.Bar, d market.Date, strike market.Paise, u string) (market.Date, bool) {
	var best market.Date
	found := false
	for key, in := range lots {
		if in.Kind != market.KindOption || in.Strike != strike || in.Right != market.Call || in.Expiry.Before(d) {
			continue
		}
		if len(byKey[key]) == 0 {
			continue
		}
		if !found || in.Expiry.Before(best) {
			best, found = in.Expiry, true
		}
	}
	return best, found
}

func netRupees(c *cost.Schedule, inst market.Instrument, t swing.Trade) (float64, error) {
	buy := market.Fill{Instrument: inst, Side: market.Buy, Qty: inst.LotSize, Price: t.EntryPrice, Time: t.Entry.Bar.End, Kind: market.Traded}
	sell := market.Fill{Instrument: inst, Side: market.Sell, Qty: inst.LotSize, Price: t.ExitPrice, Time: t.Exit.End, Kind: market.Traded}
	cb, err := c.Charges(buy)
	if err != nil {
		return 0, err
	}
	cs, err := c.Charges(sell)
	if err != nil {
		return 0, err
	}
	gross := (t.ExitPrice - t.EntryPrice) * market.Paise(inst.LotSize)
	return (gross - cb - cs).Rupees(), nil
}

func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[int(math.Round(p/100*float64(len(s)-1)))]
}

// printExcursions shows, with no initial stop, how far winners and losers
// went below the entry before exiting.
func printExcursions(ts []stopTrade) {
	var win, lose []float64
	for _, s := range ts {
		if s.t.PnLPct() > 0 {
			win = append(win, s.t.MAEPct())
		} else {
			lose = append(lose, s.t.MAEPct())
		}
	}
	fmt.Printf("How far trades went against the entry with no initial stop (MAE, %% of entry premium)\n")
	fmt.Printf("  %d trades: %d winners, %d losers\n", len(ts), len(win), len(lose))
	fmt.Printf("  %-8s %6s %6s %6s %6s %6s\n", "", "p50", "p75", "p90", "p95", "max")
	for _, row := range []struct {
		name string
		xs   []float64
	}{{"winners", win}, {"losers", lose}} {
		fmt.Printf("  %-8s", row.name)
		for _, p := range []float64{50, 75, 90, 95, 100} {
			fmt.Printf(" %5.1f%%", percentile(row.xs, p))
		}
		fmt.Println()
	}
	fmt.Printf("\n  a stop X%% below entry would have hit:\n  %6s %12s %12s\n", "X", "of winners", "of losers")
	for _, x := range []float64{5, 10, 15, 20, 25, 30, 40, 50} {
		fmt.Printf("  %5.0f%% %11.0f%% %11.0f%%\n", x, share(win, x), share(lose, x))
	}
	var loseMFE []float64
	reasons := map[string]int{}
	for _, s := range ts {
		if s.t.PnLPct() <= 0 {
			loseMFE = append(loseMFE, s.t.MFEPct())
		}
		reasons[s.t.Reason.String()]++
	}
	fmt.Printf("\n  losers were first up (MFE): p50 %.1f%%, p75 %.1f%%, p90 %.1f%%; %0.f%% were up 10%% or more\n",
		percentile(loseMFE, 50), percentile(loseMFE, 75), percentile(loseMFE, 90), share(loseMFE, 10))
	fmt.Printf("  exits: %d trailing or initial stop, %d time, %d end of data\n", reasons["stop"], reasons["time"], reasons["end"])
	var holds []float64
	for _, s := range ts {
		holds = append(holds, s.t.Hold().Minutes())
	}
	fmt.Printf("  minutes held: p25 %.0f, p50 %.0f, p75 %.0f, p90 %.0f, max %.0f\n",
		percentile(holds, 25), percentile(holds, 50), percentile(holds, 75), percentile(holds, 90), percentile(holds, 100))
	fmt.Println()
}

func share(xs []float64, at float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	n := 0
	for _, x := range xs {
		if x >= at {
			n++
		}
	}
	return float64(n) / float64(len(xs)) * 100
}

// printRules compares each initial stop on net rupees for one lot per trade.
func printRules(rules []stopRule, results [][]stopTrade) {
	fmt.Printf("  %-24s %6s %5s %8s %8s %9s %10s %6s %10s %8s %8s\n",
		"stop", "trades", "win%", "avg win", "avg loss", "avg %", "net Rs", "PF", "max DD Rs", "worst %", "init hit")
	for r, rule := range rules {
		ts := results[r]
		var wins, losses, sumW, sumL, sumPct, total, peak, dd, worst float64
		stops := 0
		worst = math.Inf(1)
		for _, s := range ts {
			sumPct += s.t.PnLPct()
			worst = min(worst, s.t.PnLPct())
			if s.t.Reason == swing.Stopped && s.t.StopFrom == nil {
				stops++
			}
			if s.net > 0 {
				wins++
				sumW += s.net
			} else {
				losses++
				sumL -= s.net
			}
			total += s.net
			peak = max(peak, total)
			dd = max(dd, peak-total)
		}
		n := float64(len(ts))
		if n == 0 {
			fmt.Printf("  %-24s no trades\n", rule.name)
			continue
		}
		pf := math.Inf(1)
		if sumL > 0 {
			pf = sumW / sumL
		}
		avgW, avgL := 0.0, 0.0
		if wins > 0 {
			avgW = sumW / wins
		}
		if losses > 0 {
			avgL = sumL / losses
		}
		fmt.Printf("  %-24s %6d %4.0f%% %8.0f %8.0f %8.1f%% %10.0f %6.2f %10.0f %7.0f%% %8d\n",
			rule.name, len(ts), wins/n*100, avgW, -avgL, sumPct/n, total, pf, dd, worst, stops)
	}
}
