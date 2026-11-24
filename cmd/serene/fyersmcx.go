package main

import (
	"context"
	"errors"
	"flag"
	"math"
	"path/filepath"
	"strings"
	"time"

	"serene/internal/data"
	"serene/internal/fyers"
	"serene/internal/market"
)

// fyersMCXStart is the first month Fyers holds expired MCX futures for: its
// minute history of contracts then trading starts in early November 2018.
var fyersMCXStart = market.Date{Year: 2018, Month: time.November, Day: 1}

// fyersMCXManifest is one contract month's download state.
type fyersMCXManifest struct {
	Ticker  string      `json:"ticker"`
	Missing bool        `json:"missing,omitempty"` // Fyers has no such contract
	Key     string      `json:"key,omitempty"`
	Expiry  market.Date `json:"expiry,omitzero"` // the last day it traded
	First   market.Date `json:"first,omitzero"`
	Bars    int         `json:"bars,omitempty"`
}

// runFyersMCX downloads expired MCX futures' minute bars from Fyers into
// <fyers dir>/mcx, in the same layout and keys as data/mcx (Kite), so the MCX
// backtests read it with --data. Fyers can't list MCX expiries, so each
// month's ticker (MCX:GOLDM20MARFUT) is tried; a contract's expiry is the last
// day it has bars. A contract is fetched back from its expiry month in
// 100-day windows until a window is empty. Finished months are skipped on a
// rerun; months not expired yet are left for Kite.
func runFyersMCX(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fyers mcx", flag.ContinueOnError)
	configPath := fs.String("config", "configs/fyers.yaml", "Fyers download config")
	names := fs.String("names", "GOLDM,SILVERM,GOLD,SILVER,COPPER,ZINC,ALUMINIUM,ZINCMINI,ALUMINI,SILVERMIC", "MCX futures, in order")
	fromS := fs.String("from", fyersMCXStart.String(), "first expiry month")
	if err := fs.Parse(args); err != nil {
		return err
	}
	from, err := market.ParseDate(*fromS)
	if err != nil {
		return err
	}
	s, err := openFyers(ctx, *configPath, true)
	if err != nil {
		return err
	}
	s.store.Close() // the NSE store; MCX has its own
	dir := filepath.Join(s.cfg.Dir, "mcx")
	store, err := data.Open(dir)
	if err != nil {
		return err
	}
	defer store.Close()
	today := market.DateOf(time.Now().In(market.IST))
	for _, name := range strings.Split(*names, ",") {
		name = strings.TrimSpace(name)
		var got, missing, bars int
		for m := (market.Date{Year: from.Year, Month: from.Month, Day: 1}); !today.Before(m); m = addMonth(m) {
			man, err := fyersMCXMonth(ctx, s.client, store, dir, name, m, today)
			if errors.Is(err, fyers.ErrRateLimited) || errors.Is(err, fyers.ErrBudget) || errors.Is(err, fyers.ErrAuth) {
				return err
			}
			if err != nil {
				stamp("%s %s: %v", name, m.String()[:7], err)
				continue
			}
			if man.Missing {
				missing++
			} else if man.Bars > 0 {
				got++
				bars += man.Bars
			}
		}
		stamp("%s done: %d contracts, %d bars, %d months with no contract", name, got, bars, missing)
	}
	return nil
}

func fyersMCXMonth(ctx context.Context, c *fyers.Client, store *data.Store, dir, name string, month, today market.Date) (fyersMCXManifest, error) {
	t := time.Date(month.Year, month.Month, 1, 0, 0, 0, 0, time.UTC)
	ticker := "MCX:" + name + strings.ToUpper(t.Format("06Jan")) + "FUT"
	path := filepath.Join(dir, "fetch", name, month.String()[:7]+".json")
	var man fyersMCXManifest
	if ok, err := readJSONFile(path, &man); err != nil || ok {
		return man, err
	}
	man.Ticker = ticker
	end := addMonth(month).AddDays(-1)
	if !end.Before(today) { // Fyers refuses a range that includes today
		end = today.AddDays(-1)
	}
	current := !end.Before(today.AddDays(-1)) // may not have expired yet
	var all []market.Bar
	for to := end; !to.Before(fyersMCXStart); {
		start := to.AddDays(-(fyers.MaxIntradayDays - 1))
		cs, err := c.ExpiredHistory(ctx, ticker, fyers.Minute, start, to)
		if errors.Is(err, fyers.ErrNoData) {
			cs, err = nil, nil
		}
		if err != nil {
			return man, err
		}
		if len(cs) == 0 {
			break
		}
		for _, cd := range cs {
			all = append(all, market.Bar{Timeframe: market.Minute1, Time: cd.Time, End: cd.Time.Add(time.Minute),
				Open: rupeesToPaise(cd.Open), High: rupeesToPaise(cd.High), Low: rupeesToPaise(cd.Low), Close: rupeesToPaise(cd.Close),
				Volume: cd.Volume, OI: cd.OI})
		}
		to = start.AddDays(-1)
	}
	if len(all) == 0 {
		if current {
			return man, nil // not listed or not expired yet: try again next run
		}
		man.Missing = true
		return man, writeJSONFile(path, man)
	}
	first, last := all[0].Time, all[0].Time
	for _, b := range all {
		if b.Time.Before(first) {
			first = b.Time
		}
		if b.Time.After(last) {
			last = b.Time
		}
	}
	man.Expiry, man.First, man.Bars = market.DateOf(last), market.DateOf(first), len(all)
	if current && !man.Expiry.Before(today.AddDays(-1)) {
		return man, nil // still trading: Kite has it; Fyers' expired API will once it's done
	}
	man.Key = market.Instrument{Exchange: market.MCX, Kind: market.KindFuture, Underlying: name, Expiry: man.Expiry}.Key()
	for i := range all {
		all[i].Key = man.Key
	}
	if err := store.PutBars(ctx, name, all); err != nil {
		return man, err
	}
	if err := writeJSONFile(path, man); err != nil {
		return man, err
	}
	stamp("%s: %s..%s, %d bars", ticker, man.First, man.Expiry, man.Bars)
	return man, nil
}

func rupeesToPaise(v float64) market.Paise { return market.Paise(math.Round(v * 100)) }


// addMonth is the first of the month after d's.
func addMonth(d market.Date) market.Date {
	return market.DateOf(time.Date(d.Year, d.Month+1, 1, 12, 0, 0, 0, market.IST))
}
