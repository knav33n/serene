package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
// With --fetch it first brings the MCX minute bars up to now from Kite, which
// is how hearth runs it each minute of the MCX session.
func runForwardSignals(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("forward signals", flag.ContinueOnError)
	useSlack := fs.Bool("slack", false, "post new signals to SLACK_TRADES_CHANNEL, SILVERM ones to SLACK_SILVERM_CHANNEL if set")
	fetch := fs.Bool("fetch", false, "fetch the MCX strategies' minute bars from Kite first")
	only := fs.String("strategies", "goldm_ema_15m", "comma-separated: goldm_ema_15m, goldm_hma_1h, silverm_ema_15m, silverm_hma_1h, goldm_hmasma_10m, silverm_hmasma_10m, ema_cross_first")
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
	route := func(string) string { return channel }
	if *useSlack {
		var err error
		if client, channel, err = openSlackChannel(ctx, "SLACK_TRADES_CHANNEL"); err != nil {
			return err
		}
		if target := os.Getenv("SLACK_SILVERM_CHANNEL"); target != "" {
			silverm, err := client.Resolve(ctx, target)
			if err != nil {
				return err
			}
			route = func(strat string) string {
				if mcxForwardUnderlying(strat) == smName {
					return silverm
				}
				return channel
			}
		}
	}
	err := forwardSignals(ctx, client, route, *fetch, strings.Split(*only, ","), *sentPath, *niftyCfg, *mcxDir, *mcxCosts)
	alertOnce(ctx, client, channel, *alertPath, "serene forward signals", err)
	return err
}

// alertOnce posts a failure of a command run every minute once, and its
// recovery once, using a marker file at path.
func alertOnce(ctx context.Context, client *slack.Client, channel, path, what string, err error) {
	if client == nil {
		return
	}
	failing := fileExists(path)
	post := func(text string) { postAlert(ctx, client, channel, text) }
	switch {
	case err != nil && !failing:
		post(fmt.Sprintf(":x: *%s failed:* %v\nSignals are paused until it recovers; this is posted once.", what, err))
		_ = os.WriteFile(path, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644)
	case err == nil && failing:
		post(fmt.Sprintf(":white_check_mark: *%s recovered*", what))
		_ = os.Remove(path)
	}
}

