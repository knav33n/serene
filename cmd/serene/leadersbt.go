package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"sort"
	"strings"

	"serene/internal/data"
	"serene/internal/market"
	"serene/internal/nse"
	"serene/internal/regime"
)

// lbStock is one symbol's history on the index panel's calendar.
type lbStock struct {
	sym   string
	close []float64 // carried over days it didn't trade; NaN before its first panel day
	ath   []float64 // highest close over its whole history up to that day
	since []int     // stock trading days of history up to that day
	last  []bool    // traded that day
	elig  []bool    // among the top by turnover in the previous month
	bad   []int     // days so far whose close moved beyond what circuit limits allow: bad adjustments
}

// lbGroup accumulates, over signal dates, a group's average forward return
// minus the universe's.
type lbGroup struct {
	n        int     // signal dates with members
	members  int     // stock-dates
	ex, exSq float64 // per-date excess over the universe, percent
	hit      int     // dates the group beat the universe
}

func (g *lbGroup) add(ex float64, members int) {
	g.n++
	g.members += members
	g.ex += ex
	g.exSq += ex * ex
	if ex > 0 {
		g.hit++
	}
}

func (g *lbGroup) String() string {
	if g.n == 0 {
		return "no dates"
	}
	n := float64(g.n)
	m := g.ex / n
	sd := math.Sqrt(math.Max(g.exSq/n-m*m, 0))
	t := math.NaN()
	if sd > 0 {
		t = m / sd * math.Sqrt(n)
	}
	return fmt.Sprintf("dates %3d  avg members %5.1f  excess %+6.2f%%  beat %3.0f%%  t %5.2f (overlapping)",
		g.n, float64(g.members)/n, m, float64(g.hit)/n*100, t)
}

