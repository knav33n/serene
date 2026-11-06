// Command calm researches and trades Indian index options.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"

	"calm/internal/analytics"
	"calm/internal/auth"
	"calm/internal/cost"
	"calm/internal/data"
	"calm/internal/margin"
	"calm/internal/market"
	"calm/internal/sim"
	"calm/internal/strategy"
	"calm/internal/synth"
	"calm/web"
)

const usage = `calm researches and trades Indian index options.

  calm auth                  open the broker login page
  calm auth status           show each broker's login, never the token
  calm auth logout BROKER    revoke a token at the broker and delete it

  calm research synth       --from DATE --to DATE [--underlying NIFTY] [--seed N] [--data DIR]
  calm research backtest    --config FILE
  calm research sweep       --config FILE
  calm research walkforward --config FILE

Not built yet:
  calm research fetch, calm research coverage  (needs Upstox Plus)
  calm prod paper, calm prod live              (needs Kite Connect)
`

// config is the YAML run file.
type config struct {
	Strategy   string          `yaml:"strategy"`
	Underlying string          `yaml:"underlying"`
	From       string          `yaml:"from"`
	To         string          `yaml:"to"`
	Capital    float64         `yaml:"capital"`
	DataDir    string          `yaml:"data_dir"`
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
		fmt.Fprintln(os.Stderr, "calm:", err)
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
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *path == "" {
			return errors.New("--config is required")
		}
		return runResearch(ctx, args[1], *path)
	case "research fetch", "research coverage", "prod paper", "prod live":
		return fmt.Errorf("%q is not built yet", cmd)
	}
	fmt.Print(usage)
	return fmt.Errorf("unknown command %q", cmd)
}

func runAuth(ctx context.Context, args []string) error {
	if err := auth.LoadDotEnv(".env"); err != nil {
		return err
	}
	sealer, err := auth.NewSealer(os.Getenv("CALM_TOKEN_KEY"))
	if err != nil {
		return err
	}
	store, err := auth.OpenPostgres(ctx, os.Getenv("CALM_DATABASE_URL"), sealer, "calm")
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
			return errors.New("usage: calm auth logout BROKER")
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
	fmt.Println("calm auth stops once every configured broker is logged in, or after 10 idle minutes.")
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
	return c, analytics.Spec{Strategy: c.Strategy, Underlying: c.Underlying, Params: c.Params,
		From: from, To: to, Capital: market.Rupees(c.Capital), Sim: simCfg,
		Margin: margin.Model{NakedPct: c.Margin.NakedPct}, Costs: costs}, nil
}

func runResearch(ctx context.Context, action, path string) error {
	c, spec, err := loadConfig(path)
	if err != nil {
		return err
	}
	store, err := data.Open(c.DataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	metric := c.Sweep.Metric
	if metric == "" {
		metric = "sharpe"
	}
	switch action {
	case "backtest":
		return backtest(ctx, store, c, spec)
	case "sweep":
		if len(c.Sweep.Grid) == 0 {
			return errors.New("sweep.grid is empty")
		}
		return sweep(ctx, store, c, spec, metric)
	default:
		if len(c.Sweep.Grid) == 0 {
			return errors.New("sweep.grid is empty")
		}
		return walkForward(ctx, store, c, spec, metric)
	}
}

func backtest(ctx context.Context, store *data.Store, c config, spec analytics.Spec) error {
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
	reportPath := filepath.Join(c.ReportDir, fmt.Sprintf("%s-%s.html", spec.Strategy, spec.Underlying))
	if err := analytics.WriteReport(reportPath, o, slip); err != nil {
		return err
	}
	fmt.Println("report:", reportPath)
	return nil
}

func paramString(p strategy.Params) string {
	return strings.TrimPrefix(fmt.Sprint(map[string]float64(p)), "map")
}

func sweep(ctx context.Context, store *data.Store, c config, spec analytics.Spec, metric string) error {
	combos := analytics.Grid(c.Params, c.Sweep.Grid)
	trials, err := analytics.Sweep(ctx, store, spec, combos)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "params\tnet\tsharpe\tmax dd\ttrades\twin rate")
	for _, t := range trials {
		m := t.Metrics
		fmt.Fprintf(w, "%s\t₹%s\t%.2f\t%.2f%%\t%d\t%.1f%%\n", paramString(t.Params), m.Net, m.Sharpe, m.MaxDrawdownPct, m.Trades, m.WinRatePct)
	}
	w.Flush()
	if best, ok := analytics.Best(trials, metric); ok {
		fmt.Printf("\nbest of %d by %s: %s\n", len(trials), metric, paramString(best.Params))
		fmt.Println("This is in-sample and the best of many tries, so expect it to be overstated. Run walkforward next.")
	}
	return nil
}

func walkForward(ctx context.Context, store *data.Store, c config, spec analytics.Spec, metric string) error {
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
	return nil
}
