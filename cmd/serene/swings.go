package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"serene/internal/indicator"
	"serene/internal/ingest"
	"serene/internal/market"
	"serene/internal/swing"
	"serene/internal/upstox"
)

// runSwings fetches one day of an at-the-money call and put from Upstox and
// reports each one's first and second breaks: a close above a significant
// lower high, then above the next swing high. The second break is the entry. It reads
// unexpired contracts and stores nothing.
func runSwings(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("swings", flag.ContinueOnError)
	underlying := fs.String("underlying", "NIFTY", "underlying name")
	date := fs.String("date", "", "trading day (required)")
	strike := fs.Float64("strike", 0, "strike; 0 rounds the day's open to --step")
	step := fs.Float64("step", 100, "strike rounding for --strike 0")
	expiry := fs.String("expiry", "", "expiry; empty picks the nearest on or after --date")
	k := fs.Int("k", 2, "bars on each side of a swing high")
	pullback := fs.Float64("min-pullback", 10, "percent the price must fall below a swing high before its break counts")
	emaN := fs.Int("ema", 21, "entry close must be above this EMA of the option's 1-minute closes; 0 turns it off")
	useVWAP := fs.Bool("vwap", true, "entry close must be above the option's session VWAP")
	exitK := fs.Int("exit-k", 1, "bars on each side of a higher low on closes, for the trailing stop")
	verbose := fs.Bool("verbose", false, "print every bar")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *date == "" {
		return errors.New("--date is required")
	}
	day, err := market.ParseDate(*date)
	if err != nil {
		return err
	}
	indexKey, ok := ingest.IndexKeys[*underlying]
	if !ok {
		return fmt.Errorf("unknown underlying %q", *underlying)
	}
	up, err := upstoxClient(ctx)
	if err != nil {
		return err
	}
	index, err := dayBars(ctx, up, indexKey, day, day)
	if err != nil {
		return fmt.Errorf("index candles: %w", err)
	}
	if len(index) == 0 {
		return fmt.Errorf("no %s candles on %s", *underlying, day)
	}
	open := index[0].Open.Rupees()
	fmt.Printf("%s %s: open %.2f at %s, high %.2f, low %.2f, last %.2f\n", *underlying, day, open,
		index[0].Time.Format("15:04"), maxHigh(index).Rupees(), minLow(index).Rupees(), index[len(index)-1].Close.Rupees())
	if *strike == 0 {
		*strike = math.Round(open / *step) * *step
	}

	var want market.Date
	if *expiry != "" {
		if want, err = market.ParseDate(*expiry); err != nil {
			return err
		}
	}
	contracts, err := up.LiveOptionContracts(ctx, indexKey, want)
	if err != nil {
		return fmt.Errorf("option contracts: %w", err)
	}
	ce, pe, exp, err := pickContracts(contracts, *strike, day, want)
	if err != nil {
		return err
	}
	fmt.Printf("strike %.0f, expiry %s; swing highs need %d bars each side, breaks need a %.0f%% pullback; times are bar starts\n", *strike, exp, *k, *pullback)
	var rules []string
	if *useVWAP {
		rules = append(rules, "VWAP")
	}
	if *emaN > 0 {
		rules = append(rules, fmt.Sprintf("EMA %d", *emaN))
	}
	if len(rules) > 0 {
		fmt.Printf("entry close must be above %s\n", strings.Join(rules, " and "))
	}
	fmt.Println()

	type result struct {
		name string
		brk  swing.Break
		ok   bool
	}
	var results []result
	for _, c := range []struct{ name, key string }{{"CE", ce}, {"PE", pe}} {
		// A week of earlier bars warms the EMA up, so it is settled at the open.
		all, err := dayBars(ctx, up, c.key, day.AddDays(-7), day)
		if err != nil {
			return fmt.Errorf("%s candles: %w", c.name, err)
		}
		var ema []float64
		if *emaN > 0 {
			ema = indicator.EMA(all, *emaN)
		}
		var bars []market.Bar
		for i, b := range all {
			if market.DateOf(b.Time) == day {
				bars = append(bars, b)
				if ema != nil && len(bars) == 1 {
					ema = ema[i:]
				}
			}
		}
		label := fmt.Sprintf("%s %.0f %s", *underlying, *strike, c.name)
		if len(bars) == 0 {
			fmt.Printf("%s: no candles\n\n", label)
			continue
		}
		f := filter{ema: ema, emaN: *emaN}
		if *useVWAP {
			f.vwap = indicator.VWAP(bars)
		}
		b, ok := report(label, bars, *k, *pullback, f, *exitK, *verbose)
		results = append(results, result{c.name, b, ok})
	}

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].ok != results[j].ok {
			return results[i].ok
		}
		return results[i].brk.Bar.Time.Before(results[j].brk.Bar.Time)
	})
	if len(results) > 0 && results[0].ok {
		fmt.Printf("first entry: %s on the %s bar\n", results[0].name, results[0].brk.Bar.Time.Format("15:04"))
	} else {
		fmt.Println("neither option made a second break")
	}
	fmt.Println("\nnote: configs/holdout.yaml seals Upstox data from 2026-05-01; tune any strategy from this on earlier days.")
	return nil
}

