package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"serene/internal/kite"
	"serene/internal/market"
)

// Weekly lists from StockEdge's FIPA and CEPA Comb scans, as used in Vivek
// Bajaj's "weekend trader" video: scan on the weekend, then set alerts on the
// matches for the week. The price rules are the scans' own. Their fundamental
// rules need history Upstox doesn't serve (only 4 quarters, today's ratios, no
// fixed-asset line), so annual stand-ins are used:
//
//   - YoY quarterly profit, EBITDA, sales growth → the latest year's net
//     profit, operating profit, revenue above the year before's;
//   - FIPA's improved ROE and ROCE → above the sector's, as Upstox gives it;
//   - CEPA's high ROE and ROCE → at least a threshold;
//   - CEPA's gross fixed assets up over 3 years → non-current assets.
//
// ROCE and non-current assets are skipped where Upstox has none (banks).
//
// Backtested 2026-11-18 (since deleted) with FIPA's price rules as a weekly
// watchlist for daily alerts: no edge over 2012–2026 once the held-back years
// were checked. These are screeners, not proven strategies.

const (
	combWeek    = 5   // trading days in 1 week
	combQuarter = 63  // trading days in 3 months
	combYear    = 252 // trading days in the 52-week high
	combBench   = "NIFTY 50"
	combMarket  = "NIFTY 500" // stands in for industries Kite has no index for
)

// combSector maps each Industry in configs/nifty500.csv to the closest
// sectoral index Kite serves. Kite has no Capital Goods, Cement, Telecom,
// Textiles or Diversified index; the nearest broader one is used.
var combSector = map[string]string{
	"Automobile and Auto Components":    "NIFTY AUTO",
	"Capital Goods":                     "NIFTY INDIA MFG",
	"Chemicals":                         "NIFTY CHEMICALS",
	"Construction":                      "NIFTY INFRA",
	"Construction Materials":            "NIFTY INFRA",
	"Consumer Durables":                 "NIFTY CONSR DURBL",
	"Consumer Services":                 "NIFTY CONSUMPTION",
	"Fast Moving Consumer Goods":        "NIFTY FMCG",
	"Financial Services":                "NIFTY FIN SERVICE",
	"Healthcare":                        "NIFTY HEALTHCARE",
	"Information Technology":            "NIFTY IT",
	"Media Entertainment & Publication": "NIFTY MEDIA",
	"Metals & Mining":                   "NIFTY METAL",
	"Oil Gas & Consumable Fuels":        "NIFTY OIL AND GAS",
	"Power":                             "NIFTY ENERGY",
	"Realty":                            "NIFTY REALTY",
	"Services":                          "NIFTY SERV SECTOR",
	"Telecommunication":                 "NIFTY MS IT TELCM",
}

func sectorIndex(industry string) string {
	if s, ok := combSector[industry]; ok {
		return s
	}
	return combMarket
}

type combRow struct {
	sym, industry, isin, sector string
	close, high52               float64
	fromHigh                    float64 // percent below the 52-week high, ≤ 0
	ex1w, ex3m                  float64 // return minus NIFTY 50's over the same days, percent
	sx1w, sx3m                  float64 // return minus the sector index's
	fund                        fundamentals
}

// combRules are the thresholds, from flags.
type combRules struct {
	zonePct                    float64 // FIPA: at most this far below the 52-week high
	roeMin, roceMin, assetsMin float64 // CEPA
}

// excess is the stock's return minus the index's over the last back candles,
// in percent.
func excess(c []kite.Candle, idx map[market.Date]float64, back int) (float64, bool) {
	n := len(c)
	a, b := c[n-1-back], c[n-1]
	ia, okA := idx[market.DateOf(a.Time)]
	ib, okB := idx[market.DateOf(b.Time)]
	if !okA || !okB || a.Close <= 0 || ia <= 0 {
		return 0, false
	}
	return (b.Close/a.Close - ib/ia) * 100, true
}

// combScore measures a stock at its last candle against NIFTY 50 and its
// sector index. ok is false when it lacks a year of candles, its last one
// isn't on the benchmark's last date, or an index lacks one of the days.
func combScore(c []kite.Candle, bench, sector map[market.Date]float64, last market.Date) (r combRow, ok bool) {
	n := len(c)
	if n < combYear || market.DateOf(c[n-1].Time) != last {
		return r, false
	}
	for _, x := range []struct {
		v   *float64
		idx map[market.Date]float64
		k   int
	}{{&r.ex1w, bench, combWeek}, {&r.ex3m, bench, combQuarter}, {&r.sx1w, sector, combWeek}, {&r.sx3m, sector, combQuarter}} {
		if *x.v, ok = excess(c, x.idx, x.k); !ok {
			return r, false
		}
	}
	for _, k := range c[n-combYear:] {
		r.high52 = math.Max(r.high52, k.High)
	}
	r.close = c[n-1].Close
	r.fromHigh = (r.close/r.high52 - 1) * 100
	return r, true
}

