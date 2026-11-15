package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"serene/internal/etf"
	"serene/internal/kite"
	"serene/internal/market"
	"serene/internal/upstox"
)

// runETFFetch brings every NSE-traded ETF's daily and 1-minute files up to date.
func runETFFetch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("etf fetch", flag.ContinueOnError)
	dir := fs.String("dir", "F:/INSIGHTS/etf", "output directory")
	data := fs.String("data", "data/equities", "equities data directory (the bhavcopy store)")
	symbols := fs.String("symbols", "", "comma-separated current symbols, default all")
	active := fs.Int("active-days", 45, "list ETFs traded in this many days before the newest bhavcopy day")
	if err := fs.Parse(args); err != nil {
		return err
	}
	f, err := etf.Open(*dir, filepath.Join(*data, "stocks", "year=*.parquet"))
	if err != nil {
		return err
	}
	defer f.Close()
	logFile, err := os.OpenFile(filepath.Join(*dir, "fetch.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	f.Log = io.MultiWriter(os.Stdout, logFile)
	logf := func(format string, a ...any) {
		fmt.Fprintf(f.Log, time.Now().In(market.IST).Format("15:04:05 ")+format+"\n", a...)
	}

	newest, err := f.NewestBhavDay()
	if err != nil {
		return err
	}
	etfs, err := f.Universe(newest.AddDate(0, 0, -*active))
	if err != nil {
		return err
	}
	if *symbols == "" {
		if err := f.WriteUniverse(etfs); err != nil {
			return err
		}
	} else {
		want := strings.Split(strings.ToUpper(*symbols), ",")
		etfs = slices.DeleteFunc(etfs, func(e etf.ETF) bool { return !slices.Contains(want, e.Symbol) })
	}
	logf("universe: %d ETFs", len(etfs))

	if f.Kite, err = kiteClient(ctx); err != nil {
		logf("Kite unavailable (%v): Upstox only", err)
		f.Kite = nil
	} else if err := f.LoadTokens(ctx); err != nil {
		return err
	}
	if f.Upstox, err = upstoxClient(ctx); err != nil {
		logf("Upstox unavailable (%v): Kite only", err)
		f.Upstox = nil
	}
	if f.Kite == nil && f.Upstox == nil {
		return errors.New("log in to Kite or Upstox first: serene auth")
	}

	failed := 0
	for i, e := range etfs {
		r, err := f.Fetch(ctx, e)
		switch {
		case errors.Is(err, kite.ErrAuth):
			return fmt.Errorf("%w; finished ETFs are kept, rerun to carry on", err)
		case errors.Is(err, upstox.ErrAuth):
			logf("Upstox login rejected: carrying on with Kite only")
			f.Upstox = nil
			r, err = f.Fetch(ctx, e)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			logf("[%d/%d] %s: %v", i+1, len(etfs), e.Symbol, err)
			failed++
			continue
		}
		src := "kite"
		if !r.KiteListed {
			src = "upstox"
		}
		logf("[%d/%d] %s/%s (%s): daily %d (+%d, %d from bhav), 1m %d (+%d)", i+1, len(etfs), e.Country, e.Symbol, src,
			r.Daily, r.NewDaily, r.FromBhav, r.Minute, r.NewMinute)
	}
	if err := f.Coverage(); err != nil {
		return err
	}
	logf("done: %d ETFs, %d failed; coverage.csv written", len(etfs)-failed, failed)
	if failed > 0 {
		return fmt.Errorf("%d ETFs failed; rerun to retry them", failed)
	}
	return nil
}