// pickContracts finds the call and put keys at strike for want, or for the
// nearest expiry on or after day when want is zero.
func pickContracts(cs []upstox.Contract, strike float64, day, want market.Date) (ce, pe string, exp market.Date, err error) {
	var expiries []market.Date
	seen := map[market.Date]bool{}
	for _, c := range cs {
		d, err := market.ParseDate(c.Expiry)
		if err != nil || seen[d] || d.Before(day) {
			continue
		}
		seen[d] = true
		expiries = append(expiries, d)
	}
	sort.Slice(expiries, func(i, j int) bool { return expiries[i].Before(expiries[j]) })
	exp = want
	if exp == (market.Date{}) {
		if len(expiries) == 0 {
			return "", "", exp, fmt.Errorf("no listed expiry on or after %s; its options may have expired", day)
		}
		exp = expiries[0]
	}
	for _, c := range cs {
		if c.Expiry != exp.String() || c.Strike != strike {
			continue
		}
		switch c.Type {
		case "CE":
			ce = c.Key
		case "PE":
			pe = c.Key
		}
	}
	if ce == "" || pe == "" {
		return "", "", exp, fmt.Errorf("no %.0f call and put listed for %s", strike, exp)
	}
	return ce, pe, exp, nil
}

// filter is the entry rule: the close above the session VWAP and the EMA.
// Either slice may be nil, which leaves that rule out. They line up with the
// day's bars.
type filter struct {
	vwap, ema []float64
	emaN      int
}

func (f filter) ok(bars []market.Bar) func(int) bool {
	return func(i int) bool {
		c := bars[i].Close.Rupees()
		return (f.vwap == nil || c > f.vwap[i]) && (f.ema == nil || (!math.IsNaN(f.ema[i]) && c > f.ema[i]))
	}
}

// levels describes the filter lines at bar i.
func (f filter) levels(i int) string {
	var parts []string
	if f.vwap != nil {
		parts = append(parts, fmt.Sprintf("VWAP %.2f", f.vwap[i]))
	}
	if f.ema != nil {
		parts = append(parts, fmt.Sprintf("EMA%d %.2f", f.emaN, f.ema[i]))
	}
	return strings.Join(parts, ", ")
}

