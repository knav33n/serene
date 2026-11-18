package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"serene/internal/data"
	"serene/internal/market"
	"serene/internal/nse"
	"serene/internal/regime"
)

// industryIndex maps each Industry in the niftyindices constituent files to
// the closest sectoral index, so a sector row can point to its stocks.
var industryIndex = map[string]string{
	"Automobile and Auto Components":    "Nifty Auto",
	"Capital Goods":                     "Nifty Capital Goods",
	"Chemicals":                         "Nifty Chemicals",
	"Construction":                      "Nifty Construction",
	"Construction Materials":            "Nifty Cement",
	"Consumer Durables":                 "Nifty Consumer Durables",
	"Consumer Services":                 "Nifty Consumer Services",
	"Fast Moving Consumer Goods":        "Nifty FMCG",
	"Financial Services":                "Nifty Financial Services",
	"Healthcare":                        "Nifty Healthcare Index",
	"Information Technology":            "Nifty IT",
	"Media Entertainment & Publication": "Nifty Media",
	"Metals & Mining":                   "Nifty Metal",
	"Oil Gas & Consumable Fuels":        "Nifty Oil & Gas",
	"Power":                             "Nifty Power",
	"Realty":                            "Nifty Realty",
	"Services":                          "Nifty Services Sector",
	"Telecommunication":                 "Nifty Telecommunications",
}

