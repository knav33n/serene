// Package ingest downloads Fyers history into a Parquet store laid out like
// serene's Upstox data, so backtests read it unchanged:
//
//	bars/underlying=NIFTY/date=*.parquet   1-minute bars of the index and its options, with OI
//	contracts/underlying=NIFTY.parquet     contract master (key, lot_size)
//	daily/underlying=NIFTY.parquet         daily index bars, for settlement
//	expiries/, lots/, probe.parquet        reference tables (see internal/data/tables.go)
//	fetch/<UNDERLYING>/*.json              resume state: what has been fetched
//
// Every job resumes where it stopped. Past expiries never change, so a
// finished one isn't fetched twice.
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"serene/internal/data"
	"serene/internal/fyers"
	oldingest "serene/internal/ingest"
	"serene/internal/market"
)

// Config is shared by the jobs.
type Config struct {
	Dir       string  // the Fyers store root
	UpstoxDir string  // optional Upstox store, read for lot sizes
	BandPct   float64 // keep strikes this far outside the index's range before expiry
	Workers   int     // contracts fetched at once; all share the client's limiter
	Log       func(format string, args ...any)
	Now       func() time.Time
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c Config) today() market.Date { return market.DateOf(c.now()) }

// lookback is how far before expiry a contract's history is sought. Weekly
// contracts list a few weeks out and monthlies up to a few months; the search
// stops at the first window with no earlier data.
const lookback = 400

// IndexRecord is the resume state of an underlying's spot series.
type IndexRecord struct {
	// Through is the last day fully fetched, as YYYY-MM-DD.
	Through string `json:"through"`
	// Closes holds each day's low and high close, for choosing strikes.
	Closes map[string][2]float64 `json:"closes"`
}

// ExpiryRecord is the resume state of a finished expiry.
type ExpiryRecord struct {
	Expiry   string   `json:"expiry"`
	LotSize  int64    `json:"lot_size"`
	Listed   int      `json:"listed"` // option contracts Fyers listed
	Unparsed []string `json:"unparsed,omitempty"`
	// Duplicates are tickers naming a contract already listed under another
	// ticker, which would otherwise merge two series into one key.
	Duplicates []string         `json:"duplicates,omitempty"`
	Low        float64          `json:"strike_low"`
	High       float64          `json:"strike_high"`
	Contracts  []ContractRecord `json:"contracts"`
	FetchedAt  time.Time        `json:"fetched_at"`
}

// ContractRecord is what was fetched for one contract.
type ContractRecord struct {
	Ticker  string `json:"ticker"`
	Key     string `json:"key"`
	Bars    int    `json:"bars"`
	Dropped int    `json:"dropped"` // candles outside the session
}

func stateDir(cfg Config, u fyers.Underlying) string { return filepath.Join(cfg.Dir, "fetch", u.Name) }

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

// session is the derivatives session bars are kept within. BSE derivatives
// keep NSE's hours.
var session = market.NSESession

func toBars(key string, cs []fyers.Candle) ([]market.Bar, int) {
	out := make([]market.Bar, 0, len(cs))
	dropped := 0
	for _, c := range cs {
		t := c.Time.In(market.IST)
		if !session.Contains(t) {
			dropped++
			continue
		}
		out = append(out, market.Bar{Key: key, Timeframe: market.Minute1, Time: t, End: t.Add(time.Minute),
			Open: market.Rupees(c.Open), High: market.Rupees(c.High), Low: market.Rupees(c.Low),
			Close: market.Rupees(c.Close), Volume: c.Volume, OI: c.OI})
	}
	return out, dropped
}

