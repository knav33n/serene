package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"sort"
	"strings"

	"serene/internal/data"
	"serene/internal/indicator"
	"serene/internal/market"
)

// divRule is a long-only bullish RSI divergence on daily bars: a swing low
// below the previous swing low with a higher RSI, optionally with the RSI at
// the first low under oversold and a close above an EMA to confirm.
type divRule struct {
	name       string
	rsiN, k    int
	minGap     int
	maxGap     int
	oversold   float64 // 0 off
	confirmEMA int     // 0 off
	confirmN   int
	targetR    float64
	maxHold    int // trading days
}

// stockTrade is one simulated trade, entered at the open after the signal.
type stockTrade struct {
	symbol      string
	signal      market.Date
	entry, exit market.Date
	in, out     float64
	days        int
	why         string
	excess      float64 // gross return minus the equal-weight universe's, same days
}

func (t stockTrade) gross() float64 { return t.out/t.in - 1 }

// divSignals returns the bars at whose close a rule signals, with the stop
// (the second low) for each. eligible gates the signal bar.
func divSignals(bars []market.Bar, eligible []bool, r divRule) (idx []int, stops []float64) {
	rsi := indicator.RSI(bars, r.rsiN)
	var ema []float64
	if r.confirmEMA > 0 {
		ema = indicator.EMA(bars, r.confirmEMA)
	}
	isLow := func(p int) bool {
		if p-r.k < 0 || p+r.k >= len(bars) {
			return false
		}
		for j := p - r.k; j <= p+r.k; j++ {
			if j != p && bars[j].Low <= bars[p].Low {
				return false
			}
		}
		return true
	}
	last, waitP, waitUntil := -1, -1, -1
	for i := range bars {
		if p := i - r.k; isLow(p) {
			q := last
			last = p
			if q >= 0 && p-q >= r.minGap && p-q <= r.maxGap && bars[p].Low < bars[q].Low &&
				!math.IsNaN(rsi[p]) && !math.IsNaN(rsi[q]) && rsi[p] > rsi[q] && (r.oversold == 0 || rsi[q] < r.oversold) {
				waitP, waitUntil = p, i+r.confirmN
			}
		}
		if waitP < 0 {
			continue
		}
		if r.confirmEMA > 0 && (math.IsNaN(ema[i]) || bars[i].Close.Rupees() <= ema[i]) {
			if i >= waitUntil {
				waitP = -1
			}
			continue
		}
		if eligible[i] {
			idx, stops = append(idx, i), append(stops, bars[waitP].Low.Rupees())
		}
		waitP = -1
	}
	return idx, stops
}

// simulate trades the signals one at a time per stock: buy at the next open,
// sell at the stop, at targetR times the risk, or at the close after maxHold
// days. A gap through a level fills at the open; a day touching both counts
// as the stop.
func simulate(symbol string, bars []market.Bar, idx []int, stops []float64, r divRule) []stockTrade {
	var out []stockTrade
	busy := -1
	for n, i := range idx {
		e := i + 1
		if e >= len(bars) || e <= busy {
			continue
		}
		in, stop := bars[e].Open.Rupees(), stops[n]
		if in <= stop {
			continue
		}
		target := in + r.targetR*(in-stop)
		t := stockTrade{symbol: symbol, signal: market.DateOf(bars[i].Time), entry: market.DateOf(bars[e].Time), in: in}
		j := e
		for ; j < len(bars); j++ {
			o, h, l, c := bars[j].Open.Rupees(), bars[j].High.Rupees(), bars[j].Low.Rupees(), bars[j].Close.Rupees()
			switch {
			case j > e && o <= stop:
				t.out, t.why = o, "stop"
			case l <= stop:
				t.out, t.why = stop, "stop"
			case j > e && o >= target:
				t.out, t.why = o, "target"
			case h >= target:
				t.out, t.why = target, "target"
			case j-e+1 >= r.maxHold:
				t.out, t.why = c, "time"
			case j == len(bars)-1:
				t.out, t.why = c, "end"
			default:
				continue
			}
			break
		}
		t.exit, t.days, busy = market.DateOf(bars[j].Time), j-e+1, j
		out = append(out, t)
	}
	return out
}

