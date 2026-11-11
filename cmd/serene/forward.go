package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"serene/internal/analytics"
	"serene/internal/auth"
	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/kite"
	"serene/internal/market"
	"serene/internal/mcx"
	"serene/internal/slack"
)

// fwdSignal is one entry or exit of a forward test. It carries no P&L: the
// forward tests stay unread until their one judgement, so only the orders go
// out, as they would to a broker.
type fwdSignal struct {
	Strategy   string
	Time       time.Time
	Side       market.Side
	Instrument string
	Price      market.Paise
	Note       string
}

// id is stable across runs, so a signal is posted once however often this runs.
func (s fwdSignal) id() string {
	return fmt.Sprintf("%s|%s|%s|%s", s.Strategy, s.Time.UTC().Format(time.RFC3339), s.Side, s.Instrument)
}

func (s fwdSignal) text() string {
	icon := ":large_green_circle:"
	if s.Side == market.Sell {
		icon = ":red_circle:"
	}
	t := fmt.Sprintf("%s *%s* %s %s @ ₹%s · %s", icon, s.Strategy, s.Side, s.Instrument, s.Price,
		s.Time.In(market.IST).Format("2006-01-02 15:04"))
	if s.Note != "" {
		t += " · " + s.Note
	}
	return t
}

// runForwardSignals posts the frozen forward tests' new entries and exits.
// With --fetch it first brings GOLDM's minute bars up to now from Kite, which
// is how hearth runs it each minute of the MCX session.
func runForwardSignals(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("forward signals", flag.ContinueOnError)
	useSlack := fs.Bool("slack", false, "post new signals to SLACK_TRADES_CHANNEL")
	fetch := fs.Bool("fetch", false, "fetch GOLDM minute bars from Kite first")
	only := fs.String("strategies", "goldm_ema_15m", "comma-separated: goldm_ema_15m, ema_cross_first")
	sentPath := fs.String("sent", "data/forward_sent.json", "signals already posted")
	alertPath := fs.String("alert", "data/forward_alert.json", "whether a failure has been posted")
	niftyCfg := fs.String("nifty-config", "configs/ema_cross_first_forward.yaml", "ema_cross_first forward config")
	mcxDir := fs.String("mcx-data", "data/mcx", "MCX data directory")
	mcxCosts := fs.String("mcx-costs", "configs/costs_mcx.yaml", "MCX charges schedule")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var client *slack.Client
	var channel string
	if *useSlack {
		var err error
		if client, channel, err = openSlackChannel(ctx, "SLACK_TRADES_CHANNEL"); err != nil {
			return err
		}
	}
	err := forwardSignals(ctx, client, channel, *fetch, strings.Split(*only, ","), *sentPath, *niftyCfg, *mcxDir, *mcxCosts)
	if client != nil {
		// It runs every minute, so a failure is posted once, and so is the recovery.
		failing := fileExists(*alertPath)
		post := func(text string) {
			if perr := client.Post(context.WithoutCancel(ctx), channel, text); perr != nil {
				fmt.Fprintln(os.Stderr, "serene: slack:", perr)
			}
		}
		switch {
		case err != nil && !failing:
			post(fmt.Sprintf(":x: *serene forward signals failed:* %v\nSignals are paused until it recovers; this is posted once.", err))
			_ = os.WriteFile(*alertPath, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644)
		case err == nil && failing:
			post(":white_check_mark: *serene forward signals recovered*")
			_ = os.Remove(*alertPath)
		}
	}
	return err
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func forwardSignals(ctx context.Context, client *slack.Client, channel string, fetch bool, only []string,
	sentPath, niftyCfg, mcxDir, mcxCosts string) error {
	var all []fwdSignal
	for _, name := range only {
		switch strings.TrimSpace(name) {
		case "ema_cross_first":
			s, err := emaCrossFirstSignals(ctx, niftyCfg)
			if err != nil {
				return fmt.Errorf("ema_cross_first: %w", err)
			}
			all = append(all, s...)
		case "goldm_ema_15m":
			if fetch {
				if err := fetchGOLDM(ctx, mcxDir); err != nil {
					return fmt.Errorf("GOLDM fetch: %w", err)
				}
			}
			s, err := goldmEMASignals(ctx, mcxDir, mcxCosts)
			if err != nil {
				return fmt.Errorf("goldm_ema_15m: %w", err)
			}
			all = append(all, s...)
		default:
			return fmt.Errorf("unknown strategy %q", name)
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Time.Before(all[j].Time) })

	sent, err := loadSent(sentPath)
	if err != nil {
		return err
	}
	var fresh []fwdSignal
	for _, s := range all {
		if !sent[s.id()] {
			fresh = append(fresh, s)
		}
	}
	if len(fresh) == 0 {
		fmt.Println("no new signals")
		return nil
	}
	for _, s := range fresh {
		fmt.Println(s.text())
	}
	if client == nil {
		fmt.Printf("%d new signal(s); --slack posts them\n", len(fresh))
		return nil
	}
	// Slack's message limit is about 40k characters; batch well under it.
	const perMessage = 40
	for i := 0; i < len(fresh); i += perMessage {
		batch := fresh[i:min(i+perMessage, len(fresh))]
		lines := make([]string, len(batch))
		for j, s := range batch {
			lines[j] = s.text()
		}
		if err := client.Post(ctx, channel, strings.Join(lines, "\n")); err != nil {
			return err
		}
		for _, s := range batch {
			sent[s.id()] = true
		}
		// Saved after each message, so a failure part-way doesn't repost.
		if err := saveSent(sentPath, sent); err != nil {
			return err
		}
	}
	fmt.Printf("posted %d signal(s)\n", len(fresh))
	return nil
}

// fetchGOLDM brings GOLDM's minute bars up to now, copying the Kite login
// from project_calm first when CALM_DATABASE_URL is set.
func fetchGOLDM(ctx context.Context, dir string) error {
	tokens, err := openTokens(ctx)
	if err != nil {
		return err
	}
	defer tokens.Close()
	if url := os.Getenv("CALM_DATABASE_URL"); url != "" {
		if _, err := auth.ImportCalm(ctx, url, tokens); err != nil {
			// The stored token may still work; a real failure shows below.
			fmt.Fprintln(os.Stderr, "project_calm tokens not copied:", err)
		}
	}
	token, err := auth.Load(ctx, tokens, "zerodha")
	if err != nil {
		return fmt.Errorf("%w (log in to Kite in project_calm)", err)
	}
	apiKey := os.Getenv("ZERODHA_API_KEY")
	if apiKey == "" {
		return errors.New("ZERODHA_API_KEY is not set")
	}
	lots, err := mcx.UpstoxLots(ctx)
	if err != nil {
		return err
	}
	store, err := data.Open(dir)
	if err != nil {
		return err
	}
	defer store.Close()
	cfg := mcx.Config{Dir: dir, Names: []string{gemName}, Minute: true,
		Lookback: 500 * 24 * time.Hour, Log: func(string, ...any) {}}
	return mcx.Run(ctx, cfg, store, kite.New(apiKey, token.AccessToken), lots)
}

// emaCrossFirstSignals replays ema_cross_first from its forward start to the
// last NIFTY day fetched and returns its fills. Nothing goes to the trial
// ledger: this reads orders, not results.
func emaCrossFirstSignals(ctx context.Context, path string) ([]fwdSignal, error) {
	c, spec, err := loadConfig(path)
	if err != nil {
		return nil, err
	}
	store, err := data.Open(c.DataDir)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	days, err := store.Days(spec.Underlying, spec.From, spec.To)
	if err != nil {
		return nil, err
	}
	if len(days) == 0 {
		return nil, nil
	}
	spec.To = days[len(days)-1]
	if spec.Closes, err = settlementCloses(ctx, c, spec); err != nil {
		return nil, err
	}
	o, err := analytics.Run(ctx, store, spec)
	if err != nil {
		return nil, err
	}
	out := make([]fwdSignal, 0, len(o.Result.Fills))
	for _, f := range o.Result.Fills {
		in := f.Instrument
		name := in.Underlying
		if in.Kind == market.KindOption {
			name = fmt.Sprintf("%s %s %s (exp %s)", in.Underlying, in.Strike, in.Right, in.Expiry)
		}
		note := ""
		if f.Kind == market.Settled {
			note = "settled at expiry"
		}
		out = append(out, fwdSignal{Strategy: spec.Strategy, Time: f.Time, Side: f.Side, Instrument: name, Price: f.Price, Note: note})
	}
	return out, nil
}

// goldmEMASignals replays the frozen GOLDM 15m 13/34 cross from its forward
// start. The last trade's "end" exit is only where the data stops, so it is
// left out: that position is still open.
func goldmEMASignals(ctx context.Context, dir, costsFile string) ([]fwdSignal, error) {
	spec, ok := mcx.Specs[gemName]
	if !ok {
		return nil, fmt.Errorf("no contract spec for %s", gemName)
	}
	costs, err := cost.Load(costsFile)
	if err != nil {
		return nil, err
	}
	store, err := data.Open(dir)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	days, err := store.Days(gemName, gemForward, gemForward.AddDays(3650))
	if err != nil {
		return nil, err
	}
	if len(days) == 0 {
		return nil, nil
	}
	to := days[len(days)-1]
	bars, err := loadMCXBars(ctx, store, []string{gemName}, gemForward.AddDays(-30), to)
	if err != nil {
		return nil, err
	}
	ts, err := mcx.EMABacktest(mcx.PrepareEMA(bars[gemName], gemFrame), mcx.EMAConfig{Name: gemName, Spec: spec,
		Frame: gemFrame, Fast: gemFast, Slow: gemSlow, Slip: 1, Costs: costs, From: gemForward, To: to})
	if err != nil {
		return nil, err
	}
	var out []fwdSignal
	for _, t := range ts {
		name := t.Key
		if in, err := market.ParseKey(t.Key); err == nil {
			name = fmt.Sprintf("%s FUT (exp %s)", in.Underlying, in.Expiry)
		}
		out = append(out, fwdSignal{Strategy: "goldm_ema_15m", Time: t.Entry, Side: t.Side, Instrument: name, Price: t.In, Note: "enter"})
		if t.Reason != "end" {
			out = append(out, fwdSignal{Strategy: "goldm_ema_15m", Time: t.Exit, Side: -t.Side, Instrument: name, Price: t.Out, Note: "exit, " + t.Reason})
		}
	}
	return out, nil
}

func loadSent(path string) (map[string]bool, error) {
	sent := map[string]bool{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return sent, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	if err := json.Unmarshal(b, &ids); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, id := range ids {
		sent[id] = true
	}
	return sent, nil
}

func saveSent(path string, sent map[string]bool) error {
	ids := make([]string, 0, len(sent))
	for id := range sent {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	b, err := json.MarshalIndent(ids, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// openSlackChannel is a Slack client and the channel ID named by env.
func openSlackChannel(ctx context.Context, env string) (*slack.Client, string, error) {
	if err := auth.LoadDotEnv(".env"); err != nil {
		return nil, "", err
	}
	token, target := os.Getenv("SLACK_TOKEN"), os.Getenv(env)
	if token == "" || target == "" {
		return nil, "", fmt.Errorf("--slack needs SLACK_TOKEN and %s in .env", env)
	}
	c := slack.New(token)
	channel, err := c.Resolve(ctx, target)
	return c, channel, err
}
