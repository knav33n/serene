package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"

	"serene/internal/data"
	"serene/internal/market"
)

// n500Pairs are the daily EMA crossovers tried on each stock, fast/slow.
var n500Pairs = [][2]int{{5, 13}, {5, 20}, {8, 21}, {9, 21}, {10, 30}, {12, 26}, {13, 34}, {20, 50},
	{21, 55}, {30, 100}, {50, 100}, {50, 200}, {100, 200}}

// emaStats is one long-only EMA cross (or buy and hold) over one period.
type emaStats struct {
	days, trades     int
	cagr, sharpe, dd float64 // dd is the worst mark-to-market drop, as a positive fraction
	exposure, total  float64
}

// emaCloses is an EMA of closes seeded with the simple average of the first n,
// like indicator.EMA.
func emaCloses(c []float64, n int) []float64 {
	out := make([]float64, len(c))
	alpha, sum := 2/float64(n+1), 0.0
	for i, x := range c {
		switch {
		case i < n-1:
			sum += x
			out[i] = math.NaN()
		case i == n-1:
			out[i] = (sum + x) / float64(n)
		default:
			out[i] = alpha*x + (1-alpha)*out[i-1]
		}
	}
	return out
}

// crossRun trades days[lo:hi] long while fast > slow at the previous close,
// and short below it when short is set (else flat), starting flat. A change
// fills at the next open, paying side (charges plus slippage, a fraction) on
// each fill; a position open at the end is closed at the last close. fast ==
// nil is buy and hold from the first open. Returns are compounded from daily
// ones, a short as the negative of the day's move; idle money earns nothing.
func crossRun(days []data.PanelDay, fast, slow []float64, lo, hi int, side float64, short bool) emaStats {
	var (
		st       emaStats
		eq, peak = 1.0, 1.0
		sum, sq  float64
		pos      float64 // 1 long, -1 short, 0 flat
		inDays   int
	)
	cost := func(p float64) float64 { return 1 - side*math.Abs(p) }
	for i := lo; i < hi; i++ {
		want := 0.0
		switch {
		case fast == nil:
			want = 1
		case i > 0 && !math.IsNaN(slow[i-1]) && !math.IsNaN(fast[i-1]):
			if fast[i-1] > slow[i-1] {
				want = 1
			} else if short {
				want = -1
			}
		}
		d := days[i]
		var g float64
		if pos == want {
			g = 1 + pos*(d.C/days[i-1].C-1)
		} else {
			g = 1.0
			if pos != 0 {
				g = (1 + pos*(d.O/days[i-1].C-1)) * cost(pos)
			}
			g *= cost(want) * (1 + want*(d.C/d.O-1))
			if want != 0 {
				st.trades++
			}
			pos = want
		}
		if i == hi-1 {
			g *= cost(pos)
		}
		r := max(g-1, -1)
		if pos != 0 {
			inDays++
		}
		eq *= 1 + r
		peak = max(peak, eq)
		st.dd = max(st.dd, 1-eq/peak)
		sum, sq = sum+r, sq+r*r
	}
	st.days = hi - lo
	if st.days == 0 {
		return st
	}
	n := float64(st.days)
	st.total = eq - 1
	st.cagr = math.Pow(eq, 252/n) - 1
	st.exposure = float64(inDays) / n
	if sd := math.Sqrt(sq/n - (sum/n)*(sum/n)); sd > 0 {
		st.sharpe = sum / n / sd * math.Sqrt(252)
	}
	return st
}

