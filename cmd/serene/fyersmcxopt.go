package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"

	"serene/internal/data"
	"serene/internal/fyers"
	"serene/internal/market"
	"serene/internal/mcx"
)

// mcxOptSteps are the strikes fetched for each MCX option, in rupees. GOLDM
// lists 100s too, but only its 500s trade.
var mcxOptSteps = map[string]float64{
	"CRUDEOIL": 50, "NATURALGAS": 5, "GOLD": 500, "GOLDM": 500, "SILVER": 500, "SILVERM": 500,
}

// fyersMCXOptManifest is one option month's download state.
type fyersMCXOptManifest struct {
	Month   string      `json:"month"`
	Missing bool        `json:"missing,omitempty"` // no options expire that month
	Expiry  market.Date `json:"expiry,omitzero"`
	From    market.Date `json:"from,omitzero"` // the window fetched
	Tickers int         `json:"tickers,omitempty"`
	Found   int         `json:"found,omitempty"`
	Bars    int         `json:"bars,omitempty"`
	Broken  int         `json:"broken,omitempty"` // tickers Fyers failed on (-99)
}

// futDay is one day's range of the future an option month is priced off.
type futDay struct {
	day       market.Date
	high, low float64 // rupees
	close     float64
}

// runFyersMCXOpt downloads expired MCX options' minute bars from Fyers into
// <fyers dir>/mcx next to the futures (`serene fyers mcx` must have run for
// the name). Fyers can't list MCX contracts, so each month's strikes are
// guessed from the future: every step within --pad steps of the range the
// future traded while that month's options were the nearest expiry (from the
// day after the previous month's option expiry). Tickers read
// MCX:CRUDEOIL24JAN6000CE, the month being the option's own expiry month. A
// month is first probed at the money; if nothing comes back, no options
// expire that month (GOLD and SILVER skip months).
func runFyersMCXOpt(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fyers mcxopt", flag.ContinueOnError)
	configPath := fs.String("config", "configs/fyers.yaml", "Fyers download config")
	names := fs.String("names", "CRUDEOIL,NATURALGAS,GOLD,GOLDM,SILVER,SILVERM", "MCX option underlyings, in order")
	fromS := fs.String("from", "2021-12-01", "first expiry month")
	toS := fs.String("to", "", "last expiry month (default: last month)")
	pad := fs.Int("pad", 6, "strikes beyond the future's range, each side")
	workers := fs.Int("workers", 2, "parallel requests")
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
	s.store.Close()
	dir := filepath.Join(s.cfg.Dir, "mcx")
	store, err := data.Open(dir)
	if err != nil {
		return err
	}
	defer store.Close()
	today := market.DateOf(time.Now().In(market.IST))
	stop := today
	if *toS != "" {
		t, err := market.ParseDate(*toS)
		if err != nil {
			return err
		}
		stop = addMonth(t)
	}
	if today.Before(stop) {
		stop = today
	}
	for _, name := range strings.Split(*names, ",") {
		name = strings.TrimSpace(name)
		step, ok := mcxOptSteps[name]
		spec, ok2 := mcx.Specs[name]
		if !ok || !ok2 {
			return fmt.Errorf("%s: no strike step or spec", name)
		}
		futs, err := futureDays(dir, name)
		if err != nil {
			return fmt.Errorf("%s futures: %w", name, err)
		}
		if len(futs) == 0 {
			return fmt.Errorf("%s: no futures in %s; run serene fyers mcx first", name, dir)
		}
		var prevExp market.Date
		var got, missing, bars int
		for m := (market.Date{Year: from.Year, Month: from.Month, Day: 1}); addMonth(m).Before(stop) || (addMonth(m) == stop && stop.Before(today)); m = addMonth(m) {
			man, err := fyersMCXOptMonth(ctx, s.client, store, dir, name, step, spec.Multiplier, m, prevExp, futs, *pad, *workers)
			if errors.Is(err, fyers.ErrRateLimited) || errors.Is(err, fyers.ErrBudget) || errors.Is(err, fyers.ErrAuth) {
				return err
			}
			if err != nil {
				stamp("%s %s options: %v", name, m.String()[:7], err)
				continue
			}
			if man.Missing {
				missing++
				continue
			}
			got++
			bars += man.Bars
			prevExp = man.Expiry
		}
		stamp("%s options done: %d months, %d bars, %d months with none", name, got, bars, missing)
	}
	return nil
}

