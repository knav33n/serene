package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"serene/internal/adjust"
	"serene/internal/auth"
	"serene/internal/bhav"
	"serene/internal/data"
	"serene/internal/market"
	"serene/internal/upstox"
)

// equitiesDownload fetches the bhavcopy for a range and returns the years to
// rebuild: those with new or changed files, and any a stopped run left unbuilt.
func equitiesDownload(ctx context.Context, dir string, from, to market.Date) ([]int, error) {
	res, err := bhav.Download(ctx, bhav.Config{Dir: dir, From: from, To: to, Log: marketLog}, bhav.New())
	if err != nil {
		return nil, err
	}
	unbuilt, err := bhav.Unbuilt(dir)
	if err != nil {
		return nil, err
	}
	years := bhav.Years(append(res.New, res.Changed...))
	return slices.Compact(slices.Sorted(slices.Values(append(years, unbuilt...)))), nil
}

// symbolError is an Upstox answer about one instrument, such as an ISIN it
// doesn't list, which shouldn't stop a run over thousands.
var symbolError = regexp.MustCompile(`^HTTP 4(00|04) `)

func upstoxCandles(up *upstox.Client) adjust.Candles {
	return func(ctx context.Context, isin string, from, to market.Date) ([]adjust.Day, error) {
		c, err := up.DailyCandles(ctx, "NSE_EQ|"+isin, from, to)
		if err != nil {
			return nil, err
		}
		out := make([]adjust.Day, len(c))
		for i, k := range c {
			out[i] = adjust.Day{Date: market.DateOf(k.Time), Open: k.Open, High: k.High, Low: k.Low, Close: k.Close}
		}
		return out, nil
	}
}

func fatalUpstox(err error) bool { return !symbolError.MatchString(err.Error()) }

func upstoxClient(ctx context.Context) (*upstox.Client, error) {
	tokens, err := openTokens(ctx)
	if err != nil {
		return nil, err
	}
	defer tokens.Close()
	if url := os.Getenv("CALM_DATABASE_URL"); url != "" {
		// On hearth the Upstox login is made in project_calm.
		if _, err := auth.ImportCalm(ctx, url, tokens); err != nil {
			fmt.Fprintln(os.Stderr, "project_calm tokens not copied:", err)
		}
	}
	token, err := auth.Load(ctx, tokens, "upstox")
	if err != nil {
		return nil, err
	}
	return upstox.New(token.AccessToken), nil
}

// adjustTodo picks the listed symbols to check: the named ones, or else the
// pending ones and, unless recheck is false and they're done, all the rest.
func adjustTodo(listed []data.Listed, st *adjust.State, named []string, all, recheck bool) []data.Listed {
	var out []data.Listed
	for _, l := range listed {
		_, done := st.Securities[l.Symbol]
		switch {
		case len(named) > 0:
			if slices.Contains(named, l.Symbol) {
				out = append(out, l)
			}
		case slices.Contains(st.Pending, l.Symbol), all && (recheck || !done):
			out = append(out, l)
		}
	}
	return out
}

func runEquitiesAdjust(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("equities adjust", flag.ContinueOnError)
	symbols := fs.String("symbols", "", "comma-separated symbols, default every listed one not yet checked")
	recheck := fs.Bool("recheck", false, "check symbols already checked again")
	dir := fs.String("data", "data/equities", "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := data.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()
	st, err := adjust.Load(*dir)
	if err != nil {
		return err
	}
	listed, err := store.EquityListed(ctx)
	if err != nil {
		return err
	}
	var named []string
	if *symbols != "" {
		named = strings.Split(*symbols, ",")
	}
	todo := adjustTodo(listed, st, named, true, *recheck)
	up, err := upstoxClient(ctx)
	if err != nil {
		return err
	}
	marketLog("checking %d symbols against Upstox", len(todo))
	if err := adjust.Check(ctx, *dir, st, store, upstoxCandles(up), fatalUpstox, todo, marketLog); err != nil {
		return err
	}
	return writeAdjustments(ctx, st, store)
}

func writeAdjustments(ctx context.Context, st *adjust.State, store *data.Store) error {
	n, err := adjust.Write(ctx, st, store)
	if err != nil {
		return err
	}
	failed := 0
	for _, s := range st.Securities {
		if s.Error != "" {
			failed++
		}
	}
	marketLog("%d symbols checked, %d events stored, %d not on Upstox, %d pending", len(st.Securities), n, failed,
		len(st.Pending))
	return nil
}

// runEquitiesDaily is the evening job: fetch the day's bhavcopy, mark the
// symbols whose prices gapped, and confirm them against Upstox when a token is
// at hand. Without one the symbols wait for the next run that has one.
func runEquitiesDaily(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("equities daily", flag.ContinueOnError)
	dir := fs.String("data", "data/equities", "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := data.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()
	years, err := equitiesDownload(ctx, *dir, bhav.FirstDay, market.DateOf(time.Now()))
	if err != nil {
		return err
	}
	if err := bhav.Rebuild(ctx, *dir, store, years); err != nil {
		return err
	}
	dates, err := bhav.RawDates(*dir)
	if err != nil || len(dates) == 0 {
		return err
	}
	latest := dates[len(dates)-1]

	st, err := adjust.Load(*dir)
	if err != nil {
		return err
	}
	if st.Scanned == nil {
		if len(st.Securities) == 0 {
			marketLog("no adjustment check has run yet: run serene equities adjust")
			return nil
		}
		// The first check covered everything up to its run.
		st.Scanned = &latest
	}
	if st.Scanned.Before(latest) {
		gaps, err := store.EquityGaps(ctx, st.Scanned.AddDays(1))
		if err != nil {
			return err
		}
		marketLog("%d symbols gapped since %s: %s", len(gaps), st.Scanned, listOrNone(gaps))
		st.AddPending(gaps...)
		st.Scanned = &latest
		if err := st.Save(*dir); err != nil {
			return err
		}
	}
	if len(st.Pending) > 0 {
		up, err := upstoxClient(ctx)
		switch {
		case err != nil:
			marketLog("%d symbols wait for an Upstox check (%v)", len(st.Pending), err)
		default:
			listed, err := store.EquityListed(ctx)
			if err != nil {
				return err
			}
			todo := adjustTodo(listed, st, nil, false, false)
			// Pending symbols no longer listed have nothing to check.
			st.Pending = nil
			for _, l := range todo {
				st.AddPending(l.Symbol)
			}
			err = adjust.Check(ctx, *dir, st, store, upstoxCandles(up), fatalUpstox, todo, marketLog)
			if errors.Is(err, upstox.ErrAuth) {
				marketLog("%d symbols wait for an Upstox check (%v)", len(st.Pending), err)
			} else if err != nil {
				return err
			}
		}
	}
	fmt.Printf("bhavcopy to %s, %d years rebuilt\n", latest, len(years))
	return writeAdjustments(ctx, st, store)
}