// Index fetches an underlying's 1-minute spot bars from `from`, or from where
// the last run stopped, through today, and replaces its daily history.
func Index(ctx context.Context, cfg Config, store *data.Store, c *fyers.Client, u fyers.Underlying, from market.Date) (*IndexRecord, error) {
	path := filepath.Join(stateDir(cfg, u), "index.json")
	rec := &IndexRecord{Closes: map[string][2]float64{}}
	if _, err := readJSON(path, rec); err != nil {
		return nil, err
	}
	if rec.Through != "" {
		t, err := market.ParseDate(rec.Through)
		if err != nil {
			return nil, err
		}
		if next := t.AddDays(1); from.Before(next) {
			from = next
		}
	}
	today := cfg.today()
	for _, w := range windows(from, today, fyers.MaxIntradayDays) {
		cs, err := c.History(ctx, u.Symbol, fyers.Minute, w[0], w[1], false)
		if err != nil {
			return nil, err
		}
		bars, _ := toBars(u.SpotKey(), cs)
		if err := store.PutBars(ctx, u.Name, bars); err != nil {
			return nil, err
		}
		for _, b := range bars {
			d := market.DateOf(b.Time).String()
			px := b.Close.Rupees()
			v, ok := rec.Closes[d]
			if !ok {
				v = [2]float64{px, px}
			}
			rec.Closes[d] = [2]float64{min(v[0], px), max(v[1], px)}
		}
		// Today is still trading, so it is fetched again next run.
		through := w[1]
		if !through.Before(today) {
			through = today.AddDays(-1)
		}
		if through.String() > rec.Through {
			rec.Through = through.String()
		}
		if err := writeJSON(path, rec); err != nil {
			return nil, err
		}
		cfg.Log("%s 1m %s..%s: %d bars", u.Name, w[0], w[1], len(bars))
	}

	daily, err := c.History(ctx, u.Symbol, fyers.Day, market.Date{Year: 1995, Month: time.January, Day: 1}, today.AddDays(-1), false)
	if err != nil {
		return nil, fmt.Errorf("daily: %w", err)
	}
	bars := make([]market.Bar, 0, len(daily))
	for _, d := range daily {
		day := market.DateOf(d.Time)
		bars = append(bars, market.Bar{Key: u.SpotKey(), Timeframe: market.Daily, Time: day.At(0, 0),
			End: day.AddDays(1).At(0, 0), Open: market.Rupees(d.Open), High: market.Rupees(d.High),
			Low: market.Rupees(d.Low), Close: market.Rupees(d.Close), Volume: d.Volume})
	}
	if len(bars) > 0 {
		if err := store.ReplaceDaily(ctx, u.Name, bars); err != nil {
			return nil, err
		}
		cfg.Log("%s daily: %d days from %s", u.Name, len(bars), market.DateOf(bars[0].Time))
	}
	return rec, nil
}

func windows(from, to market.Date, n int) [][2]market.Date {
	var out [][2]market.Date
	for start := from; !to.Before(start); {
		end := start.AddDays(n - 1)
		if to.Before(end) {
			end = to
		}
		out = append(out, [2]market.Date{start, end})
		start = end.AddDays(1)
	}
	return out
}