// A combScan is one list: its price rules, then why a stock fails its
// fundamental rules ("" when it passes).
type combScan struct {
	name, rules string
	price       func(r combRow) bool
	fundFail    func(f fundamentals) string
}

func combScans(rl combRules) []combScan {
	growth := func(f fundamentals, why []string) []string {
		if !f.opUp {
			why = append(why, "op profit")
		}
		if !f.npUp {
			why = append(why, "net profit")
		}
		return why
	}
	return []combScan{
		{
			name: "FIPA",
			rules: fmt.Sprintf("Price: within %.0f%% of the 52-week high, ahead of %s over 1 week and 3 months. "+
				"Fundamentals: latest year's revenue, operating profit and net profit above the year before; ROE and ROCE above the sector's.", rl.zonePct, combBench),
			price: func(r combRow) bool { return r.fromHigh >= -rl.zonePct && r.ex1w > 0 && r.ex3m > 0 },
			fundFail: func(f fundamentals) string {
				var why []string
				if !f.revUp {
					why = append(why, "revenue")
				}
				why = growth(f, why)
				if !(f.roe > f.roeSector) {
					why = append(why, "ROE")
				}
				if !math.IsNaN(f.roce) && !(f.roce > f.roceSector) {
					why = append(why, "ROCE")
				}
				return strings.Join(why, ", ")
			},
		},
		{
			name: "CEPA",
			rules: fmt.Sprintf("Price: ahead of %s and of the stock's sector index over 1 week and 3 months. "+
				"Fundamentals: ROE ≥ %.0f%%, ROCE ≥ %.0f%%; latest year's operating profit and net profit above the year before; non-current assets (Upstox has no fixed-asset line) up ≥ %.0f%% over 3 years.",
				combBench, rl.roeMin, rl.roceMin, rl.assetsMin),
			price: func(r combRow) bool { return r.ex1w > 0 && r.ex3m > 0 && r.sx1w > 0 && r.sx3m > 0 },
			fundFail: func(f fundamentals) string {
				var why []string
				if !(f.roe >= rl.roeMin) {
					why = append(why, "ROE")
				}
				if !math.IsNaN(f.roce) && !(f.roce >= rl.roceMin) {
					why = append(why, "ROCE")
				}
				why = growth(f, why)
				switch {
				case math.IsNaN(f.assets3y) && math.IsNaN(f.roce):
					// a bank: Upstox has neither figure
				case !(f.assets3y >= rl.assetsMin): // NaN fails too: too little history
					why = append(why, "fixed assets")
				}
				return strings.Join(why, ", ")
			},
		},
	}
}