// readSymbols reads the Symbol column of a niftyindices constituent file,
// skipping # comment lines.
func readSymbols(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := sc.Text(); !strings.HasPrefix(l, "#") && strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	rows, err := csv.NewReader(strings.NewReader(strings.Join(lines, "\n"))).ReadAll()
	if err != nil || len(rows) == 0 {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	col := -1
	for i, h := range rows[0] {
		if strings.EqualFold(strings.TrimSpace(h), "Symbol") {
			col = i
		}
	}
	if col < 0 {
		return nil, fmt.Errorf("%s: no Symbol column", path)
	}
	var out []string
	for _, r := range rows[1:] {
		out = append(out, strings.TrimSpace(r[col]))
	}
	return out, nil
}

// n500Stock is one stock's grid: stats per pair, then buy and hold last.
type n500Stock struct {
	symbol       string
	trainFrom    market.Date
	train, test  []emaStats
	pick         int     // pair with the best train Sharpe
	pickTestRank float64 // that pair's test Sharpe percentile among pairs, 1 = best
}

// runStockEMA runs daily EMA crossovers on every stock in a constituent list,
// picks each stock's pair on the train years and scores the pick on the test
// years, which nothing chose.
func runStockEMA(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("research stockema", flag.ContinueOnError)
	dir := fs.String("data", "data/equities", "equities data directory")
	list := fs.String("list", "configs/nifty500.csv", "constituent list")
	trainS := fs.String("from", "2005-01-01", "first train date")
	splitS := fs.String("split", "2019-01-01", "first test date")
	toS := fs.String("to", "2022-12-31", "last date; later data is held back")
	minYears := fs.Float64("min-train-years", 3, "skip stocks with less train history")
	costPct := fs.Float64("cost-pct", 0.24, "round-trip delivery charges, percent")
	slipBps := fs.Float64("slip-bps", 10, "slippage each side, basis points")
	out := fs.String("out", "reports/nifty500_ema.csv", "per-stock results")
	short := fs.Bool("short", false, "short below the cross (futures) instead of flat")
	unseal := fs.Bool("unseal", false, "allow dates after 2022-12-31")
	if err := fs.Parse(args); err != nil {
		return err
	}
	from, err := market.ParseDate(*trainS)
	if err != nil {
		return err
	}
	split, err := market.ParseDate(*splitS)
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
	syms, err := readSymbols(*list)
	if err != nil {
		return err
	}
	store, err := data.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()
	// A renamed stock trades under another symbol with the same ISIN; its
	// old days are joined to today's symbol.
	owner := map[string]string{}
	for _, s := range syms {
		owner[s] = s
	}
	for _, s := range syms {
		al, err := store.EquityAliases(ctx, s)
		if err != nil {
			return err
		}
		for _, a := range al {
			if _, ok := owner[a]; !ok {
				owner[a] = s
			}
		}
	}
	// Warm the slowest EMA before the first train day.
	panel, err := store.EquityPanel(ctx, from.AddDays(-400), to, 1<<30)
	if err != nil {
		return err
	}
	side := *costPct/200 + *slipBps/1e4
	slowest := 0
	for _, p := range n500Pairs {
		slowest = max(slowest, p[1])
	}

	bySym := map[string][][]data.PanelDay{}
	for lo := 0; lo < len(panel); {
		hi := lo
		for hi < len(panel) && panel[hi].Symbol == panel[lo].Symbol {
			hi++
		}
		if s, ok := owner[panel[lo].Symbol]; ok {
			bySym[s] = append(bySym[s], panel[lo:hi])
		}
		lo = hi
	}
	var stocks []n500Stock
	found := map[string]bool{}
	skipped := map[string]string{}
	renamed := 0
	for _, sym := range syms {
		days, joined, err := joinRenames(bySym[sym])
		if err != nil {
			skipped[sym] = err.Error()
			found[sym] = true
			continue
		}
		if len(days) == 0 {
			continue
		}
		found[sym] = true
		if joined {
			renamed++
		}
		closes := make([]float64, len(days))
		for i, d := range days {
			closes[i] = d.C
		}
		// Train starts once the slowest EMA has a value, so every pair and
		// buy and hold cover the same days.
		a := slowest
		for a < len(days) && days[a].Date.Before(from) {
			a++
		}
		b := a
		for b < len(days) && days[b].Date.Before(split) {
			b++
		}
		if float64(b-a) < *minYears*252 {
			skipped[sym] = fmt.Sprintf("%d train days", max(b-a, 0))
			continue
		}
		if len(days)-b < 3*252 {
			skipped[sym] = fmt.Sprintf("%d test days", len(days)-b)
			continue
		}
		s := n500Stock{symbol: sym, trainFrom: days[a].Date}
		for _, p := range n500Pairs {
			f, sl := emaCloses(closes, p[0]), emaCloses(closes, p[1])
			s.train = append(s.train, crossRun(days, f, sl, a, b, side, *short))
			s.test = append(s.test, crossRun(days, f, sl, b, len(days), side, *short))
		}
		s.train = append(s.train, crossRun(days, nil, nil, a, b, side, false))
		s.test = append(s.test, crossRun(days, nil, nil, b, len(days), side, false))
		np := len(n500Pairs)
		for k := 1; k < np; k++ {
			if s.train[k].sharpe > s.train[s.pick].sharpe {
				s.pick = k
			}
		}
		below := 0
		for k := 0; k < np; k++ {
			if s.test[k].sharpe < s.test[s.pick].sharpe {
				below++
			}
		}
		s.pickTestRank = float64(below) / float64(np-1)
		stocks = append(stocks, s)
	}
	var missing []string
	for _, s := range syms {
		if !found[s] {
			missing = append(missing, s)
		}
	}
	sort.Strings(missing)
	fmt.Printf("(%d histories joined across a rename)\n", renamed)
	n500Report(stocks, syms, missing, skipped, from, split, to, *costPct, *slipBps, *short, *list)
	return n500CSV(*out, stocks, skipped, missing)
}

// joinRenames orders the segments of one company's history, traded under
// different symbols, and joins them. Each symbol's prices are adjusted on
// their own, so a join whose close-to-open gap exceeds 20% is refused: the
// old symbol most likely missed an adjustment made after the rename.
func joinRenames(segs [][]data.PanelDay) ([]data.PanelDay, bool, error) {
	if len(segs) <= 1 {
		if len(segs) == 0 {
			return nil, false, nil
		}
		return segs[0], false, nil
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i][0].Date.Before(segs[j][0].Date) })
	out := append([]data.PanelDay(nil), segs[0]...)
	for _, s := range segs[1:] {
		for len(s) > 0 && !out[len(out)-1].Date.Before(s[0].Date) {
			s = s[1:] // overlap: keep the older symbol's day
		}
		if len(s) == 0 {
			continue
		}
		if g := s[0].O/out[len(out)-1].C - 1; math.Abs(g) > 0.2 {
			return nil, false, fmt.Errorf("rename from %s jumps %.0f%%", out[len(out)-1].Symbol, 100*g)
		}
		out = append(out, s...)
	}
	return out, true, nil
}

