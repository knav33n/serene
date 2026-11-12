// Command serene researches and trades Indian index options.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"

	"serene/internal/analytics"
	"serene/internal/auth"
	"serene/internal/bhav"
	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/ingest"
	"serene/internal/margin"
	"serene/internal/market"
	"serene/internal/nse"
	"serene/internal/regime"
	"serene/internal/risk"
	"serene/internal/sim"
	"serene/internal/slack"
	"serene/internal/strategies"
	"serene/internal/strategy"
	"serene/internal/synth"
	"serene/internal/upstox"
	"serene/web"
)

const usage = `serene researches and trades Indian index options.

  serene auth                  open the broker login page
  serene auth status           show each broker's login, never the token
  serene auth logout BROKER    revoke a token at the broker and delete it

  serene research synth       --from DATE --to DATE [--underlying NIFTY] [--seed N] [--data DIR]
  serene research backtest    --config FILE [--unseal] [--holdout FILE]
  serene research sweep       --config FILE [--unseal] [--holdout FILE]
  serene research walkforward --config FILE [--unseal] [--holdout FILE]

  serene research fetch       [--underlying NIFTY] [--from DATE] [--to DATE] [--band-pct 5] [--data DIR]
  serene research coverage    [--underlying NIFTY] [--data DIR]
  serene research swings      --date DATE [--underlying NIFTY] [--strike 0] [--expiry DATE] [--k 2] [--min-pullback 10] [--ema 21] [--vwap=true] [--exit-k 1] [--verbose]
  serene research stops       [--from 2024-12-27] [--to 2026-04-30] [--flat 15:15] [--data DIR] (same entry flags as swings)
  serene research grid        [--from DATE] [--to DATE] [--split 2025-09-01] [--out reports/grid.csv]
  serene research emacross    [--from DATE] [--to DATE] [--split 2025-09-01] [--slip-bps 30] [--step 50] [--out reports/emacross.csv] [--trades FILE]
  serene research eventvol    [--from DATE] [--to DATE] [--split 2025-09-01] [--slip-bps 30] [--step 50] [--out reports/eventvol.csv] [--trades FILE]
  serene research mcxcloud    [--names A,B] [--n 5] [--exit swing30|vote30|cloud30|ha30|2R] [--nine-k 1] [--table NAME] [--grid] [--trades FILE]
  serene research mcxema      [--name GOLDM] [--from DATE] [--to DATE] [--split DATE] [--slip-ticks 1]
  serene research mcxemagap   [--name GOLDM] [--from DATE] [--to DATE] [--split DATE]   (EMA crosses with a minimum gap)
  serene research mcxemaexit  [--frame 15] [--fast 13] [--slow 34] [--from DATE] [--to DATE]   (EMA cross exit rules)
  serene research mcxhma      [--name GOLDM] [--from DATE] [--to DATE] [--split DATE] [--slip-ticks 1]   (Hull MA crosses and turns)
  serene research mcxmetals   [--names COPPER,ZINCMINI,...] [--from DATE] [--to DATE] [--split DATE] [--slip-ticks 1]   (EMA + Hull crosses on base metals)
  serene research mcxhmafwd   [--to DATE] [--judge] [--replay]   (frozen GOLDM 1h HMA-21 turn, sessions from 2026-11-11)
  serene research mcxemafwd   [--to DATE] [--judge] [--replay]   (frozen GOLDM 15m 13/34, sessions from 2026-11-11)
  serene research silvermfwd  [--strategy silverm_ema_15m|silverm_hma_1h] [--to DATE] [--judge] [--replay]   (frozen GOLDM rules on SILVERM, sessions from 2026-11-12)
  serene research mcxdow      [--names A,B] [--from DATE] [--to DATE] [--frame 1h] [--lookback 720h] [--multiple 2.5] [--slip-ticks 1] [--trades FILE]
  serene research pdbreak     [--from DATE] [--to DATE] [--split 2025-09-01] [--offset-pct 1] [--slip-bps 30] [--trades FILE]
  serene research dayfeatures [--from DATE] [--to DATE] [--data DIR] [--market DIR] [--out reports/dayfeatures.csv]
  serene research stockdiv    [--from 2005-01-01] [--to 2022-12-31] [--top 200] [--cost-pct 0.24] [--slip-bps 10] [--data DIR]

  serene market fetch         [--from DATE] [--to DATE] [--rebuild] [--data DIR] [--catalog FILE]
  serene market report        [--date DATE] [--data DIR] [--catalog FILE] [--reports DIR]
  serene market daily         [--slack] [--data DIR] [--catalog FILE] [--reports DIR]
  serene market check         [--data DIR] [--catalog FILE]

  serene equities fetch       [--from DATE] [--to DATE] [--rebuild] [--data DIR]
  serene equities check       [--symbols A,B] [--from DATE] [--to DATE] [--data DIR]
  serene equities adjust      [--symbols A,B] [--recheck] [--data DIR]
  serene equities daily       [--data DIR]

  serene mcx fetch            [--names A,B] [--daily=true] [--minute=true] [--lookback-days 500] [--data DIR]

  serene paper emaretest      [--slack] [--date DATE] [--step 100]   (paper trade nifty_ema_retest; entries/exits with P&L)

Not built yet:
  serene forward signals      [--fetch] [--slack] [--strategies goldm_ema_15m,goldm_hma_1h,silverm_ema_15m,silverm_hma_1h,ema_cross_first]   (new forward-test entries/exits, no P&L)
  serene prod paper, serene prod live
`

