package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"serene/internal/auth"
	"serene/internal/data"
	"serene/internal/kite"
	"serene/internal/market"
	"serene/internal/mcx"
)

func runMCXFetch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcx fetch", flag.ContinueOnError)
	names := fs.String("names", "", "commodities, such as CRUDEOIL,GOLD; all when empty")
	daily := fs.Bool("daily", true, "fetch each commodity's continuous daily series")
	minute := fs.Bool("minute", true, "fetch 1-minute bars of the futures still trading")
	lookback := fs.Int("lookback-days", 500, "how far back to look for a new contract's first trade")
	dir := fs.String("data", "data/mcx", "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tokens, err := openTokens(ctx)
	if err != nil {
		return err
	}
	defer tokens.Close()
	token, err := auth.Load(ctx, tokens, "zerodha")
	if err != nil {
		return err
	}
	apiKey := os.Getenv("ZERODHA_API_KEY")
	if apiKey == "" {
		return errors.New("ZERODHA_API_KEY is not set")
	}
	lots, err := mcx.UpstoxLots(ctx)
	if err != nil {
		return err
	}
	store, err := data.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()
	cfg := mcx.Config{Dir: *dir, Daily: *daily, Minute: *minute,
		Lookback: time.Duration(*lookback) * 24 * time.Hour, Log: marketLog}
	if *names != "" {
		cfg.Names = strings.Split(strings.ToUpper(*names), ",")
	}
	return mcx.Run(ctx, cfg, store, kite.New(apiKey, token.AccessToken), lots)
}

// pulseHour returns the start of the last whole hour of trading at now. MCX
// closes at 23:55 at the latest (23:30 in the US summer), so from 23:55 the
// 23:00 hour counts as done.
func pulseHour(now time.Time) time.Time {
	t := now.In(market.IST)
	h := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, market.IST)
	if t.Hour() == 23 && t.Minute() >= 55 {
		return h
	}
	return h.Add(-time.Hour)
}

type pulseState struct {
	Posted time.Time `json:"posted"` // start of the last hour sent to Slack
}

func runMCXHourly(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcx hourly", flag.ContinueOnError)
	useSlack := fs.Bool("slack", false, "post to Slack, once per hour")
	names := fs.String("names", strings.Join(mcx.PulseNames, ","), "commodities")
	at := fs.String("hour", "", "report this hour instead, such as 2026-11-10T21")
	state := fs.String("state", "data/mcx/pulse.json", "records the last hour posted")
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
	err := mcxHourly(ctx, *names, *at, *state, dm)
	if err != nil && dm != nil {
		// A broken job must not look like a quiet hour.
		text := fmt.Sprintf(":x: *serene mcx hourly failed:* %v", err)
		if perr := dm.client.Post(context.WithoutCancel(ctx), dm.channel, text); perr != nil {
			fmt.Fprintln(os.Stderr, "serene: slack:", perr)
		}
	}
	return err
}

func mcxHourly(ctx context.Context, names, at, statePath string, dm *slackDM) error {
	hour := pulseHour(time.Now())
	if at != "" {
		t, err := time.ParseInLocation("2006-01-02T15", at, market.IST)
		if err != nil {
			return fmt.Errorf("--hour: want YYYY-MM-DDTHH: %w", err)
		}
		hour = t
	}
	var st pulseState
	if dm != nil && at == "" {
		if b, err := os.ReadFile(statePath); err == nil {
			if err := json.Unmarshal(b, &st); err != nil {
				return fmt.Errorf("%s: %w", statePath, err)
			}
		}
		if !hour.After(st.Posted) {
			marketLog("%s already posted", hour.Format("15:04"))
			return nil
		}
	}
	tokens, err := openTokens(ctx)
	if err != nil {
		return err
	}
	defer tokens.Close()
	if url := os.Getenv("CALM_DATABASE_URL"); url != "" {
		updated, err := auth.ImportCalm(ctx, url, tokens)
		if err != nil {
			// The stored token may still work; a real failure shows below.
			marketLog("project_calm tokens not copied: %v", err)
		} else if len(updated) > 0 {
			marketLog("copied tokens from project_calm: %s", strings.Join(updated, ", "))
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
	series, err := mcx.FetchRecent(ctx, kite.New(apiKey, token.AccessToken),
		strings.Split(strings.ToUpper(names), ","), hour.Add(time.Hour))
	if err != nil {
		return err
	}
	var pulses []mcx.Pulse
	for _, s := range series {
		if p, ok := mcx.Analyze(s, hour); ok {
			pulses = append(pulses, p)
		}
	}
	if len(pulses) == 0 {
		marketLog("no MCX trades %s–%s: holiday or closed", hour.Format("15:04"), hour.Add(time.Hour).Format("15:04"))
		return nil
	}
	text := mcx.Format(pulses, hour)
	fmt.Println(text)
	if dm == nil {
		return nil
	}
	if err := dm.client.Post(ctx, dm.channel, text); err != nil {
		return err
	}
	if at != "" {
		return nil
	}
	st.Posted = hour
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(statePath, b, 0o644)
}
