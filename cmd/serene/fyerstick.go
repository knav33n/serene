package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"serene/internal/fyers"
	"serene/internal/market"
)

// runFyersTick asks Fyers for expired tickers' daily bars, one request per
// ticker, and prints what came back. A probe for ticker spellings.
func runFyersTick(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fyers tick", flag.ContinueOnError)
	configPath := fs.String("config", "configs/fyers.yaml", "Fyers download config")
	fromS := fs.String("from", "", "first day")
	toS := fs.String("to", "", "last day")
	gap := fs.Duration("gap", 2*time.Second, "pause between requests")
	minute := fs.Bool("minute", false, "1m bars instead of daily")
	if err := fs.Parse(args); err != nil {
		return err
	}
	from, err := market.ParseDate(*fromS)
	if err != nil {
		return err
	}
	to, err := market.ParseDate(*toS)
	if err != nil {
		return err
	}
	s, err := openFyers(ctx, *configPath, true)
	if err != nil {
		return err
	}
	s.store.Close()
	for i, t := range fs.Args() {
		if i > 0 {
			time.Sleep(*gap)
		}
		res := fyers.Day
		if *minute {
			res = fyers.Minute
		}
		cs, err := s.client.ExpiredHistory(ctx, t, res, from, to)
		if errors.Is(err, fyers.ErrNoData) {
			err = nil
		}
		if err != nil {
			fmt.Printf("%s\terror %v\n", t, err)
			continue
		}
		if len(cs) == 0 {
			fmt.Printf("%s\tno bars\n", t)
			continue
		}
		var vol int64
		for _, c := range cs {
			vol += c.Volume
		}
		f, l := cs[0], cs[len(cs)-1]
		fmt.Printf("%s\t%d days %s..%s\tfirst close %.2f last close %.2f vol %d\n", t, len(cs),
			f.Time.Format("2006-01-02"), l.Time.Format("2006-01-02"), f.Close, l.Close, vol)
	}
	return nil
}