// Backfill downloads every expired option expiry of an underlying in [from, to]
// that isn't already finished, newest first, so an interrupted job still
// leaves the most useful data. Run Index first: strikes are chosen from it.
func Backfill(ctx context.Context, cfg Config, store *data.Store, c *fyers.Client, u fyers.Underlying, from, to market.Date) error {
	if from.Before(u.Earliest()) {
		from = u.Earliest()
	}
	if last := cfg.today().AddDays(-1); last.Before(to) {
		to = last
	}
	idx := &IndexRecord{}
	if ok, err := readJSON(filepath.Join(stateDir(cfg, u), "index.json"), idx); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("no %s index data: run serene fyers index first", u.Name)
	}

	ex, err := c.ExpiryDates(ctx, u.Symbol, from, to)
	if err != nil {
		return err
	}
	if err := store.PutExpiries(ctx, u.Name, data.ExpiryOption, ex.Options); err != nil {
		return err
	}
	if err := store.PutExpiries(ctx, u.Name, data.ExpiryFuture, ex.Futures); err != nil {
		return err
	}
	expiries := ex.Options
	sort.Slice(expiries, func(i, j int) bool { return expiries[j].Before(expiries[i]) })
	if len(expiries) == 0 {
		cfg.Log("%s: no option expiries between %s and %s", u.Name, from, to)
		return nil
	}
	cfg.Log("%s: %d option expiries, %s to %s", u.Name, len(expiries), expiries[len(expiries)-1], expiries[0])

	if err := seedLots(ctx, cfg, store, u); err != nil {
		return err
	}
	lots, err := store.Lots(ctx, u.Name)
	if err != nil {
		return err
	}

	for i, e := range expiries {
		path := filepath.Join(stateDir(cfg, u), e.String()+".json")
		if ok, err := readJSON(path, &ExpiryRecord{}); err != nil {
			return err
		} else if ok {
			continue
		}
		lot, ok := data.LotOn(lots, e)
		if !ok {
			return fmt.Errorf("no lot size for %s expiry %s: add one with serene fyers lot --underlying %s --from DATE --size N",
				u.Name, e, u.Name)
		}
		rec, err := fetchExpiry(ctx, cfg, store, c, u, e, lot, idx)
		if err != nil {
			return fmt.Errorf("%s expiry %s: %w", u.Name, e, err)
		}
		if err := writeJSON(path, rec); err != nil {
			return err
		}
		bars := 0
		for _, r := range rec.Contracts {
			bars += r.Bars
		}
		use := c.Limiter.Usage()
		cfg.Log("[%d/%d] %s %s: %d of %d contracts, %d bars; %d requests today", i+1, len(expiries), u.Name, e,
			len(rec.Contracts), rec.Listed, bars, use.Requests)
	}
	return nil
}

// seedLots copies exact per-expiry lot sizes from the Upstox store, where it
// covers the underlying, so only older expiries need manual rows.
func seedLots(ctx context.Context, cfg Config, store *data.Store, u fyers.Underlying) error {
	if cfg.UpstoxDir == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(cfg.UpstoxDir, "contracts", "underlying="+u.Name+".parquet")); err != nil {
		return nil
	}
	up, err := data.Open(cfg.UpstoxDir)
	if err != nil {
		return err
	}
	defer up.Close()
	cs, err := up.Contracts(ctx, u.Name)
	if err != nil {
		return err
	}
	byExpiry := map[market.Date]int64{}
	for _, c := range cs {
		if c.LotSize > 0 {
			byExpiry[c.Expiry] = c.LotSize
		}
	}
	var lots []data.Lot
	for e, n := range byExpiry {
		lots = append(lots, data.Lot{From: e, Size: n, Source: "upstox"})
	}
	if len(lots) == 0 {
		return nil
	}
	return store.PutLots(ctx, u.Name, lots)
}