// config is the YAML run file.
type config struct {
	Strategy   string          `yaml:"strategy"`
	Underlying string          `yaml:"underlying"`
	From       string          `yaml:"from"`
	To         string          `yaml:"to"`
	Capital    float64         `yaml:"capital"`
	DataDir    string          `yaml:"data_dir"`
	Settlement string          `yaml:"settlement"` // NSE data directory, or last_bar
	CostsFile  string          `yaml:"costs_file"`
	ReportDir  string          `yaml:"report_dir"`
	Params     strategy.Params `yaml:"params"`
	Sim        struct {
		SlippageBps float64 `yaml:"slippage_bps"`
		MinTicks    int64   `yaml:"min_ticks"`
		MaxWaitMins int     `yaml:"max_wait_minutes"`
	} `yaml:"sim"`
	Margin struct {
		NakedPct float64 `yaml:"naked_pct"`
	} `yaml:"margin"`
	// Risk is optional: without it, runs check margin only.
	Risk *struct {
		MaxDailyLoss       float64 `yaml:"max_daily_loss"` // rupees
		MaxLotsPerLeg      int64   `yaml:"max_lots_per_leg"`
		MaxOpenLots        int64   `yaml:"max_open_lots"`
		MaxOrdersPerMinute int     `yaml:"max_orders_per_minute"`
		MaxQuoteAgeSeconds int     `yaml:"max_quote_age_seconds"`
		MaxLimitAwayPct    float64 `yaml:"max_limit_away_pct"`
	} `yaml:"risk"`
	Sweep struct {
		Metric string               `yaml:"metric"`
		Grid   map[string][]float64 `yaml:"grid"`
	} `yaml:"sweep"`
	WalkForward struct {
		TrainDays int `yaml:"train_days"`
		TestDays  int `yaml:"test_days"`
	} `yaml:"walkforward"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "serene:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "auth" {
		return runAuth(ctx, args[1:])
	}
	if len(args) < 2 {
		fmt.Print(usage)
		return errors.New("expected a mode and a command")
	}
	cmd, rest := args[0]+" "+args[1], args[2:]
	switch cmd {
	case "research synth":
		return runSynth(ctx, rest)
	case "research backtest", "research sweep", "research walkforward":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		path := fs.String("config", "", "run config file")
		holdout := fs.String("holdout", "configs/holdout.yaml", "data held back from research")
		unseal := fs.Bool("unseal", false, "allow a run into held-back data; it is recorded")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *path == "" {
			return errors.New("--config is required")
		}
		return runResearch(ctx, args[1], *path, *holdout, *unseal)
	case "research fetch":
		return runFetch(ctx, rest)
	case "research coverage":
		return runCoverage(ctx, rest)
	case "research swings":
		return runSwings(ctx, rest)
	case "research stops":
		return runStops(ctx, rest)
	case "research grid":
		return runGrid(ctx, rest)
	case "research stockdiv":
		return runStockDiv(ctx, rest)
	case "research emacross":
		return runEMACross(ctx, rest)
	case "research pdbreak":
		return runPDBreak(ctx, rest)
	case "research mcxcloud":
		return runMCXCloud(ctx, rest)
	case "research mcxema":
		return runMCXEMA(ctx, rest)
	case "research mcxhma":
		return runMCXHMA(ctx, rest)
	case "research mcxmetals":
		return runMCXMetals(ctx, rest)
	case "research silvermfwd":
		return runSilvermForward(ctx, rest)
	case "research mcxhmafwd":
		return runMCXHMAForward(ctx, rest)
	case "research mcxemafwd":
		return runMCXEMAForward(ctx, rest)
	case "research mcxemagap":
		return runMCXEMAGap(ctx, rest)
	case "research mcxemaexit":
		return runMCXEMAExit(ctx, rest)
	case "research mcxdow":
		return runMCXDow(ctx, rest)
	case "research eventvol":
		return runEventVol(ctx, rest)
	case "research dayfeatures":
		return runDayFeatures(ctx, rest)
	case "market fetch":
		return runMarketFetch(ctx, rest)
	case "market report":
		return runMarketReport(ctx, rest)
	case "market daily":
		return runMarketDaily(ctx, rest)
	case "market check":
		return runMarketCheck(ctx, rest)
	case "equities fetch":
		return runEquitiesFetch(ctx, rest)
	case "equities check":
		return runEquitiesCheck(ctx, rest)
	case "equities adjust":
		return runEquitiesAdjust(ctx, rest)
	case "equities daily":
		return runEquitiesDaily(ctx, rest)
	case "mcx fetch":
		return runMCXFetch(ctx, rest)
	case "forward signals":
		return runForwardSignals(ctx, rest)
	case "paper emaretest":
		return runPaper(ctx, rest)
	case "prod paper", "prod live":
		return fmt.Errorf("%q is not built yet", cmd)
	}
	fmt.Print(usage)
	return fmt.Errorf("unknown command %q", cmd)
}

func openTokens(ctx context.Context) (*auth.PGStore, error) {
	if err := auth.LoadDotEnv(".env"); err != nil {
		return nil, err
	}
	sealer, err := auth.NewSealer(os.Getenv("SERENE_TOKEN_KEY"))
	if err != nil {
		return nil, err
	}
	return auth.OpenPostgres(ctx, os.Getenv("SERENE_DATABASE_URL"), sealer, "serene")
}

func runFetch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	underlying := fs.String("underlying", "NIFTY", "underlying name")
	from := fs.String("from", "2000-01-01", "first expiry")
	to := fs.String("to", "2100-01-01", "last expiry")
	band := fs.Float64("band-pct", 5, "keep strikes this far outside the index's range in the month before expiry")
	dir := fs.String("data", "data/upstox", "data directory")
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
	tokens, err := openTokens(ctx)
	if err != nil {
		return err
	}
	defer tokens.Close()
	token, err := auth.Load(ctx, tokens, "upstox")
	if err != nil {
		return err
	}
	store, err := data.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()
	cfg := ingest.Config{Underlying: *underlying, Dir: *dir, BandPct: *band, From: f, To: t,
		Log: func(format string, a ...any) {
			fmt.Printf("%s "+format+"\n", append([]any{time.Now().In(market.IST).Format("15:04:05")}, a...)...)
		}}
	return ingest.Run(ctx, cfg, store, upstox.New(token.AccessToken))
}

func runCoverage(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("coverage", flag.ContinueOnError)
	underlying := fs.String("underlying", "NIFTY", "underlying name")
	dir := fs.String("data", "data/upstox", "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := data.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()
	cov, err := ingest.Report(ctx, *dir, *underlying, store)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "expiry\ttype\tlot\tlisted\tselected\tempty\tthin\tbars\tdropped")
	var selected, empty, thin, bars, dropped int
	for _, e := range cov.Expiries {
		kind := "monthly"
		if e.Weekly {
			kind = "weekly"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n", e.Expiry, kind, e.LotSize, e.Listed, e.Selected, e.Empty, e.Thin, e.Bars, e.Dropped)
		selected += e.Selected
		empty += e.Empty
		thin += e.Thin
		bars += e.Bars
		dropped += e.Dropped
	}
	w.Flush()
	fmt.Printf("\n%d expiries, %d contracts, %d bars. %d contracts empty, %d thin (under 100 bars), %d candles dropped outside session.\n",
		len(cov.Expiries), selected, bars, empty, thin, dropped)
	if len(cov.Days) > 0 {
		fmt.Printf("%d trading days stored, %s to %s.\n", len(cov.Days), cov.Days[0].Date, cov.Days[len(cov.Days)-1].Date)
	}
	fmt.Printf("Missing expiries: %s\n", listOrNone(cov.Gaps))
	fmt.Printf("Expiries with no bars: %s\n", listOrNone(cov.Empty))
	fmt.Printf("Days with options but no index: %s\n", listOrNone(cov.NoIndex))
	fmt.Printf("Days with a short index session: %s\n", listOrNone(cov.Short))
	return nil
}

// marketFirstDay is before the earliest published index file, so a backfill
// finds the true start for itself.
const marketFirstDay = "2012-01-01"

func marketLog(format string, a ...any) {
	fmt.Printf("%s "+format+"\n", append([]any{time.Now().In(market.IST).Format("15:04:05")}, a...)...)
}

func runMarketFetch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("market fetch", flag.ContinueOnError)
	from := fs.String("from", marketFirstDay, "first date")
	to := fs.String("to", "", "last date, default today")
	rebuild := fs.Bool("rebuild", false, "skip the download and rebuild the store from the raw files")
	dir := fs.String("data", "data/market", "data directory")
	catalog := fs.String("catalog", "configs/indices.yaml", "index catalog")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cat, err := nse.LoadCatalog(*catalog)
	if err != nil {
		return err
	}
	store, err := data.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()
	var years []int
	if *rebuild {
		dates, err := nse.RawDates(*dir)
		if err != nil {
			return err
		}
		years = nse.Years(dates)
	} else {
		f, err := market.ParseDate(*from)
		if err != nil {
			return err
		}
		t := market.DateOf(time.Now())
		if *to != "" {
			if t, err = market.ParseDate(*to); err != nil {
				return err
			}
		}
		res, err := nse.Download(ctx, nse.Config{Dir: *dir, From: f, To: t, Log: marketLog}, nse.New())
		if err != nil {
			return err
		}
		years = nse.Years(append(res.New, res.Changed...))
	}
	unknown, err := nse.Rebuild(ctx, *dir, cat, store, years)
	if err != nil {
		return err
	}
	fmt.Printf("stored %d years. Names not in the catalog: %s\n", len(years), listOrNone(unknown))
	return nil
}

// runEquitiesFetch downloads NSE's daily cash market bhavcopy and rebuilds
// the years it touched. The default range is NSE's whole history.
func runEquitiesFetch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("equities fetch", flag.ContinueOnError)
	from := fs.String("from", bhav.FirstDay.String(), "first date")
	to := fs.String("to", "", "last date, default today")
	rebuild := fs.Bool("rebuild", false, "skip the download and rebuild the store from the raw files")
	dir := fs.String("data", "data/equities", "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := data.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()
	var years []int
	if *rebuild {
		dates, err := bhav.RawDates(*dir)
		if err != nil {
			return err
		}
		years = bhav.Years(dates)
	} else {
		f, err := market.ParseDate(*from)
		if err != nil {
			return err
		}
		t := market.DateOf(time.Now())
		if *to != "" {
			if t, err = market.ParseDate(*to); err != nil {
				return err
			}
		}
		if years, err = equitiesDownload(ctx, *dir, f, t); err != nil {
			return err
		}
	}
	if err := bhav.Rebuild(ctx, *dir, store, years); err != nil {
		return err
	}
	fmt.Printf("stored %d years in %s\n", len(years), filepath.Join(*dir, "stocks"))
	return nil
}

// runEquitiesCheck compares stored bhavcopy rows with Upstox daily candles for
// some EQ-series symbols. Bhavcopy prices are as traded, so a split or bonus
// shows as a run of mismatches before it if Upstox adjusts for it.
func runEquitiesCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("equities check", flag.ContinueOnError)
	symbols := fs.String("symbols", "RELIANCE,HDFCBANK,INFY,TCS,SBIN,ITC,TATASTEEL,IRCTC,SUZLON,YESBANK,ZEEL,20MICRONS",
		"comma-separated EQ-series symbols")
	from := fs.String("from", market.DateOf(time.Now()).AddDays(-365).String(), "first date")
	to := fs.String("to", market.DateOf(time.Now()).String(), "last date")
	dir := fs.String("data", "data/equities", "data directory")
	show := fs.Int("show", 5, "mismatches to print per symbol")
	adjusted := fs.Bool("adjusted", false, "apply the stored adjustment factors first, and allow 0.5% for rounding")
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
	tokens, err := openTokens(ctx)
	if err != nil {
		return err
	}
	defer tokens.Close()
	token, err := auth.Load(ctx, tokens, "upstox")
	if err != nil {
		return err
	}
	up := upstox.New(token.AccessToken)
	store, err := data.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "symbol\tdays\tmatch\tohlc off\tvolume off\tonly bhav\tonly upstox")
	var notes []string
	for _, sym := range strings.Split(*symbols, ",") {
		rows, err := store.EquityDays(ctx, sym, "EQ", f, t)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			fmt.Fprintf(tw, "%s\tno bhavcopy rows\n", sym)
			continue
		}
		// Files before 2011 carry no ISIN, so take the newest one.
		all, err := store.EquityDays(ctx, sym, "EQ", bhav.FirstDay, market.DateOf(time.Now()))
		if err != nil {
			return err
		}
		isin := all[len(all)-1].ISIN
		var candles []upstox.Candle
		// A decade per request is Upstox's limit for daily candles.
		for lo := f; !t.Before(lo); lo = lo.AddDays(3650) {
			hi := lo.AddDays(3649)
			if t.Before(hi) {
				hi = t
			}
			c, err := up.DailyCandles(ctx, "NSE_EQ|"+isin, lo, hi)
			if err != nil {
				return fmt.Errorf("%s: %w", sym, err)
			}
			candles = append(candles, c...)
		}
		factors, err := store.EquityFactors(ctx, sym)
		if err != nil {
			return err
		}
		factorOn := func(d market.Date) float64 {
			for _, f := range factors {
				if !d.Before(f.From) && !f.To.Before(d) {
					return f.FactorAll
				}
			}
			return 1
		}
		byDate := map[market.Date]upstox.Candle{}
		for _, c := range candles {
			byDate[market.DateOf(c.Time)] = c
		}
		var match, ohlcOff, volOff, onlyBhav, shown int
		for _, r := range rows {
			c, ok := byDate[r.Date]
			if !ok {
				onlyBhav++
				continue
			}
			delete(byDate, r.Date)
			o, h, l, cl := market.Rupees(c.Open), market.Rupees(c.High), market.Rupees(c.Low), market.Rupees(c.Close)
			priceOK := o == r.Open && h == r.High && l == r.Low && cl == r.Close
			volOK := c.Volume == r.Volume
			if *adjusted {
				f := factorOn(r.Date)
				near := func(u, b market.Paise) bool {
					return math.Abs(float64(u)-float64(b)*f) <= 0.005*float64(b)*f+1
				}
				r.Open, r.High, r.Low, r.Close = market.Rupees(r.Open.Rupees()*f), market.Rupees(r.High.Rupees()*f),
					market.Rupees(r.Low.Rupees()*f), market.Rupees(r.Close.Rupees()*f)
				f = 1
				priceOK = near(o, r.Open) && near(h, r.High) && near(l, r.Low) && near(cl, r.Close)
				volOK = true
			}
			switch {
			case priceOK && volOK:
				match++
				continue
			case !priceOK:
				ohlcOff++
			default:
				volOff++
			}
			if shown < *show {
				shown++
				notes = append(notes, fmt.Sprintf("%s %s  bhav %s/%s/%s/%s v%d  upstox %s/%s/%s/%s v%d", sym, r.Date,
					r.Open, r.High, r.Low, r.Close, r.Volume, o, h, l, cl, c.Volume))
			}
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\n", sym, len(rows), match, ohlcOff, volOff, onlyBhav, len(byDate))
	}
	tw.Flush()
	for _, n := range notes {
		fmt.Println(n)
	}
	return nil
}

func marketMeta(cat *nse.Catalog) func(string) regime.Meta {
	return func(name string) regime.Meta {
		if e, ok := cat.Lookup(name); ok {
			return regime.Meta{Group: e.Group, Breadth: e.Breadth}
		}
		return regime.Meta{}
	}
}

func loadIndexRows(ctx context.Context, store *data.Store) ([]market.IndexDay, error) {
	return store.IndexDays(ctx, market.Date{Year: 2000, Month: 1, Day: 1}, market.DateOf(time.Now()))
}

func marketSnapshot(ctx context.Context, store *data.Store, cat *nse.Catalog, asOf, lastSent market.Date) (*regime.Snapshot, error) {
	rows, err := loadIndexRows(ctx, store)
	if err != nil {
		return nil, err
	}
	p := regime.NewPanel(rows, marketMeta(cat))
	return regime.Build(p, asOf, lastSent, nse.GroupSectoral, nse.GroupBroad)
}

func marketReportPath(dir string, d market.Date) string {
	return filepath.Join(dir, "market", d.String()+".html")
}

func runMarketReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("market report", flag.ContinueOnError)
	date := fs.String("date", "", "report date, default the latest close")
	dir := fs.String("data", "data/market", "data directory")
	catalog := fs.String("catalog", "configs/indices.yaml", "index catalog")
	reports := fs.String("reports", "reports", "report directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cat, err := nse.LoadCatalog(*catalog)
	if err != nil {
		return err
	}
	asOf := market.DateOf(time.Now())
	if *date != "" {
		if asOf, err = market.ParseDate(*date); err != nil {
			return err
		}
	}
	store, err := data.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()
	s, err := marketSnapshot(ctx, store, cat, asOf, asOf)
	if err != nil {
		return err
	}
	path := marketReportPath(*reports, s.Date)
	if err := regime.WriteReport(path, s); err != nil {
		return err
	}
	fmt.Println(strings.ReplaceAll(s.Summary(), "*", ""))
	fmt.Println("report:", path)
	card, err := renderCard(ctx, path, s)
	if err != nil {
		return err
	}
	fmt.Println("card:", card)
	return nil
}

// slackDM is a Slack client aimed at one conversation.
type slackDM struct {
	client  *slack.Client
	channel string
}

func openSlack(ctx context.Context) (*slackDM, error) {
	if err := auth.LoadDotEnv(".env"); err != nil {
		return nil, err
	}
	token, target := os.Getenv("SLACK_TOKEN"), os.Getenv("SLACK_CHANNEL")
	if token == "" || target == "" {
		return nil, errors.New("--slack needs SLACK_TOKEN and SLACK_CHANNEL in .env")
	}
	c := slack.New(token)
	channel, err := c.Resolve(ctx, target)
	if err != nil {
		return nil, err
	}
	return &slackDM{c, channel}, nil
}

func runMarketDaily(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("market daily", flag.ContinueOnError)
	useSlack := fs.Bool("slack", false, "send the report to Slack, once per close")
	dir := fs.String("data", "data/market", "data directory")
	catalog := fs.String("catalog", "configs/indices.yaml", "index catalog")
	reports := fs.String("reports", "reports", "report directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var dm *slackDM
	if *useSlack {
		var err error
		if dm, err = openSlack(ctx); err != nil {
			return err
		}
	}
	err := marketDaily(ctx, *dir, *catalog, *reports, dm)
	if err != nil && dm != nil {
		// A broken job must not look like a quiet day.
		text := fmt.Sprintf(":x: *serene market daily failed:* %v", err)
		if perr := dm.client.Post(context.WithoutCancel(ctx), dm.channel, text); perr != nil {
			fmt.Fprintln(os.Stderr, "serene: slack:", perr)
		}
	}
	return err
}

func marketDaily(ctx context.Context, dir, catalog, reports string, dm *slackDM) error {
	cat, err := nse.LoadCatalog(catalog)
	if err != nil {
		return err
	}
	store, err := data.Open(dir)
	if err != nil {
		return err
	}
	defer store.Close()
	first, err := market.ParseDate(marketFirstDay)
	if err != nil {
		return err
	}
	today := market.DateOf(time.Now())
	res, err := nse.Download(ctx, nse.Config{Dir: dir, From: first, To: today, Log: marketLog}, nse.New())
	if err != nil {
		return err
	}
	unknown, err := nse.Rebuild(ctx, dir, cat, store, nse.Years(append(res.New, res.Changed...)))
	if err != nil {
		return err
	}
	dates, err := nse.RawDates(dir)
	if err != nil {
		return err
	}
	if len(dates) == 0 {
		return errors.New("no index files stored: run serene market fetch")
	}
	latest := dates[len(dates)-1]
	sent, err := nse.LoadSent(dir)
	if err != nil {
		return err
	}
	if expected := nse.ExpectedDate(today); latest.Before(expected) && sent.Noted != expected.String() {
		text := fmt.Sprintf("No NSE index file for %s: a holiday, or the source is down. Latest close stored is %s.", expected, latest)
		fmt.Println(text)
		if dm != nil {
			if err := dm.client.Post(ctx, dm.channel, text); err != nil {
				return err
			}
			sent.Noted = expected.String()
			if err := nse.SaveSent(dir, sent); err != nil {
				return err
			}
		}
	}
	switch step := sent.Next(latest); {
	case step == nse.Done, step == nse.AttachOnly && dm == nil:
		fmt.Println("already reported", latest)
		return nil
	case step == nse.AttachOnly:
		return attachReport(ctx, dm, dir, &sent, marketReportPath(reports, latest), latest)
	}
	lastSent := latest
	if sent.Reported != "" {
		if lastSent, err = market.ParseDate(sent.Reported); err != nil {
			return err
		}
	}
	s, err := marketSnapshot(ctx, store, cat, today, lastSent)
	if err != nil {
		return err
	}
	if len(unknown) > 0 {
		s.Warnings = append(s.Warnings, "New index names, not in the catalog: "+strings.Join(unknown, ", "))
	}
	for _, d := range res.Changed {
		s.Warnings = append(s.Warnings, fmt.Sprintf("NSE rewrote the file for %s, so the store was updated.", d))
	}
	path := marketReportPath(reports, s.Date)
	if err := regime.WriteReport(path, s); err != nil {
		return err
	}
	fmt.Println(strings.ReplaceAll(s.Summary(), "*", ""))
	fmt.Println("report:", path)
	card, cardErr := renderCard(ctx, path, s)
	if cardErr != nil {
		fmt.Fprintln(os.Stderr, "serene:", cardErr)
	} else {
		fmt.Println("card:", card)
	}
	if dm == nil {
		return nil
	}
	if cardErr == nil {
		err := dm.client.Upload(ctx, dm.channel, s.Brief(), reportFiles(path, s))
		if err == nil {
			sent.Reported, sent.Attached = s.Date.String(), s.Date.String()
			return nse.SaveSent(dir, sent)
		}
		cardErr = err
	}
	// The market summary still goes out as text, and the alert says why.
	if err := dm.client.Post(ctx, dm.channel, s.Summary()); err != nil {
		return err
	}
	sent.Reported = s.Date.String()
	if err := nse.SaveSent(dir, sent); err != nil {
		return err
	}
	return fmt.Errorf("summary sent as text, because the card failed: %w", cardErr)
}

func cardPath(reportPath string) string { return strings.TrimSuffix(reportPath, ".html") + ".png" }

// renderCard makes the image for Slack beside the report, using a scratch page.
func renderCard(ctx context.Context, reportPath string, s *regime.Snapshot) (string, error) {
	page := strings.TrimSuffix(reportPath, ".html") + ".card.html"
	defer os.Remove(page)
	if err := regime.WriteCard(page, s); err != nil {
		return "", err
	}
	out := cardPath(reportPath)
	return out, regime.RenderPNG(ctx, page, out, regime.CardHeight(s))
}

// reportFiles lists what goes to Slack. The summary doubles as the image's alt
// text, so everything in the picture also exists as words.
func reportFiles(path string, s *regime.Snapshot) []slack.File {
	return []slack.File{
		{Path: cardPath(path), Title: "Market card " + s.Date.String(), Alt: strings.ReplaceAll(s.Summary(), "*", "")},
		{Path: path, Title: "Full report " + s.Date.String()},
	}
}

func attachReport(ctx context.Context, dm *slackDM, dir string, sent *nse.Sent, path string, d market.Date) error {
	files := []slack.File{{Path: path, Title: "Full report " + d.String()}}
	if _, err := os.Stat(cardPath(path)); err == nil {
		files = append([]slack.File{{Path: cardPath(path), Title: "Market card " + d.String(), Alt: "Market card for " + d.String()}}, files...)
	}
	if err := dm.client.Upload(ctx, dm.channel, "Report for "+d.String(), files); err != nil {
		return fmt.Errorf("summary sent, but the report file failed: %w", err)
	}
	sent.Attached = d.String()
	return nse.SaveSent(dir, *sent)
}

// marketWindows are the falls that the regime review measures.
var marketWindows = []struct{ name, from, to string }{
	{"2015-16 fall", "2015-01-01", "2016-06-30"},
	{"2018-19 small-cap slide", "2018-01-01", "2019-12-31"},
	{"March 2020", "2020-01-01", "2020-06-30"},
	{"2021-22 fall", "2021-10-01", "2022-09-30"},
	{"2024-25 fall", "2024-09-01", "2025-06-30"},
}

func sortedNames(p *regime.Panel) []string {
	out := make([]string, 0, len(p.Series))
	for n := range p.Series {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func dateOrDash(d market.Date) string {
	if d.IsZero() {
		return "-"
	}
	return d.String()
}

func runMarketCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("market check", flag.ContinueOnError)
	dir := fs.String("data", "data/market", "data directory")
	catalog := fs.String("catalog", "configs/indices.yaml", "index catalog")
	options := fs.String("options-data", "data/upstox", "options data, whose NIFTY days are compared with the index calendar")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cat, err := nse.LoadCatalog(*catalog)
	if err != nil {
		return err
	}
	store, err := data.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()
	rows, err := loadIndexRows(ctx, store)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return errors.New("no index data: run serene market fetch")
	}
	p := regime.NewPanel(rows, marketMeta(cat))
	first, last := p.Dates[0], p.Dates[len(p.Dates)-1]
	fmt.Printf("%d rows, %d indices, %d trading days, %s to %s\n\n", len(rows), len(p.Series), len(p.Dates), first, last)

	// Continuity proves the aliases: a wrong one shows up as a level jump.
	// Dividend Points is a running tally and VIX publishes a different previous
	// close, so their jumps are expected.
	type tally struct {
		name        string
		count, gaps int
		worst       float64
		worstDate   market.Date
		from, to    string
	}
	byName := map[string]*tally{}
	var names []string
	total, atRename := 0, 0
	for _, b := range nse.Continuity(rows, p.Dates, 0.05) {
		if cat.Group(b.Name) == "" || strings.Contains(b.Name, "Dividend Points") || b.Name == regime.VIX {
			continue
		}
		t, ok := byName[b.Name]
		if !ok {
			t = &tally{name: b.Name}
			byName[b.Name] = t
			names = append(names, b.Name)
		}
		if b.Gap {
			t.gaps++
			continue
		}
		t.count++
		total++
		if !strings.EqualFold(b.From, b.To) {
			atRename++
			fmt.Printf("  rename break: %s %s stored %s published %s, %s -> %s\n", b.Date, b.Name, b.Stored, b.Said, b.From, b.To)
		}
		if pct := math.Abs(float64(b.Said-b.Stored)) / float64(b.Stored) * 100; pct > t.worst {
			t.worst, t.worstDate, t.from, t.to = pct, b.Date, b.From, b.To
		}
	}
	sort.Slice(names, func(i, j int) bool { return byName[names[i]].worst > byName[names[j]].worst })
	fmt.Printf("Continuity breaks on consecutive days: %d, in %d indices. On a rename: %d\n", total, len(names), atRename)
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "index\tgroup\tbreaks\tacross gaps\tworst\ton\tnames that day")
	for i, n := range names {
		if i == 40 {
			fmt.Fprintf(w, "... %d more\n", len(names)-i)
			break
		}
		t := byName[n]
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%.2f%%\t%s\t%s -> %s\n", n, cat.Group(n), t.count, t.gaps, t.worst, dateOrDash(t.worstDate), t.from, t.to)
	}
	w.Flush()

	fmt.Println("\nRebases, adjusted out of history:")
	reb := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	for _, n := range sortedNames(p) {
		for _, r := range p.Series[n].Rebases {
			fmt.Fprintf(reb, "%s\t%s\t×%.3f\n", r.Date, n, r.Ratio)
		}
	}
	reb.Flush()

	if o, err := data.Open(*options); err == nil {
		defer o.Close()
		days, err := o.Days("NIFTY", first, last)
		if err != nil {
			return err
		}
		stored := map[market.Date]bool{}
		for _, d := range p.Dates {
			stored[d] = true
		}
		onOptions := map[market.Date]bool{}
		var noIndex, noOptions []string
		for _, d := range days {
			onOptions[d] = true
			if !stored[d] {
				noIndex = append(noIndex, d.String())
			}
		}
		if len(days) > 0 {
			for _, d := range p.Dates {
				if !d.Before(days[0]) && !onOptions[d] {
					noOptions = append(noOptions, d.String())
				}
			}
		}
		fmt.Printf("\nCalendar against %d NIFTY days. Missing from index files: %s\n", len(days), listOrNone(noIndex))
		fmt.Printf("Index files with no NIFTY day: %s\n", listOrNone(noOptions))
	}

	days := regime.Timeline(p)
	if len(days) == 0 {
		return fmt.Errorf("no history for %s", regime.Market)
	}
	fmt.Printf("\nRegime changes, %s to %s\n", days[0].Date, days[len(days)-1].Date)
	w = tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "date\tchange\tNifty 500\tdrawdown\tdays in previous")
	changes := regime.Transitions(days)
	for _, t := range changes {
		fmt.Fprintf(w, "%s\t%s to %s\t%.0f\t%.1f%%\t%d\n", t.Date, t.From, t.To, t.Close, t.Drawdown, t.Days)
	}
	w.Flush()

	windows := make([]regime.Window, 0, len(marketWindows))
	for _, m := range marketWindows {
		from, err := market.ParseDate(m.from)
		if err != nil {
			return err
		}
		to, err := market.ParseDate(m.to)
		if err != nil {
			return err
		}
		windows = append(windows, regime.Window{Name: m.name, From: from, To: to})
	}
	fmt.Println("\nKnown falls")
	w = tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "episode\tpeak\ttrough\tfall\tleft Bull\tat\tentered Bear\tat\tback to Bull\tabove trough")
	for _, e := range regime.Review(days, windows) {
		fmt.Fprintf(w, "%s\t%s\t%s\t%.1f%%\t%s\t%.1f%%\t%s\t%.1f%%\t%s\t%.1f%%\n", e.Name, dateOrDash(e.Peak), dateOrDash(e.Trough), e.Fall,
			dateOrDash(e.LeftBull), e.LeftBullDD, dateOrDash(e.EnteredBear), e.BearDD, dateOrDash(e.Bull), e.BullGain)
	}
	w.Flush()

	bearOutside := 0
	for _, t := range changes {
		if t.To != regime.Bear {
			continue
		}
		inside := false
		for _, win := range windows {
			inside = inside || (!t.Date.Before(win.From) && !win.To.Before(t.Date))
		}
		if !inside {
			bearOutside++
		}
	}
	fmt.Printf("\nBear calls outside those falls: %d\n", bearOutside)

	const forward = 63
	fmt.Printf("\nWhat followed each state over %d trading days. In-sample and overlapping, so this describes history and proves no edge.\n", forward)
	w = tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "state\tdays\tshare\tmean return\tpositive\tworst")
	for _, s := range regime.Stats(days, forward) {
		fmt.Fprintf(w, "%s\t%d\t%.0f%%\t%+.1f%%\t%.0f%%\t%+.1f%%\n", s.State, s.Days, s.Share, s.Forward, s.Positive, s.WorstNext)
	}
	return w.Flush()
}

func listOrNone(v []string) string {
	if len(v) == 0 {
		return "none"
	}
	return fmt.Sprintf("%d: %s", len(v), strings.Join(v, ", "))
}

func runAuth(ctx context.Context, args []string) error {
	store, err := openTokens(ctx)
	if err != nil {
		return err
	}
	defer store.Close()

	fs := flag.NewFlagSet("auth", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "local address; must match the redirect URLs registered with each broker")
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	base := "http://" + *addr
	providers := auth.ProvidersFromEnv(base)
	srv := &auth.Server{Addr: *addr, Providers: providers, Store: store, UI: web.Dist(),
		Grace: 30 * time.Second, Idle: 10 * time.Minute}

	switch sub {
	case "status":
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "broker\tstate\tuser\texpires")
		for _, s := range srv.Statuses(ctx) {
			expires := "-"
			if s.ExpiresAt != nil {
				expires = s.ExpiresAt.In(market.IST).Format("Mon 2 Jan 15:04 IST")
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Name, s.State, s.UserID, expires)
		}
		return w.Flush()
	case "logout":
		if fs.NArg() != 1 {
			return errors.New("usage: serene auth logout BROKER")
		}
		name := fs.Arg(0)
		known := false
		for _, b := range auth.Brokers {
			known = known || b.Name == name
		}
		if !known {
			return fmt.Errorf("unknown broker %q", name)
		}
		if err := auth.Logout(ctx, store, providers[name], name, time.Now()); err != nil {
			return err
		}
		fmt.Printf("logged out of %s\n", name)
		return nil
	case "":
	default:
		return fmt.Errorf("unknown auth command %q", sub)
	}

	if tls, err := store.TLS(ctx); err == nil && !tls {
		fmt.Fprintln(os.Stderr, "warning: the database connection isn't using TLS. Tokens are encrypted anyway, but other data is not.")
	}
	if len(providers) == 0 {
		fmt.Fprintln(os.Stderr, "warning: no broker keys found in .env")
	}
	if u, ok := providers["upstox"].(*auth.Upstox); ok {
		fmt.Println("Upstox redirect URI (must exactly match your app):", u.RedirectURI)
	}
	fmt.Println("Opening", base)
	fmt.Println("serene auth stops once every configured broker is logged in, or after 10 idle minutes.")
	openBrowser(base)
	return srv.Run(ctx)
}

// openBrowser is best effort, because the URL has already been printed.
func openBrowser(url string) {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	if cmd := exec.Command(name, url); cmd.Start() == nil {
		go cmd.Wait()
	}
}

func runSynth(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("synth", flag.ContinueOnError)
	from := fs.String("from", "", "first date")
	to := fs.String("to", "", "last date")
	dir := fs.String("data", "data/synthetic", "data directory")
	seed := fs.Uint64("seed", 1, "random seed")
	underlying := fs.String("underlying", "NIFTY", "underlying name")
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
	store, err := data.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()
	cfg := synth.Default(f, t, *seed)
	cfg.Underlying = *underlying
	days, err := synth.Generate(ctx, store, cfg)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %d synthetic trading days to %s\n", days, *dir)
	return nil
}

func loadConfig(path string) (config, analytics.Spec, error) {
	var c config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, analytics.Spec{}, err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true) // a misspelt setting must not fall back to a default
	if err := dec.Decode(&c); err != nil {
		return c, analytics.Spec{}, fmt.Errorf("parse %s: %w", path, err)
	}
	from, err := market.ParseDate(c.From)
	if err != nil {
		return c, analytics.Spec{}, err
	}
	to, err := market.ParseDate(c.To)
	if err != nil {
		return c, analytics.Spec{}, err
	}
	costs, err := cost.Load(c.CostsFile)
	if err != nil {
		return c, analytics.Spec{}, err
	}
	simCfg := sim.DefaultConfig()
	simCfg.SlippageBps = c.Sim.SlippageBps
	simCfg.MinTicks = c.Sim.MinTicks
	if c.Sim.MaxWaitMins > 0 {
		simCfg.MaxWait = time.Duration(c.Sim.MaxWaitMins) * time.Minute
	}
	if c.Margin.NakedPct <= 0 {
		return c, analytics.Spec{}, errors.New("margin.naked_pct must be positive")
	}
	if c.Capital <= 0 {
		return c, analytics.Spec{}, errors.New("capital must be positive")
	}
	spec := analytics.Spec{Strategy: c.Strategy, Underlying: c.Underlying, Params: c.Params,
		From: from, To: to, Capital: market.Rupees(c.Capital), Sim: simCfg,
		Margin: margin.Model{NakedPct: c.Margin.NakedPct}, Costs: costs}
	if r := c.Risk; r != nil {
		spec.Risk = &risk.Limits{MaxDailyLoss: market.Rupees(r.MaxDailyLoss), MaxLotsPerLeg: r.MaxLotsPerLeg,
			MaxOpenLots: r.MaxOpenLots, MaxOrdersPerMinute: r.MaxOrdersPerMinute,
			MaxQuoteAge: time.Duration(r.MaxQuoteAgeSeconds) * time.Second, MaxLimitAwayPct: r.MaxLimitAwayPct}
		if _, err := risk.New(*spec.Risk); err != nil {
			return c, analytics.Spec{}, err
		}
	}
	return c, spec, nil
}

// settlementIndex names the NSE index each underlying settles against.
var settlementIndex = map[string]string{"NIFTY": "Nifty 50", "BANKNIFTY": "Nifty Bank"}

// settlementCloses loads the official closes expiries settle at. The setting is
// required, so a real-data run can't settle at the last bar by omission.
func settlementCloses(ctx context.Context, c config, spec analytics.Spec) (map[market.Date]market.Paise, error) {
	switch c.Settlement {
	case "":
		return nil, errors.New("settlement is required: the NSE data directory, or last_bar for synthetic data")
	case "last_bar":
		return nil, nil
	}
	name, ok := settlementIndex[spec.Underlying]
	if !ok {
		return nil, fmt.Errorf("no settlement index for %s", spec.Underlying)
	}
	store, err := data.Open(c.Settlement)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	rows, err := store.IndexDays(ctx, spec.From, spec.To)
	if err != nil {
		return nil, err
	}
	closes := map[market.Date]market.Paise{}
	for _, r := range rows {
		if r.Name == name {
			closes[r.Date] = r.Close
		}
	}
	return closes, nil
}

// regimeStates is the serene market regime at each date's close, built from the
// settlement data directory's index history. A date's state uses only that
// date and earlier, so a strategy reading the previous close sees nothing
// from the future.
func regimeStates(ctx context.Context, c config) (map[market.Date]string, error) {
	if c.Settlement == "" || c.Settlement == "last_bar" {
		return nil, errors.New("the regime filter needs settlement set to the NSE data directory")
	}
	cat, err := nse.LoadCatalog("configs/indices.yaml")
	if err != nil {
		return nil, err
	}
	store, err := data.Open(c.Settlement)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	rows, err := loadIndexRows(ctx, store)
	if err != nil {
		return nil, err
	}
	days := regime.Timeline(regime.NewPanel(rows, marketMeta(cat)))
	if len(days) == 0 {
		return nil, fmt.Errorf("no regime history in %s", c.Settlement)
	}
	out := make(map[market.Date]string, len(days))
	for _, d := range days {
		out[d.Date] = string(d.State)
	}
	return out, nil
}

func runResearch(ctx context.Context, action, path, holdoutPath string, unseal bool) error {
	c, spec, err := loadConfig(path)
	if err != nil {
		return err
	}
	if note, ok := strategies.Rejected[spec.Strategy]; ok {
		fmt.Fprintf(os.Stderr, "REJECTED: %s %s\n", spec.Strategy, note)
	}
	holdouts, err := analytics.LoadHoldouts(holdoutPath)
	if err != nil {
		return err
	}
	unsealed, err := holdouts.Check(c.DataDir, spec.To, unseal)
	if err != nil {
		return err
	}
	if unsealed {
		fmt.Fprintf(os.Stderr, "UNSEALED: this run reads held-back data in %s, and the ledger records it\n", c.DataDir)
	}
	configSHA, err := analytics.FileSHA256(path)
	if err != nil {
		return err
	}
	costsSHA, err := analytics.FileSHA256(c.CostsFile)
	if err != nil {
		return err
	}
	r := researchRun{c: c, spec: spec, unsealed: unsealed, ledger: analytics.LedgerPath(c.DataDir),
		configPath: path, configSHA: configSHA, costsSHA: costsSHA}
	store, err := data.Open(c.DataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	if spec.Closes, err = settlementCloses(ctx, c, spec); err != nil {
		return err
	}
	if _, swept := c.Sweep.Grid["regime"]; spec.Params["regime"] != 0 || swept {
		if spec.Regime, err = regimeStates(ctx, c); err != nil {
			return err
		}
	}
	// r was built from spec before the closes and regime were loaded.
	r.spec = spec
	metric := c.Sweep.Metric
	if metric == "" {
		metric = "sharpe"
	}
	switch action {
	case "backtest":
		return backtest(ctx, store, r)
	case "sweep":
		if len(c.Sweep.Grid) == 0 {
			return errors.New("sweep.grid is empty")
		}
		return sweep(ctx, store, r, metric)
	default:
		if len(c.Sweep.Grid) == 0 {
			return errors.New("sweep.grid is empty")
		}
		return walkForward(ctx, store, r, metric)
	}
}

// researchRun is one research command's settings and where it records trials.
type researchRun struct {
	c        config
	spec     analytics.Spec
	unsealed bool
	ledger   string

	configPath, configSHA, costsSHA string
}

// record adds the run's trials to the ledger and returns the strategy's total.
func (r researchRun) record(recs ...analytics.TrialRecord) (analytics.Provenance, error) {
	for i := range recs {
		recs[i].Config = r.configSHA
	}
	if err := analytics.AppendTrials(r.ledger, recs); err != nil {
		return analytics.Provenance{}, fmt.Errorf("trial ledger: %w", err)
	}
	n, u, err := analytics.TrialCount(r.ledger, r.spec.Strategy, r.spec.Underlying)
	if err != nil {
		return analytics.Provenance{}, fmt.Errorf("trial ledger: %w", err)
	}
	fmt.Printf("trials of %s on %s in %s: %d", r.spec.Strategy, r.spec.Underlying, r.c.DataDir, n)
	if u > 0 {
		fmt.Printf(", %d run(s) on held-back data", u)
	}
	fmt.Println()
	return analytics.Provenance{Trials: n, UnsealedRuns: u, Unsealed: r.unsealed}, nil
}

func backtest(ctx context.Context, store *data.Store, r researchRun) error {
	c, spec := r.c, r.spec
	o, err := analytics.Run(ctx, store, spec)
	if err != nil {
		return err
	}
	slip, err := analytics.SlippageSensitivity(ctx, store, spec, []float64{0, 1, 2, 3})
	if err != nil {
		return err
	}
	m := o.Metrics
	fmt.Printf("%s on %s, %s to %s (%d days)\n", spec.Strategy, spec.Underlying, spec.From, spec.To, m.Days)
	fmt.Printf("net ₹%s  gross ₹%s  charges ₹%s  return %.2f%%\n", m.Net, m.Gross, m.Charges, m.ReturnPct)
	fmt.Printf("sharpe %.2f  max drawdown %.2f%%  trades %d  win rate %.1f%%  profit factor %.2f\n",
		m.Sharpe, m.MaxDrawdownPct, m.Trades, m.WinRatePct, m.ProfitFactor)
	fmt.Printf("settled legs %d  unfilled orders %d  rejected orders %d\n", m.SettledLegs, m.Unfilled, m.Rejected)
	prov, err := r.record(analytics.Record("backtest", spec, m, 1, r.unsealed, analytics.Revision(), time.Now()))
	if err != nil {
		return err
	}
	reportPath := filepath.Join(c.ReportDir, fmt.Sprintf("%s-%s.html", spec.Strategy, spec.Underlying))
	if err := analytics.WriteReport(reportPath, o, slip, prov); err != nil {
		return err
	}
	manifest := analytics.NewManifest(o, prov, r.configPath, r.configSHA, r.costsSHA, c.DataDir, time.Now())
	if err := analytics.WriteManifest(strings.TrimSuffix(reportPath, ".html")+".json", manifest); err != nil {
		return err
	}
	fmt.Println("report:", reportPath)
	return nil
}

func paramString(p strategy.Params) string {
	return strings.TrimPrefix(fmt.Sprint(map[string]float64(p)), "map")
}

func sweep(ctx context.Context, store *data.Store, r researchRun, metric string) error {
	c, spec := r.c, r.spec
	combos := analytics.Grid(c.Params, c.Sweep.Grid)
	trials, err := analytics.Sweep(ctx, store, spec, combos)
	if err != nil {
		return err
	}
	rev, now := analytics.Revision(), time.Now()
	recs := make([]analytics.TrialRecord, 0, len(trials))
	for _, t := range trials {
		v := spec
		v.Params = t.Params
		recs = append(recs, analytics.Record("sweep", v, t.Metrics, 1, r.unsealed, rev, now))
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "params\tnet\tsharpe\tmax dd\ttrades\twin rate\tpf")
	for _, t := range trials {
		m := t.Metrics
		fmt.Fprintf(w, "%s\t₹%s\t%.2f\t%.2f%%\t%d\t%.1f%%\t%.2f\n", paramString(t.Params), m.Net, m.Sharpe, m.MaxDrawdownPct, m.Trades, m.WinRatePct, m.ProfitFactor)
	}
	w.Flush()
	if best, ok := analytics.Best(trials, metric); ok {
		fmt.Printf("\nbest of %d by %s: %s\n", len(trials), metric, paramString(best.Params))
		fmt.Println("This is in-sample and the best of many tries, so expect it to be overstated. Run walkforward next.")
	}
	_, err = r.record(recs...)
	return err
}

func walkForward(ctx context.Context, store *data.Store, r researchRun, metric string) error {
	c, spec := r.c, r.spec
	combos := analytics.Grid(c.Params, c.Sweep.Grid)
	wf, err := analytics.RunWalkForward(ctx, store, spec, combos, metric, c.WalkForward.TrainDays, c.WalkForward.TestDays)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "test window\tchosen params\ttrain sharpe\ttest sharpe\ttest net")
	for _, x := range wf.Windows {
		fmt.Fprintf(w, "%s to %s\t%s\t%.2f\t%.2f\t₹%s\n", x.TestFrom, x.TestTo, paramString(x.Params),
			x.InSample.Sharpe, x.OutOfSample.Sharpe, x.OutOfSample.Net)
	}
	w.Flush()
	m := wf.Combined
	fmt.Printf("\nout of sample, %d windows, %d combinations per window:\n", len(wf.Windows), wf.Trials)
	fmt.Printf("net ₹%s  charges ₹%s  sharpe %.2f  max drawdown %.2f%%  trades %d  win rate %.1f%%\n",
		m.Net, m.Charges, m.Sharpe, m.MaxDrawdownPct, m.Trades, m.WinRatePct)
	// Every combination was scored on every training window to choose parameters.
	v := spec
	v.Params = nil
	_, err = r.record(analytics.Record("walkforward", v, m, wf.Trials*len(wf.Windows), r.unsealed, analytics.Revision(), time.Now()))
	return err
}
