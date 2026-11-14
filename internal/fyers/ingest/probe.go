package ingest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"serene/internal/data"
	"serene/internal/fyers"
	"serene/internal/market"
)

// Probe checks what Fyers actually serves, since the docs and Fyers' own
// forum disagree on how far back expired contracts go. It records each
// finding in probe.parquet and returns them.
func Probe(ctx context.Context, cfg Config, store *data.Store, c *fyers.Client, us []fyers.Underlying) ([]data.ProbeRow, error) {
	var rows []data.ProbeRow
	add := func(check, symbol, value, note string) {
		rows = append(rows, data.ProbeRow{Check: check, Symbol: symbol, Value: value, Note: note, At: cfg.now()})
		cfg.Log("%-22s %-26s %s %s", check, symbol, value, note)
	}
	today := cfg.today()
	for _, u := range us {
		// Earliest minute bar: step forward from the documented floor.
		floor := market.Date{Year: 2017, Month: time.July, Day: 3}
		if d, err := firstData(ctx, c, u.Symbol, fyers.Minute, floor, today, fyers.MaxIntradayDays, 40); err != nil {
			return rows, err
		} else {
			add("earliest_minute", u.Symbol, dateOrNone(d), "documented floor 2017-07-03")
		}
		if d, err := firstData(ctx, c, u.Symbol, fyers.Day, market.Date{Year: 1990, Month: time.January, Day: 1}, today, fyers.MaxDailyDays, 40); err != nil {
			return rows, err
		} else {
			add("earliest_daily", u.Symbol, dateOrNone(d), "")
		}

		// Expiries: the first one Fyers lists, and the most recent.
		ex, err := c.ExpiryDates(ctx, u.Symbol, u.Earliest(), today.AddDays(-1))
		if err != nil {
			add("expiry_dates", u.Symbol, "error", err.Error())
			if errors.Is(err, fyers.ErrAuth) || errors.Is(err, fyers.ErrRateLimited) {
				return rows, err
			}
			continue
		}
		opts := ex.Options
		sort.Slice(opts, func(i, j int) bool { return opts[i].Before(opts[j]) })
		add("option_expiries", u.Symbol, strconv.Itoa(len(opts)), "")
		if len(opts) == 0 {
			continue
		}
		add("earliest_expiry", u.Symbol, opts[0].String(), "documented floor "+u.Earliest().String())
		if err := store.PutExpiries(ctx, u.Name, data.ExpiryOption, opts); err != nil {
			return rows, err
		}

		// One contract from the oldest and newest expiries: bars and OI.
		for _, e := range []market.Date{opts[0], opts[len(opts)-1]} {
			if err := probeExpiry(ctx, c, u, e, add); err != nil {
				return rows, err
			}
		}
	}
	return rows, store.PutProbe(ctx, rows)
}

func probeExpiry(ctx context.Context, c *fyers.Client, u fyers.Underlying, e market.Date, add func(check, symbol, value, note string)) error {
	cs, err := c.ExpiredContracts(ctx, u.Symbol, e)
	if err != nil {
		add("expired_contracts", u.Symbol+" "+e.String(), "error", err.Error())
		return fatal(err)
	}
	add("expired_contracts", u.Symbol+" "+e.String(), strconv.Itoa(len(cs.Options)), "options listed")
	// A middle strike is likely near the money and traded.
	var calls []market.Instrument
	tickers := map[string]string{}
	unparsed := 0
	for _, t := range cs.Options {
		inst, err := fyers.ParseContract(t, u, e)
		if err != nil {
			unparsed++
			continue
		}
		if inst.Right == market.Call {
			calls = append(calls, inst)
			tickers[inst.Key()] = t
		}
	}
	if unparsed > 0 {
		add("unparsed_tickers", u.Symbol+" "+e.String(), strconv.Itoa(unparsed), "symbol.go needs a new format")
	}
	if len(calls) == 0 {
		return nil
	}
	sort.Slice(calls, func(i, j int) bool { return calls[i].Strike < calls[j].Strike })
	ticker := tickers[calls[len(calls)/2].Key()]
	candles, err := c.ExpiredHistory(ctx, ticker, fyers.Minute, e.AddDays(-20), e)
	if err != nil {
		add("expired_minute", ticker, "error", err.Error())
		return fatal(err)
	}
	withOI := 0
	for _, cd := range candles {
		if cd.OI > 0 {
			withOI++
		}
	}
	note := "no bars"
	if len(candles) > 0 {
		note = fmt.Sprintf("%s to %s; %d with OI", candles[0].Time.Format("2006-01-02 15:04"),
			candles[len(candles)-1].Time.Format("2006-01-02 15:04"), withOI)
	}
	add("expired_minute", ticker, strconv.Itoa(len(candles)), note)
	return nil
}

// fatal passes on errors that should stop the probe.
func fatal(err error) error {
	if errors.Is(err, fyers.ErrAuth) || errors.Is(err, fyers.ErrRateLimited) || errors.Is(err, fyers.ErrBudget) {
		return err
	}
	return nil
}

// firstData returns the date of the first candle found stepping forward from
// `from` in windows of n days, trying at most tries windows.
func firstData(ctx context.Context, c *fyers.Client, symbol string, res fyers.Resolution, from, to market.Date, n, tries int) (market.Date, error) {
	for i, w := range windows(from, to, n) {
		if i >= tries {
			break
		}
		cs, err := c.History(ctx, symbol, res, w[0], w[1], false)
		if err != nil {
			return market.Date{}, err
		}
		if len(cs) > 0 {
			return market.DateOf(cs[0].Time), nil
		}
	}
	return market.Date{}, nil
}

func dateOrNone(d market.Date) string {
	if d.IsZero() {
		return "none"
	}
	return d.String()
}