func fetchExpiry(ctx context.Context, cfg Config, store *data.Store, c *fyers.Client, u fyers.Underlying,
	e market.Date, lot int64, idx *IndexRecord) (ExpiryRecord, error) {
	listed, err := c.ExpiredContracts(ctx, u.Symbol, e)
	if err != nil {
		return ExpiryRecord{}, err
	}
	lo, hi, err := oldingest.StrikeBand(idx.Closes, e, cfg.BandPct)
	if err != nil {
		return ExpiryRecord{}, err
	}
	rec := ExpiryRecord{Expiry: e.String(), LotSize: lot, Listed: len(listed.Options), Low: lo, High: hi}
	type job struct {
		ticker string
		inst   market.Instrument
	}
	var jobs []job
	seen := map[string]bool{}
	for _, t := range listed.Options {
		inst, err := fyers.ParseContract(t, u, e)
		if err != nil {
			rec.Unparsed = append(rec.Unparsed, t)
			continue
		}
		if seen[inst.Key()] {
			rec.Duplicates = append(rec.Duplicates, t)
			continue
		}
		seen[inst.Key()] = true
		if s := inst.Strike.Rupees(); s < lo || s > hi {
			continue
		}
		inst.LotSize = lot
		jobs = append(jobs, job{t, inst})
	}
	if len(rec.Unparsed) > 0 {
		cfg.Log("%s %s: %d tickers not understood, e.g. %s", u.Name, e, len(rec.Unparsed), rec.Unparsed[0])
	}
	if len(rec.Duplicates) > 0 {
		cfg.Log("%s %s: skipped %d tickers naming an already listed contract, e.g. %s", u.Name, e, len(rec.Duplicates), rec.Duplicates[0])
	}

	workers := max(cfg.Workers, 1)
	results := make([]ContractRecord, len(jobs))
	barsOf := make([][]market.Bar, len(jobs))
	errs := make([]error, len(jobs))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	next := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range next {
				cs, err := contractHistory(ctx, c, jobs[i].ticker, e)
				if err != nil {
					errs[i] = fmt.Errorf("%s: %w", jobs[i].ticker, err)
					cancel()
					continue
				}
				b, dropped := toBars(jobs[i].inst.Key(), cs)
				barsOf[i] = b
				results[i] = ContractRecord{Ticker: jobs[i].ticker, Key: jobs[i].inst.Key(), Bars: len(b), Dropped: dropped}
			}
		})
	}
	for i := range jobs {
		if ctx.Err() != nil {
			break
		}
		next <- i
	}
	close(next)
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return rec, err
	}
	if ctx.Err() != nil {
		return rec, ctx.Err()
	}

	var bars []market.Bar
	var instruments []market.Instrument
	for i, j := range jobs {
		bars = append(bars, barsOf[i]...)
		if results[i].Bars > 0 {
			instruments = append(instruments, j.inst)
		}
		rec.Contracts = append(rec.Contracts, results[i])
	}
	if err := store.PutBars(ctx, u.Name, bars); err != nil {
		return rec, err
	}
	if len(instruments) > 0 {
		if err := store.PutContracts(ctx, u.Name, instruments); err != nil {
			return rec, err
		}
	}
	rec.FetchedAt = time.Now()
	return rec, nil
}

// contractHistory walks back from expiry in 100-day windows, stopping once a
// window holds no data from its first days: the contract wasn't listed yet.
func contractHistory(ctx context.Context, c *fyers.Client, ticker string, e market.Date) ([]fyers.Candle, error) {
	var out []fyers.Candle
	to := e
	for {
		from := to.AddDays(-(fyers.MaxIntradayDays - 1))
		cs, err := c.ExpiredHistory(ctx, ticker, fyers.Minute, from, to)
		if err != nil {
			return nil, err
		}
		out = append(cs, out...)
		if len(cs) == 0 || from.AddDays(5).Before(market.DateOf(cs[0].Time)) || from.Before(e.AddDays(-lookback)) {
			return out, nil
		}
		to = from.AddDays(-1)
	}
}

// Update is the daily job: extend each spot series, record the live contracts'
// lot sizes, and backfill expiries that finished since the last run.
func Update(ctx context.Context, cfg Config, store *data.Store, c *fyers.Client, us []fyers.Underlying, segments []string) error {
	for _, seg := range segments {
		rows, err := c.SymbolMaster(ctx, seg)
		if err != nil {
			return err
		}
		for _, u := range us {
			byExpiry := map[market.Date]int64{}
			for _, r := range rows {
				if r.Underlying == u.Name && r.LotSize > 0 && (r.Right == "CE" || r.Right == "PE") {
					byExpiry[r.Expiry] = r.LotSize
				}
			}
			var lots []data.Lot
			for e, n := range byExpiry {
				lots = append(lots, data.Lot{From: e, Size: n, Source: "symbol_master"})
			}
			if len(lots) > 0 {
				if err := store.PutLots(ctx, u.Name, lots); err != nil {
					return err
				}
			}
		}
	}
	today := cfg.today()
	for _, u := range us {
		if _, err := Index(ctx, cfg, store, c, u, today.AddDays(-30)); err != nil {
			return fmt.Errorf("%s index: %w", u.Name, err)
		}
		if err := Backfill(ctx, cfg, store, c, u, today.AddDays(-60), today); err != nil {
			return err
		}
	}
	return nil
}