func pairName(k int) string {
	if k == len(n500Pairs) {
		return "B&H"
	}
	return fmt.Sprintf("%d/%d", n500Pairs[k][0], n500Pairs[k][1])
}

func median(x []float64) float64 {
	if len(x) == 0 {
		return math.NaN()
	}
	y := append([]float64(nil), x...)
	sort.Float64s(y)
	if len(y)%2 == 1 {
		return y[len(y)/2]
	}
	return (y[len(y)/2-1] + y[len(y)/2]) / 2
}

func n500Report(stocks []n500Stock, syms, missing []string, skipped map[string]string, from, split, to market.Date, costPct, slipBps float64, short bool, list string) {
	np := len(n500Pairs)
	mode := "Long-only"
	if short {
		mode = "Long and short"
	}
	fmt.Printf("%s daily EMA crossovers on the stocks in %s (%d listed); buy and hold is long only\n", mode, list, len(syms))
	fmt.Printf("train %s to %s picks each stock's pair by Sharpe; test %s to %s scores it\n", from, split.AddDays(-1), split, to)
	fmt.Printf("long when fast > slow at the close, else short or flat, filled next open; %.2f%% round trip + %.0f bps slippage each side\n", costPct, slipBps)
	fmt.Printf("%d stocks tested, %d with too little history, %d with no price data under that symbol\n\n", len(stocks), len(skipped), len(missing))
	if len(stocks) == 0 {
		return
	}

	fmt.Printf("All stocks, medians per pair\n%-8s %9s %9s %8s %8s %8s %6s %6s %9s %9s\n", "pair", "trainCAGR", "testCAGR",
		"trainSh", "testSh", "testDD", "trd/yr", "expo", "beatB&H*", "picked")
	picked := make([]int, np+1)
	for _, s := range stocks {
		picked[s.pick]++
	}
	for k := 0; k <= np; k++ {
		var tc, ec, ts, es, dd, tpy, ex []float64
		beat := 0
		for _, s := range stocks {
			tc, ec = append(tc, s.train[k].cagr), append(ec, s.test[k].cagr)
			ts, es = append(ts, s.train[k].sharpe), append(es, s.test[k].sharpe)
			dd, ex = append(dd, s.test[k].dd), append(ex, s.test[k].exposure)
			tpy = append(tpy, float64(s.test[k].trades)/float64(s.test[k].days)*252)
			if s.test[k].sharpe > s.test[np].sharpe {
				beat++
			}
		}
		fmt.Printf("%-8s %8.1f%% %8.1f%% %8.2f %8.2f %7.0f%% %6.1f %5.0f%% %8.0f%% %9d\n", pairName(k), 100*median(tc), 100*median(ec),
			median(ts), median(es), 100*median(dd), median(tpy), 100*median(ex), 100*float64(beat)/float64(len(stocks)), picked[k])
	}
	fmt.Println("* share of stocks where the pair's test Sharpe beat buy and hold's")

	// Does picking a pair per stock beat one pair for everyone?
	var pickS, pickC, bhS, bhC, ranks []float64
	posTest, beatBH := 0, 0
	for _, s := range stocks {
		p, bh := s.test[s.pick], s.test[np]
		pickS, pickC = append(pickS, p.sharpe), append(pickC, p.cagr)
		bhS, bhC = append(bhS, bh.sharpe), append(bhC, bh.cagr)
		ranks = append(ranks, s.pickTestRank)
		if p.cagr > 0 {
			posTest++
		}
		if p.sharpe > bh.sharpe {
			beatBH++
		}
	}
	mean := 0.0
	for _, r := range ranks {
		mean += r
	}
	mean /= float64(len(ranks))
	fmt.Printf("\nEach stock's train-best pair, on the test years\n")
	fmt.Printf("median test Sharpe %.2f (buy and hold %.2f), median test CAGR %.1f%% (buy and hold %.1f%%)\n",
		median(pickS), median(bhS), 100*median(pickC), 100*median(bhC))
	fmt.Printf("positive in test: %d of %d; beat buy and hold's Sharpe: %d of %d\n", posTest, len(stocks), beatBH, len(stocks))
	fmt.Printf("its test rank among the %d pairs averages %.2f (0 worst, 1 best, 0.50 = no better than a random pair)\n", np, mean)
	// A rough null: under no persistence the mean of n uniform ranks has sd 1/sqrt(12n).
	fmt.Printf("that is %.1f standard errors from 0.50\n", (mean-0.5)/(1/math.Sqrt(12*float64(len(ranks)))))

	// Persistence of the best pair: of stocks whose train winner was pair k,
	// how often was k also in the test's top three?
	top3 := 0
	for _, s := range stocks {
		order := make([]int, np)
		for k := range order {
			order[k] = k
		}
		sort.Slice(order, func(i, j int) bool { return s.test[order[i]].sharpe > s.test[order[j]].sharpe })
		for _, k := range order[:3] {
			if k == s.pick {
				top3++
			}
		}
	}
	fmt.Printf("train-best pair was among the test's top 3: %d of %d (chance %.0f%%)\n", top3, len(stocks), 300/float64(np))
}