func runMarketComb(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("market comb", flag.ContinueOnError)
	list := fs.String("list", "configs/nifty500.csv", "stocks to scan, with Symbol, Industry and ISIN Code columns")
	only := fs.String("scans", "fipa,cepa", "which lists: fipa, cepa or both")
	var rl combRules
	fs.Float64Var(&rl.zonePct, "zone-pct", 10, "FIPA: the 52-week high zone, at most this percent below the high")
	fs.Float64Var(&rl.roeMin, "roe", 15, "CEPA: the lowest ROE, percent")
	fs.Float64Var(&rl.roceMin, "roce", 15, "CEPA: the lowest ROCE, percent")
	fs.Float64Var(&rl.assetsMin, "assets-pct", 30, "CEPA: the least rise in non-current assets over 3 years, percent")
	equitiesDir := fs.String("equities", "data/equities", "where Upstox fundamentals are cached")
	reports := fs.String("reports", "reports", "report directory")
	useSlack := fs.Bool("slack", false, "post each list to SLACK_SCANS_CHANNEL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var scans []combScan
	for _, s := range combScans(rl) {
		for _, w := range strings.Split(*only, ",") {
			if strings.EqualFold(strings.TrimSpace(w), s.name) {
				scans = append(scans, s)
			}
		}
	}
	if len(scans) == 0 {
		return fmt.Errorf("--scans %q names no list; use fipa, cepa or both", *only)
	}
	if !*useSlack {
		return combRun(ctx, *list, scans, *equitiesDir, *reports, nil, "")
	}
	client, channel, err := openSlackChannel(ctx, "SLACK_SCANS_CHANNEL")
	if err != nil {
		return err
	}
	if err := combRun(ctx, *list, scans, *equitiesDir, *reports, client, channel); err != nil {
		postAlert(ctx, client, channel, fmt.Sprintf(":x: *serene market comb failed:* %v\nThis week's FIPA/CEPA lists were not posted.", err))
		return err
	}
	return nil
}

func combRun(ctx context.Context, list string, scans []combScan, equitiesDir, reports string, client interface {
	Post(context.Context, string, string) error
}, channel string) error {
	cols, err := readListColumns(list, "Symbol", "Industry", "ISIN Code")
	if err != nil {
		return err
	}
	kc, err := kiteClient(ctx)
	if err != nil {
		return err
	}
	ins, err := kc.Instruments(ctx, "NSE")
	if err != nil {
		return err
	}
	stocks, indices := map[string]int64{}, map[string]int64{}
	for _, in := range ins {
		switch {
		case in.Segment == "INDICES":
			indices[in.Symbol] = in.Token
		case in.Segment == "NSE" && in.Type == "EQ":
			stocks[in.Symbol] = in.Token
		}
	}
	to := time.Now()
	from := to.AddDate(0, 0, -400)
	closes := map[string]map[market.Date]float64{}
	var last market.Date
	need := []string{combBench, combMarket}
	for _, s := range combSector {
		need = append(need, s)
	}
	for _, name := range need {
		if closes[name] != nil {
			continue
		}
		tok, ok := indices[name]
		if !ok {
			return fmt.Errorf("no %s in Kite's NSE indices", name)
		}
		c, err := kc.Candles(ctx, tok, "day", from, to, false)
		if err != nil {
			return fmt.Errorf("%s candles: %w", name, err)
		}
		if len(c) <= combQuarter { // the sector rules look back 3 months
			return fmt.Errorf("only %d %s candles", len(c), name)
		}
		m := map[market.Date]float64{}
		for _, k := range c {
			m[market.DateOf(k.Time)] = k.Close
		}
		closes[name] = m
		if name == combBench {
			last = market.DateOf(c[len(c)-1].Time)
		}
	}
	fmt.Printf("scanning %d stocks on Kite daily candles to %s\n", len(cols[0]), last)

	var rows []combRow
	var missing, short []string
	for k, sym := range cols[0] {
		tok, ok := stocks[sym]
		if !ok {
			missing = append(missing, sym)
			continue
		}
		c, err := kc.Candles(ctx, tok, "day", from, to, false)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			return fmt.Errorf("%s candles: %w", sym, err)
		}
		sector := sectorIndex(cols[1][k])
		r, ok := combScore(c, closes[combBench], closes[sector], last)
		if !ok {
			short = append(short, sym)
			continue
		}
		r.sym, r.industry, r.isin, r.sector = sym, cols[1][k], cols[2][k], sector
		rows = append(rows, r)
	}
	var common []string
	if len(missing) > 0 {
		common = append(common, fmt.Sprintf("not in Kite's NSE list (renamed or delisted): %s", strings.Join(missing, ", ")))
	}
	if len(short) > 0 {
		common = append(common, fmt.Sprintf("%d stocks skipped: under a year of candles or no candle on %s", len(short), last))
	}

	// Fundamentals for every stock that passes some list's price rules.
	seen := map[string]bool{}
	var isins []string
	for _, r := range rows {
		for _, s := range scans {
			if s.price(r) && r.isin != "" && !seen[r.isin] {
				seen[r.isin] = true
				isins = append(isins, r.isin)
			}
		}
	}
	got, more, err := loadFundamentals(ctx, filepath.Join(equitiesDir, "fundamentals"), isins)
	if err != nil {
		return err
	}
	for _, n := range more {
		// Without a token the fundamentals can't be checked: fail, so the alert says why.
		if strings.HasPrefix(n, "fundamentals ") {
			return fmt.Errorf("%s (log in to Upstox with serene auth)", n)
		}
	}
	for k := range rows {
		rows[k].fund = got[rows[k].isin]
	}

	var texts []string
	for _, s := range scans {
		var price, hits []combRow
		var noData []string
		failed := map[string]int{}
		for _, r := range rows {
			if !s.price(r) {
				continue
			}
			price = append(price, r)
			if !r.fund.ok {
				noData = append(noData, r.sym)
				continue
			}
			if why := s.fundFail(r.fund); why != "" {
				for _, w := range strings.Split(why, ", ") {
					failed[w]++
				}
				continue
			}
			hits = append(hits, r)
		}
		sort.Slice(hits, func(a, b int) bool { return hits[a].ex3m > hits[b].ex3m })
		notes := append([]string(nil), common...)
		if len(noData) > 0 {
			notes = append(notes, fmt.Sprintf("passed the price rules but have no Upstox fundamentals, so left out: %s", strings.Join(noData, ", ")))
		}
		var fails []string
		for _, w := range []string{"revenue", "op profit", "net profit", "ROE", "ROCE", "fixed assets"} {
			if failed[w] > 0 {
				fails = append(fails, fmt.Sprintf("%s %d", w, failed[w]))
			}
		}
		if len(fails) > 0 {
			notes = append(notes, fmt.Sprintf("%d passed the price rules; failing the fundamentals (a stock can fail several): %s", len(price), strings.Join(fails, ", ")))
		}
		text := combSummary(s, last, len(rows), hits, notes)
		fmt.Print(text, "\n")
		path := filepath.Join(reports, strings.ToLower(s.name), last.String()+".csv")
		if err := writeCombCSV(path, hits); err != nil {
			return err
		}
		fmt.Println("csv:", path)
		texts = append(texts, text)
	}
	if client != nil {
		for _, t := range texts {
			if err := client.Post(ctx, channel, t); err != nil {
				return err
			}
		}
		fmt.Println("posted to Slack")
	}
	return nil
}

