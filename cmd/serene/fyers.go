package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"

	"serene/internal/auth"
	"serene/internal/data"
	"serene/internal/fobhav"
	"serene/internal/fyers"
	fyingest "serene/internal/fyers/ingest"
	"serene/internal/market"
)

// fyersConfig is configs/fyers.yaml.
type fyersConfig struct {
	Plan string `yaml:"plan"`
	// ExpiredPerDay caps expired-contract intraday requests a day, for the
	// limit Fyers enforces below its published Prime caps; 0 keeps the plan's.
	ExpiredPerDay int      `yaml:"expired_per_day"`
	Dir           string   `yaml:"dir"`
	UpstoxDir     string   `yaml:"upstox_dir"`
	BandPct       float64  `yaml:"band_pct"`
	Workers       int      `yaml:"workers"`
	Underlyings   []string `yaml:"underlyings"`
	Stocks        []string `yaml:"stocks"`
	// StocksFile adds every stock in a universe.csv that serene fyers lots
	// writes: each stock with options on any day since 2018-10, delisted too.
	StocksFile string   `yaml:"stocks_file"`
	Segments   []string `yaml:"segments"`
}

func (c fyersConfig) all() ([]fyers.Underlying, error) {
	names := append(append([]string{}, c.Underlyings...), c.Stocks...)
	if c.StocksFile != "" {
		path := c.StocksFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(c.Dir, path)
		}
		uni, err := fobhav.ReadUniverse(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		names = append(names, uni...)
	}
	var out []fyers.Underlying
	seen := map[string]bool{}
	for _, n := range names {
		u := fyers.Lookup(strings.ToUpper(strings.TrimSpace(n)))
		if !seen[u.Name] {
			seen[u.Name] = true
			out = append(out, u)
		}
	}
	return out, nil
}

func loadFyersConfig(path string) (fyersConfig, error) {
	c := fyersConfig{Plan: "prime", Dir: "/run/media/naveen/Seagate Expansion Drive/INSIGHTS/fyers", BandPct: 5, Workers: 4}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// fyersSession opens the store, the limiter and the client.
type fyersSession struct {
	cfg    fyersConfig
	store  *data.Store
	client *fyers.Client
	ingest fyingest.Config
}

func openFyers(ctx context.Context, configPath string, needToken bool) (*fyersSession, error) {
	cfg, err := loadFyersConfig(configPath)
	if err != nil {
		return nil, err
	}
	limits, err := fyers.LimitsFor(cfg.Plan)
	if err != nil {
		return nil, err
	}
	if cfg.ExpiredPerDay > 0 {
		limits.ExpiredIntraday = cfg.ExpiredPerDay
	}
	lim, err := fyers.NewLimiter(limits, filepath.Join(cfg.Dir, "fetch", "usage.json"))
	if err != nil {
		return nil, err
	}
	var token auth.Secret
	if needToken {
		tokens, err := openTokens(ctx)
		if err != nil {
			return nil, err
		}
		t, err := auth.Load(ctx, tokens, "fyers")
		tokens.Close()
		if err != nil {
			return nil, err
		}
		token = t.AccessToken
	}
	appID := os.Getenv("FYERS_APP_ID")
	if needToken && appID == "" {
		return nil, errors.New("FYERS_APP_ID is not set")
	}
	store, err := data.Open(cfg.Dir)
	if err != nil {
		return nil, err
	}
	return &fyersSession{cfg: cfg, store: store, client: fyers.New(appID, token, lim),
		ingest: fyingest.Config{Dir: cfg.Dir, UpstoxDir: cfg.UpstoxDir, BandPct: cfg.BandPct,
			Workers: cfg.Workers, Log: stamp}}, nil
}

func stamp(format string, a ...any) {
	fmt.Printf("%s "+format+"\n", append([]any{time.Now().In(market.IST).Format("15:04:05")}, a...)...)
}

// pick returns the named underlyings, or every configured one.
func (s *fyersSession) pick(names string) ([]fyers.Underlying, error) {
	if names == "" {
		return s.cfg.all()
	}
	var out []fyers.Underlying
	for _, n := range strings.Split(names, ",") {
		out = append(out, fyers.Lookup(strings.ToUpper(strings.TrimSpace(n))))
	}
	return out, nil
}

func runFyers(ctx context.Context, sub string, args []string) error {
	fs := flag.NewFlagSet("fyers "+sub, flag.ContinueOnError)
	configPath := fs.String("config", "configs/fyers.yaml", "Fyers download config")
	names := fs.String("underlying", "", "comma-separated underlyings; default every configured one")
	from := fs.String("from", "", "first date (default: the earliest Fyers holds)")
	to := fs.String("to", "2100-01-01", "last date")
	size := fs.Int64("size", 0, "lot size (fyers lot)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := openFyers(ctx, *configPath, sub != "lot" && sub != "lots")
	if err != nil {
		return err
	}
	defer s.store.Close()
	if sub == "lots" {
		return runFyersLots(ctx, s)
	}
	us, err := s.pick(*names)
	if err != nil {
		return err
	}
	t, err := market.ParseDate(*to)
	if err != nil {
		return err
	}
	var f market.Date
	if *from != "" {
		if f, err = market.ParseDate(*from); err != nil {
			return err
		}
	}
	defer func() {
		u := s.client.Limiter.Usage()
		fmt.Printf("%d Fyers requests today (%s plan)\n", u.Requests, s.client.Limiter.Limits().Name)
	}()

	switch sub {
	case "probe":
		rows, err := fyingest.Probe(ctx, s.ingest, s.store, s.client, us)
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "\ncheck\tsymbol\tvalue\tnote")
		for _, r := range rows {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Check, r.Symbol, r.Value, r.Note)
		}
		w.Flush()
		fmt.Println("saved to", filepath.Join(s.cfg.Dir, "probe.parquet"))
		return err
	case "index":
		if f.IsZero() {
			f = fyingest.IndexFrom
		}
		for _, u := range us {
			if _, err := fyingest.Index(ctx, s.ingest, s.store, s.client, u, f); err != nil {
				return fmt.Errorf("%s: %w", u.Name, err)
			}
		}
		return nil
	case "backfill":
		for _, u := range us {
			if err := fyingest.Backfill(ctx, s.ingest, s.store, s.client, u, f, t); err != nil {
				return err
			}
		}
		return nil
	case "update":
		return fyingest.Update(ctx, s.ingest, s.store, s.client, us, s.cfg.Segments)
	case "all":
		return runFyersAll(ctx, s, us)
	case "lot":
		if *names == "" || len(us) != 1 || f.IsZero() || *size <= 0 {
			return errors.New("usage: serene fyers lot --underlying NAME --from DATE --size N")
		}
		if err := s.store.PutLots(ctx, us[0].Name, []data.Lot{{From: f, Size: *size, Source: "manual"}}); err != nil {
			return err
		}
		lots, err := s.store.Lots(ctx, us[0].Name)
		if err != nil {
			return err
		}
		for _, l := range lots {
			fmt.Printf("%s  %s from %s: %d\n", us[0].Name, l.Source, l.From, l.Size)
		}
		return nil
	}
	return fmt.Errorf("unknown fyers command %q", sub)
}