// dayBars returns the session 1-minute bars from one day to another, oldest
// first.
func dayBars(ctx context.Context, up *upstox.Client, key string, from, to market.Date) ([]market.Bar, error) {
	cs, err := up.Candles(ctx, key, from, to)
	if err != nil {
		return nil, err
	}
	var out []market.Bar
	for _, c := range cs {
		t := c.Time.In(market.IST)
		if !market.NSESession.Contains(t) {
			continue
		}
		out = append(out, market.Bar{Key: key, Timeframe: market.Minute1, Time: t, End: t.Add(time.Minute),
			Open: market.Rupees(c.Open), High: market.Rupees(c.High), Low: market.Rupees(c.Low),
			Close: market.Rupees(c.Close), Volume: c.Volume, OI: c.OI})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// report prints an option's breaks and returns its first Second break.
func report(label string, bars []market.Bar, k int, pullback float64, f filter, exitK int, verbose bool) (swing.Break, bool) {
	index := map[time.Time]int{}
	for i, b := range bars {
		index[b.Time] = i
	}
	last := bars[len(bars)-1]
	fmt.Printf("%s: open %s, high %s, low %s, last %s (%d bars)\n", label, bars[0].Open, maxHigh(bars), minLow(bars), last.Close, len(bars))
	if verbose {
		for _, b := range bars {
			fmt.Printf("  %s  O %-8s H %-8s L %-8s C %-8s V %-9d %s\n", b.Time.Format("15:04"), b.Open, b.High, b.Low, b.Close, b.Volume, f.levels(index[b.Time]))
		}
		fmt.Printf("  swing highs:")
		pivots := swing.Pivots(bars, k)
		for i, p := range pivots {
			mark := ""
			if i > 0 && p.High < pivots[i-1].High {
				mark = " LH"
			}
			fmt.Printf(" %s %s%s;", p.Time.Format("15:04"), p.High, mark)
		}
		fmt.Println()
	}
	var entry swing.Break
	found := false
	for _, b := range swing.Breaks(bars, k, pullback, f.ok(bars)) {
		fmt.Printf("  %-6s %s close %-7s above %s high %s (known %s, pulled back to %s, -%.1f%%)",
			b.Kind, b.Bar.Time.Format("15:04"), b.Bar.Close, b.Level.Time.Format("15:04"), b.Level.High,
			b.Level.Confirmed.Format("15:04"), b.Pullback, b.PullbackPct())
		if b.Kind == swing.Second {
			for _, x := range b.Blocked {
				fmt.Printf("\n         %s close %-7s blocked: %s", x.Time.Format("15:04"), x.Close, f.levels(index[x.Time]))
			}
			if lv := f.levels(index[b.Bar.Time]); lv != "" {
				fmt.Printf("\n         %s close %-7s passed:  %s", b.Bar.Time.Format("15:04"), b.Bar.Close, lv)
			}
			fmt.Printf("\n         ENTRY; %s", outcome(bars, b))
			if !found {
				entry, found = b, true
			}
		}
		fmt.Println()
	}
	if !found {
		fmt.Println("  no second break")
	}
	trades := swing.Trades(bars, k, pullback, f.ok(bars), swing.Exit{TrailK: exitK})
	if len(trades) > 0 {
		fmt.Println("  trades (buy the second break's close, sell below the latest higher low):")
	}
	total := 0.0
	for _, t := range trades {
		why := "stop below the pullback low"
		if t.StopFrom != nil {
			why = "below the " + t.StopFrom.Time.Format("15:04") + " higher low"
		}
		if t.Reason != swing.Stopped {
			why = "last bar, stop was " + t.Stop.String()
		}
		pts := t.ExitPrice.Rupees() - t.EntryPrice.Rupees()
		total += pts
		fmt.Printf("    buy %s @ %-7s sell %s @ %-7s %+7.2f (%+.1f%%)  %s %s\n",
			t.Entry.Bar.Time.Format("15:04"), t.EntryPrice, t.Exit.Time.Format("15:04"), t.ExitPrice,
			pts, t.PnLPct(), why, t.Stop)
	}
	if len(trades) > 1 {
		fmt.Printf("    total %+.2f points over %d trades\n", total, len(trades))
	}
	fmt.Println()
	return entry, found
}

// outcome summarises the move after a break: the best high and worst low to
// come, and the last close, against the break bar's close.
func outcome(bars []market.Bar, b swing.Break) string {
	var after []market.Bar
	for _, x := range bars {
		if x.Time.After(b.Bar.Time) {
			after = append(after, x)
		}
	}
	if len(after) == 0 {
		return "no bars after"
	}
	top := after[0]
	for _, x := range after {
		if x.High > top.High {
			top = x
		}
	}
	pct := func(p market.Paise) float64 { return (p.Rupees()/b.Bar.Close.Rupees() - 1) * 100 }
	low, last := minLow(after), after[len(after)-1].Close
	return fmt.Sprintf("then high %s at %s (%+.0f%%), low %s (%+.0f%%), last %s (%+.0f%%)",
		top.High, top.Time.Format("15:04"), pct(top.High), low, pct(low), last, pct(last))
}

func maxHigh(bars []market.Bar) market.Paise {
	h := bars[0].High
	for _, b := range bars {
		h = max(h, b.High)
	}
	return h
}

func minLow(bars []market.Bar) market.Paise {
	l := bars[0].Low
	for _, b := range bars {
		l = min(l, b.Low)
	}
	return l
}