func runStockDiv(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("research stockdiv", flag.ContinueOnError)
	dir := fs.String("data", "data/equities", "equities data directory")
	fromS := fs.String("from", "2005-01-01", "first signal date")
	toS := fs.String("to", "2022-12-31", "last date; later data is held back")
	top := fs.Int("top", 200, "universe: most traded stocks in the previous month")
	costPct := fs.Float64("cost-pct", 0.24, "round-trip delivery charges, percent")
	slipBps := fs.Float64("slip-bps", 10, "slippage each side, basis points")
	unseal := fs.Bool("unseal", false, "allow dates after 2022-12-31")
	if err := fs.Parse(args); err != nil {
		return err
	}
	from, err := market.ParseDate(*fromS)
	if err != nil {
		return err
	}
	to, err := market.ParseDate(*toS)
	if err != nil {
		return err
	}
	if seal, _ := market.ParseDate("2023-01-01"); !to.Before(seal) && !*unseal {
		return fmt.Errorf("data from 2023-01-01 is held back for a final check; pass --unseal for that one run")
	}
	store, err := data.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()
	panel, err := store.EquityPanel(ctx, from.AddDays(-400), to, *top)
	if err != nil {
		return err
	}
	ewi := universeIndex(panel)

	rules := []divRule{
		{name: "confirmed: RSI<30 at 1st low, close > EMA20 within 4 bars", rsiN: 14, k: 2, minGap: 3, maxGap: 24, oversold: 30, confirmEMA: 20, confirmN: 4, targetR: 2, maxHold: 20},
		{name: "plain: RSI<40 at 1st low, no confirmation", rsiN: 14, k: 2, minGap: 3, maxGap: 24, oversold: 40, targetR: 2, maxHold: 20},
	}
	fmt.Printf("Bullish RSI(14) divergence, long only, daily bars; top %d stocks by last month's turnover\n", *top)
	fmt.Printf("signals %s to %s; buy next open; stop at the 2nd low, target 2R, exit after 20 days\n", from, to)
	fmt.Printf("costs %.2f%% round trip + %.0f bps slippage each side\n\n", *costPct, *slipBps)
	for _, r := range rules {
		var trades []stockTrade
		for lo := 0; lo < len(panel); {
			hi := lo
			for hi < len(panel) && panel[hi].Symbol == panel[lo].Symbol {
				hi++
			}
			bars, elig := panelBars(panel[lo:hi], from)
			idx, stops := divSignals(bars, elig, r)
			trades = append(trades, simulate(panel[lo].Symbol, bars, idx, stops, r)...)
			lo = hi
		}
		for i := range trades {
			trades[i].excess = trades[i].gross() - (ewi[trades[i].exit]/ewi[trades[i].signal] - 1)
		}
		reportStockTrades(r.name, trades, *costPct/100, *slipBps/1e4)
	}
	return nil
}

// panelBars turns one stock's days into bars, eligible only from from on.
func panelBars(days []data.PanelDay, from market.Date) ([]market.Bar, []bool) {
	bars := make([]market.Bar, len(days))
	elig := make([]bool, len(days))
	for i, d := range days {
		t := market.NSESession.Open(d.Date)
		bars[i] = market.Bar{Key: d.Symbol, Time: t, End: t, Open: market.Rupees(d.O), High: market.Rupees(d.H),
			Low: market.Rupees(d.L), Close: market.Rupees(d.C)}
		elig[i] = d.Eligible && !d.Date.Before(from)
	}
	return bars, elig
}

// universeIndex is an equal-weight index of the eligible stocks' daily
// close-to-close returns, each capped at ±50% against bad prints.
func universeIndex(panel []data.PanelDay) map[market.Date]float64 {
	sum, n := map[market.Date]float64{}, map[market.Date]int{}
	for i := 1; i < len(panel); i++ {
		a, b := panel[i-1], panel[i]
		if a.Symbol != b.Symbol || !b.Eligible {
			continue
		}
		r := max(-0.5, min(0.5, b.C/a.C-1))
		sum[b.Date] += r
		n[b.Date]++
	}
	days := make([]market.Date, 0, len(sum))
	for d := range sum {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	out, level := map[market.Date]float64{}, 1.0
	for _, d := range days {
		level *= 1 + sum[d]/float64(n[d])
		out[d] = level
	}
	// Days before the first eligible one keep the index at 1.
	for _, d := range panel {
		if _, ok := out[d.Date]; !ok {
			out[d.Date] = 1
		}
	}
	return out
}

func reportStockTrades(name string, trades []stockTrade, cost, slip float64) {
	fmt.Println("== " + name)
	if len(trades) == 0 {
		fmt.Println("no trades")
		return
	}
	stats := func(ts []stockTrade, s float64) (n int, win, mean, pf, excess, t float64) {
		var gain, loss, sum, sq float64
		for _, tr := range ts {
			r := tr.gross() - cost - 2*s
			sum, sq = sum+r, sq+r*r
			excess += tr.excess - cost - 2*s
			if r > 0 {
				win++
				gain += r
			} else {
				loss -= r
			}
		}
		n = len(ts)
		mean = sum / float64(n)
		if sd := math.Sqrt(sq/float64(n) - mean*mean); sd > 0 {
			t = mean / sd * math.Sqrt(float64(n))
		}
		if loss > 0 {
			pf = gain / loss
		}
		return n, win / float64(n), mean, pf, excess / float64(n), t
	}
	days, why := 0, map[string]int{}
	for _, tr := range trades {
		days += tr.days
		why[tr.why]++
	}
	fmt.Printf("%d trades, avg hold %.1f days, exits %v\n", len(trades), float64(days)/float64(len(trades)), why)
	fmt.Printf("%-10s %6s %7s %9s %6s %10s %6s\n", "slippage", "trades", "win", "avg net", "PF", "vs univ.", "t")
	for _, m := range []float64{0, 1, 2} {
		n, win, mean, pf, ex, t := stats(trades, slip*m)
		fmt.Printf("%-10s %6d %6.1f%% %8.2f%% %6.2f %9.2f%% %6.2f\n", fmt.Sprintf("%.0fx", m), n, 100*win, 100*mean, pf, 100*ex, t)
	}
	byYear := map[int][]stockTrade{}
	for _, tr := range trades {
		byYear[tr.entry.Year] = append(byYear[tr.entry.Year], tr)
	}
	years := make([]int, 0, len(byYear))
	for y := range byYear {
		years = append(years, y)
	}
	sort.Ints(years)
	var row []string
	for _, y := range years {
		n, _, mean, _, ex, _ := stats(byYear[y], slip)
		row = append(row, fmt.Sprintf("%d: %d tr %+.2f%% (%+.2f%% vs univ.)", y, n, 100*mean, 100*ex))
	}
	fmt.Println("by year at 1x slippage, avg net per trade:\n  " + strings.Join(row, "\n  "))
	fmt.Println()
}