// combSummary is one list's Slack text: a header, then the matches in a code block.
func combSummary(s combScan, last market.Date, scanned int, hits []combRow, notes []string) string {
	var sb strings.Builder
	f := func(format string, a ...any) { fmt.Fprintf(&sb, format, a...) }
	f("*%s weekly list, close of %s*: %d of %d Nifty 500 stocks\n", s.name, last.At(12, 0).Format("Mon 2 Jan 2006"), len(hits), scanned)
	f("%s Upstox serves only annual figures and today's ratios, so these stand in for the scan's quarterly YoY rules. "+
		"Q = ROE and ROCE ≥ %.0f%%; rev/NP are year-on-year, NP Q the latest quarter over the one before.\n", s.rules, qualityMin)
	if len(hits) > 0 {
		sb.WriteString("```\n")
		f("%-11s %-20s %9s %7s %6s %6s %6s %6s  %s\n", "symbol", "industry", "close", "52wH", "1W ex", "3M ex", "1W sec", "3M sec", "fundamentals")
		for _, r := range hits {
			f("%-11s %-20s %9.2f %6.1f%% %5.1f%% %5.1f%% %5.1f%% %5.1f%%  %s | assets 3y %s\n", trunc(r.sym, 11), trunc(r.industry, 20), r.close,
				r.fromHigh, r.ex1w, r.ex3m, r.sx1w, r.sx3m, fundLine(r.fund), pct(r.fund.assets3y))
		}
		sb.WriteString("```\n")
	}
	for _, n := range notes {
		f("_%s_\n", n)
	}
	return sb.String()
}

func writeCombCSV(path string, hits []combRow) error {
	rows := [][]string{{"symbol", "industry", "isin", "sector_index", "close", "high_52w", "from_high_pct", "ex_1w_pct", "ex_3m_pct",
		"sector_ex_1w_pct", "sector_ex_3m_pct", "pe", "roe_pct", "roe_sector_pct", "roce_pct", "roce_sector_pct",
		"rev_growth_pct", "np_growth_pct", "np_q_change_pct", "assets_3y_pct", "quality"}}
	for _, r := range hits {
		q := ""
		if r.fund.quality {
			q = "Q"
		}
		fd := r.fund
		rows = append(rows, []string{r.sym, r.industry, r.isin, r.sector, num2(r.close), num2(r.high52), num2(r.fromHigh), num2(r.ex1w), num2(r.ex3m),
			num2(r.sx1w), num2(r.sx3m), num2(fd.pe), num2(fd.roe), num2(fd.roeSector), num2(fd.roce), num2(fd.roceSector),
			num2(fd.revGrowth), num2(fd.npGrowth), num2(fd.qNPChange), num2(fd.assets3y), q})
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeCSVFile(path, rows)
}