// notASector is true for thematic indices that cut across sectors by a rule
// other than business: faith screens, ESG scores, business groups, REITs and
// the SME board.
func notASector(name string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{"shariah", "esg", "corporate group", "reit", "sme emerge", "ahimsa", "select 5"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// stockLeader is one constituent on the report date.
type stockLeader struct {
	regime.LeaderRow
	industry, isin string
	fund           fundamentals
}

// runMarketLeaders screens sectoral and thematic indices, then the Nifty 500's
// stocks, for strength while the small-cap market is off its high.
func runMarketLeaders(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("market leaders", flag.ContinueOnError)
	date := fs.String("date", "", "report date, default the latest index close")
	marketDir := fs.String("market", "data/market", "NSE index history")
	equitiesDir := fs.String("equities", "data/equities", "equities data directory")
	catalog := fs.String("catalog", "configs/indices.yaml", "index catalog")
	list := fs.String("list", "configs/nifty500.csv", "constituent list with an Industry column")
	bench := fs.String("bench", regime.LeaderBench, "benchmark index")
	reports := fs.String("reports", "reports", "report directory")
	useSlack := fs.Bool("slack", false, "post the summary to SLACK_CHANNEL")
	top := fs.Int("top", 15, "sectors shown in the summary")
	fundFor := fs.String("fundamentals", "leaders", "Upstox fundamentals for: leaders, all or off")
	if err := fs.Parse(args); err != nil {
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
	if len(p.Dates) == 0 {
		return fmt.Errorf("no index history in %s; run serene market fetch", *marketDir)
	}
	asOf := market.DateOf(time.Now())
	if *date != "" {
		if asOf, err = market.ParseDate(*date); err != nil {
			return err
		}
	}
	i := len(p.Dates) - 1
	for i >= 0 && asOf.Before(p.Dates[i]) {
		i--
	}
	if i < 0 {
		return fmt.Errorf("no index close on or before %s", asOf)
	}
	day := p.Dates[i]
	if day != asOf && *date != "" {
		fmt.Printf("warning: no index close on %s; using %s\n", asOf, day)
	}
	rep, ok := regime.Leaders(p, i, *bench, nse.GroupSectoral, nse.GroupThematic)
	if !ok {
		return fmt.Errorf("%s has no close on %s", *bench, day)
	}
	rep.Sectors = slices.DeleteFunc(rep.Sectors, func(r regime.LeaderRow) bool { return notASector(r.Name) })

	stocks, notes, err := screenStocks(ctx, *equitiesDir, *list, p, i, *bench, rep.Window)
	if err != nil {
		return err
	}
	if *fundFor != "off" {
		var isins []string
		for _, s := range stocks {
			if s.isin != "" && (s.Leader || *fundFor == "all") {
				isins = append(isins, s.isin)
			}
		}
		got, more, err := loadFundamentals(ctx, filepath.Join(*equitiesDir, "fundamentals"), isins)
		if err != nil {
			return err
		}
		for k := range stocks {
			stocks[k].fund = got[stocks[k].isin]
		}
		notes = append(notes, more...)
		if *date != "" {
			notes = append(notes, "fundamentals are today's, not as of the report date")
		}
	}
	text := leadersSummary(p, day, rep, stocks, notes, *top)
	fmt.Print(text)
	dir := filepath.Join(*reports, "leaders")
	if err := writeSectorCSV(filepath.Join(dir, day.String()+"-sectors.csv"), p, rep); err != nil {
		return err
	}
	if err := writeStockCSV(filepath.Join(dir, day.String()+"-stocks.csv"), p, stocks); err != nil {
		return err
	}
	fmt.Println("csv:", filepath.Join(dir, day.String()+"-{sectors,stocks}.csv"))
	if *useSlack {
		dm, err := openSlack(ctx)
		if err != nil {
			return err
		}
		if err := dm.client.Post(ctx, dm.channel, "```\n"+text+"```"); err != nil {
			return err
		}
		fmt.Println("posted to Slack")
	}
	return nil
}

// screenStocks scores each constituent on the panel's dates. All-time-high
// figures come from the stock's whole history, which starts before the index
// panel; window, excess and relative strength use the panel's benchmark.
func screenStocks(ctx context.Context, dir, list string, p *regime.Panel, i int, bench string, w regime.LeaderWindow) ([]stockLeader, []string, error) {
	cols, err := readListColumns(list, "Symbol", "Industry", "ISIN Code")
	if err != nil {
		return nil, nil, err
	}
	syms, inds, isins := cols[0], cols[1], cols[2]
	store, err := data.Open(dir)
	if err != nil {
		return nil, nil, err
	}
	defer store.Close()
	owner := map[string]string{}
	for _, s := range syms {
		owner[s] = s
	}
	for _, s := range syms {
		al, err := store.EquityAliases(ctx, s)
		if err != nil {
			return nil, nil, err
		}
		for _, a := range al {
			if _, ok := owner[a]; !ok {
				owner[a] = s
			}
		}
	}
	all := make([]string, 0, len(owner))
	for s := range owner {
		all = append(all, s)
	}
	days, err := store.EquityAdjusted(ctx, all, p.Dates[i])
	if err != nil {
		return nil, nil, err
	}
	bySym := map[string][][]data.PanelDay{}
	var last market.Date
	for lo := 0; lo < len(days); {
		hi := lo
		for hi < len(days) && days[hi].Symbol == days[lo].Symbol {
			hi++
		}
		bySym[owner[days[lo].Symbol]] = append(bySym[owner[days[lo].Symbol]], days[lo:hi])
		if d := days[hi-1].Date; last.Before(d) {
			last = d
		}
		lo = hi
	}
	var notes []string
	if last.Before(p.Dates[i]) {
		notes = append(notes, fmt.Sprintf("equity data ends %s, before the index date %s; run serene equities fetch", last, p.Dates[i]))
	}
	pos := make(map[market.Date]int, len(p.Dates))
	for k, d := range p.Dates {
		pos[d] = k
	}
	b := p.Series[bench]
	var out []stockLeader
	var missing, stale, short []string
	for k, sym := range syms {
		hist, _, err := joinRenames(bySym[sym])
		if err != nil {
			notes = append(notes, sym+": "+err.Error())
			continue
		}
		if len(hist) == 0 {
			missing = append(missing, sym)
			continue
		}
		if pk, ok := pos[hist[len(hist)-1].Date]; !ok || i-pk > 5 {
			stale = append(stale, sym)
			continue
		}
		// Closes on the panel's dates, carried over days the stock didn't trade.
		closes := make([]float64, len(p.Dates))
		for j := range closes {
			closes[j] = math.NaN()
		}
		first := -1
		for _, d := range hist {
			if j, ok := pos[d.Date]; ok {
				closes[j] = d.C
				if first < 0 {
					first = j
				}
			}
		}
		first = max(first, b.First)
		if first < 0 || first > i-5 {
			short = append(short, sym)
			continue
		}
		for j := first + 1; j <= i; j++ {
			if math.IsNaN(closes[j]) {
				closes[j] = closes[j-1]
			}
		}
		if math.IsNaN(closes[first]) {
			short = append(short, sym)
			continue
		}
		r := regime.LeaderStats(sym, closes, b.Close, first, i, w)
		// All-time high over the stock's own full history.
		ath := 0
		for j, d := range hist {
			if d.C >= hist[ath].C {
				ath = j
			}
		}
		r.FromATH = (hist[len(hist)-1].C/hist[ath].C - 1) * 100
		r.ATHAge = len(hist) - 1 - ath
		r.NewHigh = r.ATHAge < 20
		r.Leader = r.FromATH >= -regime.LeaderNearHigh && r.Excess > 0
		out = append(out, stockLeader{LeaderRow: r, industry: inds[k], isin: isins[k]})
	}
	if len(missing) > 0 {
		notes = append(notes, fmt.Sprintf("%d with no prices: %s", len(missing), strings.Join(missing, " ")))
	}
	if len(stale) > 0 {
		notes = append(notes, fmt.Sprintf("%d not trading lately: %s", len(stale), strings.Join(stale, " ")))
	}
	if len(short) > 0 {
		notes = append(notes, fmt.Sprintf("%d with under a week of history: %s", len(short), strings.Join(short, " ")))
	}
	return out, notes, nil
}

func pct(v float64) string {
	if math.IsNaN(v) {
		return "—"
	}
	return fmt.Sprintf("%+.1f%%", v)
}

func leadersSummary(p *regime.Panel, day market.Date, rep regime.LeaderReport, stocks []stockLeader, notes []string, top int) string {
	var sb strings.Builder
	w := rep.Window
	f := func(format string, a ...any) { fmt.Fprintf(&sb, format, a...) }
	f("Sector leaders %s\n", day)
	bn := rep.Bench[0]
	if w.SincePeak {
		f("Window: since %s's all-time high on %s (%d trading days)\n", bn.Name, p.Dates[w.From], w.To-w.From)
	} else {
		f("Window: last %d trading days, from %s (%s is at or near its high)\n", w.To-w.From, p.Dates[w.From], bn.Name)
	}
	for _, r := range rep.Bench {
		f("  %-20s %6s from high  3M %6s  6M %6s  window %6s\n", r.Name, pct(r.FromATH), pct(r.R3M), pct(r.R6M), pct(r.Window))
	}
	leaders := 0
	for _, r := range rep.Sectors {
		if r.Leader {
			leaders++
		}
	}
	f("\nSectors: %d of %d are leaders (within %.0f%% of their high and ahead of %s). Highs count from 2012 or the index's launch.\n",
		leaders, len(rep.Sectors), regime.LeaderNearHigh, bn.Name)
	line := func(r regime.LeaderRow) {
		rs := ""
		if r.RSHigh {
			rs = "yes"
		}
		f("  %-36s %7s %7s %7s %5s %5d\n", trunc(r.Name, 36), pct(r.FromATH), pct(r.Window), pct(r.Excess), rs, p.Dates[r.First].Year)
	}
	f("  %-36s %7s %7s %7s %5s %5s\n", "", "high", "window", "excess", "RS1Y", "since")
	shown := 0
	for _, r := range rep.Sectors {
		if r.Leader && shown < top {
			line(r)
			shown++
		}
	}
	if leaders > shown {
		f("  … %d more leaders in the CSV\n", leaders-shown)
	}
	f("Ahead of %s but more than %.0f%% below their high:\n", bn.Name, regime.LeaderNearHigh)
	shown = 0
	for _, r := range rep.Sectors {
		if !r.Leader && r.Excess > 0 && shown < 5 {
			line(r)
			shown++
		}
	}

	type ind struct {
		name             string
		n, near, leaders int
		median           float64
		rows             []stockLeader
	}
	byInd := map[string]*ind{}
	for _, s := range stocks {
		g := byInd[s.industry]
		if g == nil {
			g = &ind{name: s.industry}
			byInd[s.industry] = g
		}
		g.n++
		if s.FromATH >= -regime.LeaderNearHigh {
			g.near++
		}
		if s.Leader {
			g.leaders++
		}
		g.rows = append(g.rows, s)
	}
	var inds []*ind
	for _, g := range byInd {
		var ex []float64
		for _, s := range g.rows {
			if !math.IsNaN(s.Excess) {
				ex = append(ex, s.Excess)
			}
		}
		g.median = math.NaN()
		if len(ex) > 0 {
			sort.Float64s(ex)
			g.median = ex[len(ex)/2]
		}
		inds = append(inds, g)
	}
	// Most members near their highs first, then the best median excess.
	sort.Slice(inds, func(a, b int) bool {
		sa, sb := float64(inds[a].near)/float64(inds[a].n), float64(inds[b].near)/float64(inds[b].n)
		if sa != sb {
			return sa > sb
		}
		x, y := inds[a].median, inds[b].median
		if math.IsNaN(x) != math.IsNaN(y) {
			return !math.IsNaN(x)
		}
		return x > y || (x == y && inds[a].name < inds[b].name)
	})
	sectorOf := map[string]regime.LeaderRow{}
	for _, r := range rep.Sectors {
		sectorOf[r.Name] = r
	}
	nLead, nQual, nFund := 0, 0, 0
	for _, s := range stocks {
		if s.Leader {
			nLead++
			if s.fund.ok {
				nFund++
			}
			if s.fund.quality {
				nQual++
			}
		}
	}
	if nFund > 0 {
		f("\n%d of %d leader stocks have ROE and ROCE of at least %.0f%% (Q below; %d with Upstox fundamentals).", nQual, nLead, qualityMin, nFund)
		f(" Growth is the latest year over the one before, NP Q the latest quarter over the previous one.\n")
	}
	f("\nNifty 500 industries, by share of members within %.0f%% of their all-time high:\n", regime.LeaderNearHigh)
	for _, g := range inds {
		idx := ""
		if name, ok := industryIndex[g.name]; ok {
			if r, ok := sectorOf[name]; ok {
				state := "not leading"
				if r.Leader {
					state = "leader"
				}
				idx = fmt.Sprintf("  [%s: %s]", name, state)
			}
		}
		f("  %-34s %3d stocks, %3d near high (%3.0f%%), median excess %6s%s\n",
			trunc(g.name, 34), g.n, g.near, float64(g.near)/float64(g.n)*100, pct(g.median), idx)
		var ls []stockLeader
		for _, s := range g.rows {
			if s.Leader {
				ls = append(ls, s)
			}
		}
		sort.Slice(ls, func(a, b int) bool { return ls[a].Excess > ls[b].Excess })
		if len(ls) == 0 {
			continue
		}
		for k, s := range ls {
			if k == 8 {
				f("      … %d more in the CSV\n", len(ls)-8)
				break
			}
			nh := ""
			if s.NewHigh {
				nh = "new high"
			}
			f("      %-12s %7s %-8s %s\n", s.Name, pct(s.Excess), nh, fundLine(s.fund))
		}
	}
	for _, n := range notes {
		f("note: %s\n", n)
	}
	return sb.String()
}

func num0(v float64, suffix string) string {
	if math.IsNaN(v) {
		return "—"
	}
	return fmt.Sprintf("%.0f%s", v, suffix)
}

func fundLine(fd fundamentals) string {
	if !fd.ok {
		return ""
	}
	q := " "
	if fd.quality {
		q = "Q"
	}
	return fmt.Sprintf("%s P/E %5s ROE %4s ROCE %4s | rev %6s NP %6s 3y rev %5s/yr | NP Q %6s",
		q, num0(fd.pe, ""), num0(fd.roe, "%"), num0(fd.roce, "%"), pct(fd.revGrowth), pct(fd.npGrowth), pct(fd.revCAGR3), pct(fd.qNPChange))
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func num2(v float64) string {
	if math.IsNaN(v) {
		return ""
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

var leaderHeader = []string{"close", "from_ath_pct", "from_1y_high_pct", "ath_age_days", "new_high",
	"window_pct", "excess_pct", "rs_1y_high", "r3m_pct", "r6m_pct", "leader", "first_date"}

func leaderFields(p *regime.Panel, r regime.LeaderRow) []string {
	return []string{num2(r.Close), num2(r.FromATH), num2(r.From1Y), strconv.Itoa(r.ATHAge), strconv.FormatBool(r.NewHigh),
		num2(r.Window), num2(r.Excess), strconv.FormatBool(r.RSHigh), num2(r.R3M), num2(r.R6M),
		strconv.FormatBool(r.Leader), p.Dates[r.First].String()}
}

func writeCSVFile(path string, rows [][]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	if err := w.WriteAll(rows); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func writeSectorCSV(path string, p *regime.Panel, rep regime.LeaderReport) error {
	rows := [][]string{append([]string{"name", "group"}, leaderHeader...)}
	for _, r := range append(append([]regime.LeaderRow(nil), rep.Bench...), rep.Sectors...) {
		g := r.Group
		if g == "" {
			g = "benchmark"
		}
		rows = append(rows, append([]string{r.Name, g}, leaderFields(p, r)...))
	}
	return writeCSVFile(path, rows)
}

func writeStockCSV(path string, p *regime.Panel, stocks []stockLeader) error {
	rows := [][]string{append(append([]string{"symbol", "industry", "sector_index"}, leaderHeader...),
		"isin", "pe", "pb", "roe_pct", "roce_pct", "rev_growth_fy_pct", "np_growth_fy_pct", "rev_cagr_3y_pct", "last_quarter", "np_qoq_pct", "quality")}
	sorted := append([]stockLeader(nil), stocks...)
	sort.SliceStable(sorted, func(a, b int) bool {
		if sorted[a].industry != sorted[b].industry {
			return sorted[a].industry < sorted[b].industry
		}
		if sorted[a].Leader != sorted[b].Leader {
			return sorted[a].Leader
		}
		x, y := sorted[a].Excess, sorted[b].Excess
		return !math.IsNaN(x) && (math.IsNaN(y) || x > y)
	})
	for _, s := range sorted {
		row := append([]string{s.Name, s.industry, industryIndex[s.industry]}, leaderFields(p, s.LeaderRow)...)
		fd := s.fund
		if fd.ok {
			row = append(row, s.isin, num2(fd.pe), num2(fd.pb), num2(fd.roe), num2(fd.roce), num2(fd.revGrowth), num2(fd.npGrowth),
				num2(fd.revCAGR3), fd.lastQ, num2(fd.qNPChange), strconv.FormatBool(fd.quality))
		} else {
			row = append(row, s.isin, "", "", "", "", "", "", "", "", "", "")
		}
		rows = append(rows, row)
	}
	return writeCSVFile(path, rows)
}