// n500CSV writes a row per tested stock and, after them, a row per member
// left out with the reason.
func n500CSV(path string, stocks []n500Stock, skipped map[string]string, missing []string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	head := []string{"symbol", "train_from", "pick", "pick_train_sharpe", "pick_train_cagr", "pick_test_sharpe", "pick_test_cagr",
		"pick_test_dd", "pick_test_trades", "pick_test_rank", "test_best", "test_best_sharpe", "bh_train_cagr", "bh_test_cagr",
		"bh_train_sharpe", "bh_test_sharpe", "bh_test_dd"}
	for k := range n500Pairs {
		head = append(head, "train_sharpe_"+pairName(k), "test_sharpe_"+pairName(k))
	}
	w.Write(head)
	np := len(n500Pairs)
	ff := func(x float64) string { return fmt.Sprintf("%.4f", x) }
	for _, s := range stocks {
		best := 0
		for k := 1; k < np; k++ {
			if s.test[k].sharpe > s.test[best].sharpe {
				best = k
			}
		}
		p, bh := s.pick, np
		row := []string{s.symbol, s.trainFrom.String(), pairName(p), ff(s.train[p].sharpe), ff(s.train[p].cagr), ff(s.test[p].sharpe),
			ff(s.test[p].cagr), ff(s.test[p].dd), fmt.Sprint(s.test[p].trades), ff(s.pickTestRank), pairName(best),
			ff(s.test[best].sharpe), ff(s.train[bh].cagr), ff(s.test[bh].cagr), ff(s.train[bh].sharpe), ff(s.test[bh].sharpe), ff(s.test[bh].dd)}
		for k := range n500Pairs {
			row = append(row, ff(s.train[k].sharpe), ff(s.test[k].sharpe))
		}
		w.Write(row)
	}
	var left []string
	for s := range skipped {
		left = append(left, s)
	}
	sort.Strings(left)
	for _, s := range left {
		w.Write([]string{s, "", "skipped: " + skipped[s]})
	}
	for _, s := range missing {
		w.Write([]string{s, "", "skipped: no daily prices to " + "the end of the test"})
	}
	w.Flush()
	return w.Error()
}