// runLeadersBacktest tests the screen from `market leaders` on history: on the
// first trading day of each month, do stocks (and sector indices) within 5% of
// their all-time high and ahead of Smallcap 100 over the window beat the rest
// over the next 3, 6 and 12 months? The universe is the top stocks by turnover
// in the previous month, so it holds no survivorship bias.
func runLeadersBacktest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("research leaders", flag.ContinueOnError)
	marketDir := fs.String("market", "data/market", "NSE index history")
	equitiesDir := fs.String("equities", "data/equities", "equities data directory")
	catalog := fs.String("catalog", "configs/indices.yaml", "index catalog")
	bench := fs.String("bench", regime.LeaderBench, "benchmark index")
	top := fs.Int("top", 500, "universe: top stocks by last month's turnover")
	pick := fs.Int("pick", 20, "portfolio variant holding the best leaders by excess")
	costPct := fs.Float64("cost-pct", 0.24, "round-trip delivery charges, percent")
	slipBps := fs.Float64("slip-bps", 10, "slippage each side, basis points")
	weakPct := fs.Float64("weak-pct", 10, "a correction: benchmark at least this far below its all-time high")
	splitS := fs.String("split", "2020-01-01", "first date of the second half")
	if err := fs.Parse(args); err != nil {
		return err
	}
	split, err := market.ParseDate(*splitS)
	if err != nil {
		return err
	}
	cat, err := nse.LoadCatalog(*catalog)
	if err != nil {
		return err
	}
	ms, err := data.Open(*marketDir)
	if err != nil {
		return err
	}
	defer ms.Close()
	rows, err := loadIndexRows(ctx, ms)
	if err != nil {
		return err
	}
	p := regime.NewPanel(rows, marketMeta(cat))
	b, ok := p.Series[*bench]
	if !ok {
		return fmt.Errorf("no %s in %s", *bench, *marketDir)
	}
	nd := len(p.Dates)
	pos := make(map[market.Date]int, nd)
	for k, d := range p.Dates {
		pos[d] = k
	}

	es, err := data.Open(*equitiesDir)
	if err != nil {
		return err
	}
	defer es.Close()
	panel, err := es.EquityPanel(ctx, market.Date{Year: 1994, Month: 11, Day: 1}, p.Dates[nd-1], *top)
	if err != nil {
		return err
	}
	var stocks []*lbStock
	for lo := 0; lo < len(panel); {
		hi := lo
		for hi < len(panel) && panel[hi].Symbol == panel[lo].Symbol {
			hi++
		}
		h := panel[lo:hi]
		lo = hi
		s := &lbStock{sym: h[0].Symbol, close: lbNaNs(nd), ath: lbNaNs(nd), since: make([]int, nd),
			last: make([]bool, nd), elig: make([]bool, nd), bad: make([]int, nd)}
		high, count, any, badN := 0.0, 0, false, 0
		k := 0
		for j := b.First; j < nd; j++ {
			for k < len(h) && !p.Dates[j].Before(h[k].Date) {
				count++
				if k > 0 {
					if g := h[k].C / h[k-1].C; g > 1.45 || g < 0.6 {
						// A missed adjustment: older prices aren't comparable, so the
						// high starts again and its history counts from here.
						badN++
						high, count = 0, 1
					}
				}
				high = math.Max(high, h[k].C)
				if h[k].Date == p.Dates[j] {
					s.close[j] = h[k].C
					s.last[j] = true
					s.elig[j] = h[k].Eligible
				}
				k++
			}
			if count == 0 {
				continue
			}
			if math.IsNaN(s.close[j]) && j > 0 {
				s.close[j] = s.close[j-1]
			}
			if math.IsNaN(s.close[j]) {
				// first panel day falls after its last trade before the panel
				s.close[j] = h[k-1].C
			}
			s.ath[j], s.since[j], s.bad[j] = high, count, badN
			any = true
		}
		if any {
			stocks = append(stocks, s)
		}
	}
	dirty := 0
	for _, s := range stocks {
		if s.bad[nd-1] > 0 {
			dirty++
		}
	}
	fmt.Printf("%d symbols ever in the top %d by turnover (%d with a close-to-close move beyond +45%%/-40%%; no interval may span one, and highs restart after it); benchmark %s from %s\n",
		len(stocks), *top, dirty, *bench, p.Dates[b.First])

	// Signal days: the first panel day of each month with a year of benchmark history.
	var sig []int
	for j := b.First + 252; j < nd; j++ {
		if p.Dates[j].Month != p.Dates[j-1].Month {
			sig = append(sig, j)
		}
	}
	horizons := []struct {
		label string
		days  int
	}{{"3M", 63}, {"6M", 126}, {"12M", 252}}
	type key struct {
		group, regime, half string
		h                   int
	}
	acc := map[key]*lbGroup{}
	get := func(k key) *lbGroup {
		if acc[k] == nil {
			acc[k] = &lbGroup{}
		}
		return acc[k]
	}
	fwd := func(c []float64, i, h int) float64 {
		// Enter at the next close, after the signal day's close is known.
		if i+1+h >= nd || math.IsNaN(c[i+1]) || math.IsNaN(c[i+1+h]) {
			return math.NaN()
		}
		return (c[i+1+h]/c[i+1] - 1) * 100
	}
	type pickDay struct {
		i                   int
		leaders, best, univ []*lbStock
		weak                bool
	}
	var days []pickDay
	weakDates := 0
	for _, i := range sig {
		w := regime.WindowFor(b.Close, b.First, i)
		dd := (b.Close[i]/maxOf(b.Close[b.First:i+1]) - 1) * 100
		weak := w.SincePeak && dd <= -*weakPct
		if weak {
			weakDates++
		}
		bw := (b.Close[i]/b.Close[w.From] - 1) * 100
		type cand struct {
			s      *lbStock
			excess float64
			near   bool
		}
		var univ []cand
		for _, s := range stocks {
			if !s.elig[i] || !s.last[i] || s.since[i] < 252 || math.IsNaN(s.close[w.From]) || s.bad[i] != s.bad[w.From] {
				continue
			}
			ex := (s.close[i]/s.close[w.From]-1)*100 - bw
			near := (s.close[i]/s.ath[i]-1)*100 >= -regime.LeaderNearHigh
			univ = append(univ, cand{s, ex, near})
		}
		if len(univ) < 50 {
			continue
		}
		pd := pickDay{i: i, weak: weak}
		for _, c := range univ {
			pd.univ = append(pd.univ, c.s)
			if c.near && c.excess > 0 {
				pd.leaders = append(pd.leaders, c.s)
			}
		}
		sort.Slice(univ, func(a, c int) bool { return univ[a].excess > univ[c].excess })
		for _, c := range univ {
			if c.near && c.excess > 0 && len(pd.best) < *pick {
				pd.best = append(pd.best, c.s)
			}
		}
		days = append(days, pd)
		reg := "normal"
		if weak {
			reg = "correction"
		}
		half := "H1"
		if !p.Dates[i].Before(split) {
			half = "H2"
		}
		for hk, h := range horizons {
			avg := func(ss []*lbStock) (float64, int) {
				sum, n := 0.0, 0
				for _, s := range ss {
					if r := fwd(s.close, i, h.days); !math.IsNaN(r) && s.bad[i+1+h.days] == s.bad[i+1] {
						sum += r
						n++
					}
				}
				if n == 0 {
					return math.NaN(), 0
				}
				return sum / float64(n), n
			}
			u, un := avg(pd.univ)
			if un < 50 {
				continue
			}
			nearOnly, exOnly := []*lbStock{}, []*lbStock{}
			for _, c := range univ {
				if c.near {
					nearOnly = append(nearOnly, c.s)
				}
				if c.excess > 0 {
					exOnly = append(exOnly, c.s)
				}
			}
			groups := map[string][]*lbStock{"leaders": pd.leaders, fmt.Sprintf("top %d leaders", *pick): pd.best,
				"near high (any excess)": nearOnly, "ahead of bench (any high)": exOnly}
			for name, ss := range groups {
				a, n := avg(ss)
				if n == 0 {
					continue
				}
				for _, r := range []string{reg, "all"} {
					for _, hf := range []string{half, "all"} {
						get(key{name, r, hf, hk}).add(a-u, n)
					}
				}
			}
			if br := fwd(b.Close, i, h.days); !math.IsNaN(br) {
				for _, r := range []string{reg, "all"} {
					get(key{"universe vs " + *bench, r, "all", hk}).add(u-br, un)
				}
			}
		}
	}
	fmt.Printf("%d signal dates %s..%s, %d in a correction (benchmark >=%.0f%% below its high, window since the peak)\n",
		len(days), p.Dates[days[0].i], p.Dates[days[len(days)-1].i], weakDates, *weakPct)
	fmt.Println("\n== forward return minus the equal-weight universe's, averaged over signal dates ==")
	names := []string{"leaders", fmt.Sprintf("top %d leaders", *pick), "near high (any excess)", "ahead of bench (any high)", "universe vs " + *bench}
	for hk, h := range horizons {
		fmt.Printf("-- %s after entry --\n", h.label)
		for _, name := range names {
			for _, r := range []string{"all", "correction", "normal"} {
				halves := []string{"all", "H1", "H2"}
				if strings.HasPrefix(name, "universe") {
					halves = []string{"all"}
				}
				for _, hf := range halves {
					g := acc[key{name, r, hf, hk}]
					if g == nil {
						continue
					}
					label := name + " / " + r
					if hf != "all" {
						label += " / " + hf
					}
					fmt.Printf("  %-52s %s\n", label, g)
				}
			}
		}
	}

	// Portfolio: equal weight, rebalanced at each signal date's next close.
	side := *costPct/200 + *slipBps/1e4
	fmt.Printf("\n== monthly rebalanced equal-weight portfolios, %.2f%% cost per side on turnover ==\n", side*100)
	type pf struct {
		name string
		pick func(pickDay) []*lbStock
		only func(pickDay) bool // nil: always hold; else hold the universe otherwise
	}
	pfs := []pf{
		{"universe (top 500 equal weight)", func(d pickDay) []*lbStock { return d.univ }, nil},
		{"leaders", func(d pickDay) []*lbStock { return d.leaders }, nil},
		{fmt.Sprintf("top %d leaders", *pick), func(d pickDay) []*lbStock { return d.best }, nil},
		{fmt.Sprintf("top %d leaders in corrections, universe otherwise", *pick), func(d pickDay) []*lbStock { return d.best },
			func(d pickDay) bool { return d.weak }},
	}
	for _, f := range pfs {
		for _, hf := range []string{"all", "H1", "H2"} {
			eq, peak, dd := 1.0, 1.0, 0.0
			beq := 1.0
			var held map[*lbStock]bool
			from, to := -1, -1
			for k := 0; k+1 < len(days); k++ {
				d, nx := days[k], days[k+1]
				in := hf == "all" || (hf == "H1") == p.Dates[d.i].Before(split)
				if !in {
					continue
				}
				if from < 0 {
					from = d.i
				}
				to = nx.i
				set := f.pick(d)
				if f.only != nil && !f.only(d) {
					set = d.univ
				}
				if len(set) == 0 {
					held = nil
					continue // cash
				}
				now := map[*lbStock]bool{}
				for _, s := range set {
					now[s] = true
				}
				changed := 0
				for s := range now {
					if !held[s] {
						changed++
					}
				}
				for s := range held {
					if !now[s] {
						changed++
					}
				}
				turnover := float64(changed) / float64(len(set))
				a, c := d.i+1, nx.i+1
				if c >= nd {
					break
				}
				sum, n := 0.0, 0
				for _, s := range set {
					if !math.IsNaN(s.close[a]) && !math.IsNaN(s.close[c]) && s.bad[c] == s.bad[a] {
						sum += s.close[c] / s.close[a]
						n++
					}
				}
				if n == 0 {
					continue
				}
				eq *= sum / float64(n) * (1 - side*turnover)
				beq *= b.Close[c] / b.Close[a]
				peak = math.Max(peak, eq)
				dd = math.Max(dd, 1-eq/peak)
				held = now
			}
			if from < 0 {
				continue
			}
			yrs := float64(to-from) / 252
			fmt.Printf("  %-52s %-3s %s..%s  CAGR %6.1f%%  maxDD %3.0f%% (monthly)  | %s CAGR %5.1f%%\n", f.name, hf,
				p.Dates[from], p.Dates[to], (math.Pow(eq, 1/yrs)-1)*100, dd*100, *bench, (math.Pow(beq, 1/yrs)-1)*100)
		}
	}

	// Sector indices, same screen.
	fmt.Println("\n== sector and thematic indices: leaders' forward return minus all screened indices' ==")
	for _, h := range horizons {
		g := map[string]*lbGroup{"all": {}, "correction": {}, "normal": {}}
		gb := map[string]*lbGroup{"all": {}, "correction": {}, "normal": {}}
		for _, d := range days {
			rep, ok := regime.Leaders(p, d.i, *bench, nse.GroupSectoral, nse.GroupThematic)
			if !ok {
				continue
			}
			var all, lead []float64
			for _, r := range rep.Sectors {
				if notASector(r.Name) || d.i-r.First < 252 {
					continue
				}
				f := fwd(p.Series[r.Name].Close, d.i, h.days)
				if math.IsNaN(f) {
					continue
				}
				all = append(all, f)
				if r.Leader {
					lead = append(lead, f)
				}
			}
			br := fwd(b.Close, d.i, h.days)
			if len(all) < 10 || len(lead) == 0 || math.IsNaN(br) {
				continue
			}
			ex := mean(lead) - mean(all)
			reg := "normal"
			if d.weak {
				reg = "correction"
			}
			g[reg].add(ex, len(lead))
			g["all"].add(ex, len(lead))
			gb[reg].add(mean(lead)-br, len(lead))
			gb["all"].add(mean(lead)-br, len(lead))
		}
		for _, r := range []string{"all", "correction", "normal"} {
			fmt.Printf("  %-4s %-11s vs all indices: %s\n", h.label, r, g[r])
			fmt.Printf("  %-4s %-11s vs %s: %s\n", h.label, r, *bench, gb[r])
		}
	}
	return nil
}

func maxOf(x []float64) float64 {
	m := math.Inf(-1)
	for _, v := range x {
		if !math.IsNaN(v) {
			m = math.Max(m, v)
		}
	}
	return m
}

func mean(x []float64) float64 {
	s := 0.0
	for _, v := range x {
		s += v
	}
	return s / float64(len(x))
}

func lbNaNs(n int) []float64 {
	v := make([]float64, n)
	for i := range v {
		v[i] = math.NaN()
	}
	return v
}
