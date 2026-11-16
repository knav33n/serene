package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"serene/internal/auth"
	"serene/internal/fobhav"
	"serene/internal/fyers"
	fyingest "serene/internal/fyers/ingest"
	"serene/internal/market"
)

// bhavFrom is where the F&O bhavcopies are read from: a month before Fyers'
// first expiry, so October 2018's contracts have prices and lots.
var bhavFrom = market.Date{Year: 2018, Month: time.September, Day: 1}

// runFyersLots downloads the NSE and BSE F&O bhavcopies and NSE's market
// activity reports, then derives each underlying's lots, strikes and prices,
// and the universe of stocks that ever had options.
func runFyersLots(ctx context.Context, s *fyersSession) error {
	root := s.cfg.Dir
	today := market.DateOf(time.Now())
	if err := fobhav.DownloadNSE(ctx, root, bhavFrom, today, stamp); err != nil {
		return fmt.Errorf("nse bhavcopy: %w", err)
	}
	if err := fobhav.DownloadBSE(ctx, root, today, stamp); err != nil {
		return err
	}
	if err := fobhav.DownloadMkt(ctx, root, stamp); err != nil {
		return err
	}
	sum, err := fobhav.Build(root, bhavFrom, stamp)
	if err != nil {
		return err
	}
	fmt.Printf("%d files read: %d underlyings, %d of them stocks (universe.csv)\n", sum.Files, sum.Symbols, sum.Stocks)
	var srcs []string
	for k, n := range sum.BySource {
		srcs = append(srcs, fmt.Sprintf("%s %d", k, n))
	}
	sort.Strings(srcs)
	fmt.Printf("option expiry lots by source: %s; without a lot: %d\n", strings.Join(srcs, ", "), sum.NoLot)
	fmt.Printf("market activity vs UDiFF: %d expiries compared, %d differ\n", sum.Checked, sum.Mismatched)
	for i, m := range sum.Mismatches {
		if i == 20 {
			fmt.Printf("  ... %d more\n", len(sum.Mismatches)-20)
			break
		}
		fmt.Println("  " + m)
	}
	fmt.Printf("turnover estimate vs market activity: %d compared, %d differ\n", sum.Estimated, sum.EstimateOff)
	return nil
}

// runFyersAll downloads every underlying's history: indices first, then
// stocks in name order, each one's spot and then its expired contracts,
// newest expiry first. It runs for days: when the daily budget is spent it
// sleeps to the next day, and when the token expires (06:30 IST) it asks on
// Slack for a login and carries on once serene auth has stored a new one.
// Every step resumes, so stopping and starting again loses nothing.
func runFyersAll(ctx context.Context, s *fyersSession, us []fyers.Underlying) error {
	sort.SliceStable(us, func(i, j int) bool {
		if us[i].IsIndex() != us[j].IsIndex() {
			return us[i].IsIndex()
		}
		return !us[i].IsIndex() && us[i].Name < us[j].Name
	})
	var failed []string
	for i, u := range us {
		stamp("=== [%d/%d] %s", i+1, len(us), u.Name)
		failures := 0
		for {
			err := fyersOne(ctx, s, u)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var wait time.Duration
			switch {
			case errors.Is(err, fyers.ErrBudget):
				stamp("%s: daily budget used up; sleeping to tomorrow", u.Name)
				wait = untilTomorrow()
			case errors.Is(err, fyers.ErrAuth):
				if err := waitForToken(ctx, s); err != nil {
					return err
				}
				continue
			case errors.Is(err, fyers.ErrRateLimited):
				// Retrying into a limit risks a block for the day, and Fyers
				// caps expired intraday data below its published limits, so
				// wait for the daily reset.
				stamp("%s: %v; sleeping to tomorrow", u.Name, err)
				wait = untilTomorrow()
			default:
				if failures++; failures > 2 {
					stamp("%s: giving up for this run: %v", u.Name, err)
					failed = append(failed, fmt.Sprintf("%s: %v", u.Name, err))
				} else {
					stamp("%s: %v; retrying in 2 minutes", u.Name, err)
					wait = 2 * time.Minute
				}
			}
			if failures > 2 {
				break
			}
			if err := sleepCtx(ctx, wait); err != nil {
				return err
			}
		}
	}
	stamp("finished %d underlyings, %d failed", len(us), len(failed))
	for _, f := range failed {
		fmt.Println("  " + f)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d underlyings failed; run serene fyers all again to retry them", len(failed))
	}
	return nil
}

func fyersOne(ctx context.Context, s *fyersSession, u fyers.Underlying) error {
	if _, err := fyingest.Index(ctx, s.ingest, s.store, s.client, u, fyingest.IndexFrom); err != nil {
		return fmt.Errorf("index: %w", err)
	}
	return fyingest.Backfill(ctx, s.ingest, s.store, s.client, u, market.Date{}, market.DateOf(time.Now()))
}

// untilTomorrow is the wait to 00:05 IST, when Fyers' daily count resets.
func untilTomorrow() time.Duration {
	now := time.Now().In(market.IST)
	next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 5, 0, 0, market.IST)
	return time.Until(next)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// waitForToken asks for a Fyers login on Slack, then checks the token store
// every five minutes until it holds a different token.
func waitForToken(ctx context.Context, s *fyersSession) error {
	old := s.client.Token.Reveal()
	msg := "Fyers token expired: the full history download is paused. Log in with `serene auth` to resume."
	stamp("%s", msg)
	if dm, err := openSlack(ctx); err == nil {
		if err := dm.client.Post(ctx, dm.channel, msg); err != nil {
			stamp("slack: %v", err)
		}
	} else {
		stamp("slack: %v", err)
	}
	for {
		if err := sleepCtx(ctx, 5*time.Minute); err != nil {
			return err
		}
		tokens, err := openTokens(ctx)
		if err != nil {
			stamp("token store: %v", err)
			continue
		}
		t, err := auth.Load(ctx, tokens, "fyers")
		tokens.Close()
		if err != nil {
			continue
		}
		if t.AccessToken.Reveal() != old {
			s.client.Token = t.AccessToken
			stamp("new Fyers token found; resuming")
			return nil
		}
	}
}