// futureDays reads each day's range of each stored future of name, keyed by
// the future's expiry.
func futureDays(dir, name string) (map[market.Date][]futDay, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	glob := filepath.Join(dir, "bars", "underlying="+name, "*.parquet")
	rows, err := db.Query(fmt.Sprintf(`select key, strftime(cast(ts + interval 330 minute as date), '%%Y-%%m-%%d') d,
		max(high), min(low), arg_max(close, ts)
		from read_parquet(%s, hive_partitioning = false) where key like '%%:FUT' group by key, d order by key, d`, quote(glob)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[market.Date][]futDay{}
	for rows.Next() {
		var key, ds string
		var h, l, c int64
		if err := rows.Scan(&key, &ds, &h, &l, &c); err != nil {
			return nil, err
		}
		in, err := market.ParseKey(key)
		if err != nil {
			return nil, err
		}
		d, err := market.ParseDate(ds)
		if err != nil {
			return nil, err
		}
		out[in.Expiry] = append(out[in.Expiry], futDay{day: d, high: market.Paise(h).Rupees(), low: market.Paise(l).Rupees(), close: market.Paise(c).Rupees()})
	}
	return out, rows.Err()
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func fyersMCXOptMonth(ctx context.Context, c *fyers.Client, store *data.Store, dir, name string, step float64, lot int64,
	month, prevExp market.Date, futs map[market.Date][]futDay, pad, workers int) (fyersMCXOptManifest, error) {
	path := filepath.Join(dir, "fetch", name+"-OPT", month.String()[:7]+".json")
	var man fyersMCXOptManifest
	if ok, err := readJSONFile(path, &man); err != nil || ok {
		return man, err
	}
	man.Month = month.String()[:7]
	end := addMonth(month).AddDays(-1)
	start := prevExp.AddDays(1)
	if prevExp.IsZero() || start.Before(month.AddDays(-62)) {
		start = month.AddDays(-31) // first month, or after a gap: assume a one-month run
	}
	if start.Before(end.AddDays(-(fyers.MaxIntradayDays - 1))) {
		start = end.AddDays(-(fyers.MaxIntradayDays - 1))
	}
	man.From = start

	// The option prices off the first future expiring on or after mid-month
	// (crude/natgas: the same month's; bullion: the next listed one).
	var fexp market.Date
	for e := range futs {
		if !e.Before(month.AddDays(14)) && (fexp.IsZero() || e.Before(fexp)) {
			fexp = e
		}
	}
	if fexp.IsZero() {
		return man, fmt.Errorf("no future expiring after %s", month.AddDays(14))
	}
	var lo, hi, last float64
	for _, fd := range futs[fexp] {
		if fd.day.Before(start) || end.Before(fd.day) {
			continue
		}
		if lo == 0 || fd.low < lo {
			lo = fd.low
		}
		hi = max(hi, fd.high)
		last = fd.close
	}
	if lo == 0 {
		return man, fmt.Errorf("future %s has no bars %s..%s", fexp, start, end)
	}
	t := time.Date(month.Year, month.Month, 1, 0, 0, 0, 0, time.UTC)
	prefix := "MCX:" + name + strings.ToUpper(t.Format("06Jan"))
	ticker := func(k float64, r market.Right) string { return fmt.Sprintf("%s%.0f%s", prefix, k, r) }

	var brokenMu sync.Mutex
	broken := 0
	fetch := func(tk string) ([]market.Bar, error) {
		cs, err := c.ExpiredHistory(ctx, tk, fyers.Minute, start, end)
		if errors.Is(err, fyers.ErrBroken) {
			// -99 is often transient: one more try after a pause
			time.Sleep(5 * time.Second)
			cs, err = c.ExpiredHistory(ctx, tk, fyers.Minute, start, end)
			if errors.Is(err, fyers.ErrBroken) {
				brokenMu.Lock()
				broken++
				brokenMu.Unlock()
				return nil, nil
			}
		}
		if errors.Is(err, fyers.ErrNoData) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		bs := make([]market.Bar, 0, len(cs))
		for _, cd := range cs {
			bs = append(bs, market.Bar{Timeframe: market.Minute1, Time: cd.Time, End: cd.Time.Add(time.Minute),
				Open: rupeesToPaise(cd.Open), High: rupeesToPaise(cd.High), Low: rupeesToPaise(cd.Low), Close: rupeesToPaise(cd.Close),
				Volume: cd.Volume, OI: cd.OI})
		}
		return bs, nil
	}

	// Probe at the money near the window's end.
	atm := math.Round(last/step) * step
	var probe []market.Bar
	var probeTk string
probing:
	for _, k := range []float64{atm, atm + step, atm - step, atm + 2*step, atm - 2*step} {
		for _, r := range []market.Right{market.Call, market.Put} {
			probeTk = ticker(k, r)
			bs, err := fetch(probeTk)
			if err != nil {
				return man, err
			}
			if len(bs) > 0 {
				probe = bs
				break probing
			}
		}
	}
	if probe == nil {
		if broken > 0 {
			return man, fmt.Errorf("probe: Fyers failed (-99) on %d of 10 tickers; not marking the month missing", broken)
		}
		man.Missing = true
		return man, writeJSONFile(path, man)
	}

	var tickers []string
	for k := math.Floor(lo/step)*step - float64(pad)*step; k <= math.Ceil(hi/step)*step+float64(pad)*step+step/2; k += step {
		if k <= 0 {
			continue
		}
		for _, r := range []market.Right{market.Call, market.Put} {
			if tk := ticker(k, r); tk != probeTk {
				tickers = append(tickers, tk)
			}
		}
	}
	got := map[string][]market.Bar{probeTk: probe}
	var mu sync.Mutex
	var firstErr error
	jobs := make(chan string)
	var wg sync.WaitGroup
	for range max(workers, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for tk := range jobs {
				bs, err := fetch(tk)
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", tk, err)
				}
				if len(bs) > 0 {
					got[tk] = bs
				}
				mu.Unlock()
			}
		}()
	}
	for _, tk := range tickers {
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop {
			break
		}
		jobs <- tk
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return man, firstErr
	}

	// The month's expiry is the last day any of its options traded.
	var lastT time.Time
	for _, bs := range got {
		for _, b := range bs {
			if b.Time.After(lastT) {
				lastT = b.Time
			}
		}
	}
	man.Expiry = market.DateOf(lastT)
	var all []market.Bar
	var insts []market.Instrument
	keys := make([]string, 0, len(got))
	for tk := range got {
		keys = append(keys, tk)
	}
	sort.Strings(keys)
	for _, tk := range keys {
		body := strings.TrimPrefix(tk, prefix)
		r := market.Right(body[len(body)-2:])
		var k float64
		if _, err := fmt.Sscan(body[:len(body)-2], &k); err != nil {
			return man, fmt.Errorf("strike of %s: %w", tk, err)
		}
		in := market.Option(market.MCX, name, man.Expiry, rupeesToPaise(k), r, lot)
		insts = append(insts, in)
		for _, b := range got[tk] {
			b.Key = in.Key()
			all = append(all, b)
		}
	}
	if err := store.PutBars(ctx, name, all); err != nil {
		return man, err
	}
	if err := store.PutContracts(ctx, name, insts); err != nil {
		return man, err
	}
	man.Tickers, man.Found, man.Bars, man.Broken = len(tickers)+1, len(got), len(all), broken
	if err := writeJSONFile(path, man); err != nil {
		return man, err
	}
	stamp("%s options %s: expiry %s, %d of %d strikes×rights (%d -99), %d bars, future %s %.0f..%.0f", name, man.Month, man.Expiry,
		man.Found, man.Tickers, broken, man.Bars, fexp, lo, hi)
	return man, nil
}