// postAlert posts a failure or recovery to SLACK_ALERT_CHANNEL, or to fallback
// when that isn't set or can't be resolved.
func postAlert(ctx context.Context, client *slack.Client, fallback, text string) {
	ctx = context.WithoutCancel(ctx)
	channel := fallback
	if target := os.Getenv("SLACK_ALERT_CHANNEL"); target != "" {
		if c, err := client.Resolve(ctx, target); err != nil {
			fmt.Fprintln(os.Stderr, "serene: SLACK_ALERT_CHANNEL:", err)
		} else {
			channel = c
		}
	}
	if err := client.Post(ctx, channel, text); err != nil {
		fmt.Fprintln(os.Stderr, "serene: slack:", err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// route is the Slack channel for a strategy's signals. Failure alerts go to
// SLACK_ALERT_CHANNEL, or SLACK_TRADES_CHANNEL without it.
func forwardSignals(ctx context.Context, client *slack.Client, route func(strategy string) string, fetch bool, only []string,
	sentPath, niftyCfg, mcxDir, mcxCosts string) error {
	var all []fwdSignal
	if fetch {
		var names []string
		for _, n := range only {
			if u := mcxForwardUnderlying(strings.TrimSpace(n)); u != "" && !slices.Contains(names, u) {
				names = append(names, u)
			}
		}
		if len(names) > 0 {
			if err := fetchMCX(ctx, mcxDir, names); err != nil {
				return fmt.Errorf("%s fetch: %w", strings.Join(names, ","), err)
			}
		}
	}
	for _, name := range only {
		switch strings.TrimSpace(name) {
		case "ema_cross_first":
			s, err := emaCrossFirstSignals(ctx, niftyCfg)
			if err != nil {
				return fmt.Errorf("ema_cross_first: %w", err)
			}
			all = append(all, s...)
		case "goldm_ema_15m", "goldm_hma_1h", "silverm_ema_15m", "silverm_hma_1h", "goldm_hmasma_10m", "silverm_hmasma_10m":
			s, err := mcxSignals(ctx, mcxDir, mcxCosts, strings.TrimSpace(name))
			if err != nil {
				return fmt.Errorf("%s: %w", strings.TrimSpace(name), err)
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
	// Each channel gets its own messages, oldest first.
	var channels []string
	byChannel := map[string][]fwdSignal{}
	for _, s := range fresh {
		c := route(s.Strategy)
		if _, ok := byChannel[c]; !ok {
			channels = append(channels, c)
		}
		byChannel[c] = append(byChannel[c], s)
	}
	buttons := newFutContracts(os.Getenv("SLACK_ORDER_USERS") != "")
	now := time.Now()
	markSent := func(batch []fwdSignal) error {
		for _, s := range batch {
			sent[s.id()] = true
		}
		// Saved after each message, so a failure part-way doesn't repost.
		return saveSent(sentPath, sent)
	}
	// Slack's message limit is about 40k characters; batch well under it. A
	// fresh signal with buttons goes in a message of its own.
	const perMessage = 40
	for _, c := range channels {
		var batch []fwdSignal
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			lines := make([]string, len(batch))
			for j, s := range batch {
				lines[j] = s.text()
			}
			if err := client.Post(ctx, c, strings.Join(lines, "\n")); err != nil {
				return err
			}
			err := markSent(batch)
			batch = nil
			return err
		}
		for _, s := range byChannel[c] {
			if row := buttons.row(ctx, s, now); row != nil {
				if err := flush(); err != nil {
					return err
				}
				if _, err := client.PostBlocks(ctx, c, s.text(), []slack.Block{slack.SectionBlock(s.text()), row}); err != nil {
					return err
				}
				if err := markSent([]fwdSignal{s}); err != nil {
					return err
				}
				continue
			}
			batch = append(batch, s)
			if len(batch) == perMessage {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if err := flush(); err != nil {
			return err
		}
	}
	fmt.Printf("posted %d signal(s)\n", len(fresh))
	return nil
}

// mcxForwardUnderlying is the commodity an MCX forward test trades, or "".
func mcxForwardUnderlying(strat string) string {
	switch {
	case strings.HasPrefix(strat, "goldm_"):
		return gemName
	case strings.HasPrefix(strat, "silverm_"):
		return smName
	}
	return ""
}

// kiteClient is a Kite client on the stored Zerodha login (serene auth).
func kiteClient(ctx context.Context) (*kite.Client, error) {
	tokens, err := openTokens(ctx)
	if err != nil {
		return nil, err
	}
	defer tokens.Close()
	token, err := auth.Load(ctx, tokens, "zerodha")
	if err != nil {
		return nil, fmt.Errorf("%w (log in with serene auth)", err)
	}
	apiKey := os.Getenv("ZERODHA_API_KEY")
	if apiKey == "" {
		return nil, errors.New("ZERODHA_API_KEY is not set")
	}
	return kite.New(apiKey, token.AccessToken), nil
}

// fetchMCX brings names' minute bars up to now from Kite.
func fetchMCX(ctx context.Context, dir string, names []string) error {
	kc, err := kiteClient(ctx)
	if err != nil {
		return err
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
	cfg := mcx.Config{Dir: dir, Names: names, Minute: true,
		Lookback: 500 * 24 * time.Hour, Log: func(string, ...any) {}}
	return mcx.Run(ctx, cfg, store, kc, lots)
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

// mcxSignals replays a frozen MCX strategy (goldm_ema_15m, goldm_hma_1h,
// silverm_ema_15m, silverm_hma_1h or a hmasma_10m) from its forward start. The last
// trade's "end" exit is only where the data stops, so it is left out: that
// position is still open.
func mcxSignals(ctx context.Context, dir, costsFile, strat string) ([]fwdSignal, error) {
	ts, err := mcxTrades(ctx, dir, costsFile, strat)
	if err != nil {
		return nil, err
	}
	var out []fwdSignal
	var from time.Time // the hmasma replays start before go-live; nothing before it is posted
	if hsStrategies[strat] != "" {
		from = hsForward.At(0, 0)
	}
	for _, t := range ts {
		name := t.Key
		if in, err := market.ParseKey(t.Key); err == nil {
			name = fmt.Sprintf("%s FUT (exp %s)", in.Underlying, in.Expiry)
		}
		if !t.Entry.Before(from) {
			out = append(out, fwdSignal{Strategy: strat, Time: t.Entry, Side: t.Side, Instrument: name, Price: t.In, Note: "enter"})
		}
		if t.Reason != "end" && !t.Exit.Before(from) {
			out = append(out, fwdSignal{Strategy: strat, Time: t.Exit, Side: -t.Side, Instrument: name, Price: t.Out, Note: "exit, " + t.Reason})
		}
	}
	return out, nil
}

// mcxTrades replays a frozen MCX forward test from its start to the last
// fetched bar. The last trade, if it ends "end", is the position held now.
func mcxTrades(ctx context.Context, dir, costsFile, strat string) ([]mcx.Trade, error) {
	name, start := gemName, gemForward
	hs := hsStrategies[strat] != ""
	if hs {
		name, start = hsStrategies[strat], hsForward.AddDays(-hsLead)
	}
	if strat == "goldm_hma_1h" {
		start = ghmForward
	}
	if !hs && mcxForwardUnderlying(strat) == smName {
		name, start = smName, smForward
	}
	spec, ok := mcx.Specs[name]
	if !ok {
		return nil, fmt.Errorf("no contract spec for %s", name)
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
	days, err := store.Days(name, start, start.AddDays(3650))
	if err != nil {
		return nil, err
	}
	if len(days) == 0 {
		return nil, nil
	}
	to := days[len(days)-1]
	bars, err := loadMCXBars(ctx, store, []string{name}, start.AddDays(-30), to)
	if err != nil {
		return nil, err
	}
	frame, cfg := gemFrame, mcx.EMAConfig{Name: gemName, Spec: spec, Frame: gemFrame, Fast: gemFast, Slow: gemSlow,
		Slip: 1, Costs: costs, From: gemForward, To: to}
	switch {
	case hs:
		// From well before go-live, so the side held at go-live comes from a
		// real cross rather than an entry at the first bar replayed.
		frame, cfg = hsFrame, hsConfig(name, spec, costs, 1, start, to)
	case strat == "goldm_hma_1h":
		frame, cfg = ghmFrame, ghmConfig(spec, costs, 1, ghmForward, to)
	case name == smName:
		if frame, cfg, err = smConfig(strat, spec, costs, 1, smForward, to); err != nil {
			return nil, err
		}
	}
	return mcx.EMABacktest(mcx.PrepareEMA(bars[name], frame), cfg)
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
