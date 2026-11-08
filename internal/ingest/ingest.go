// Package ingest downloads expired index options from Upstox into the store.
//
// A manifest under the data directory records each finished expiry, so a
// download can stop at any time, including when the daily token expires, and
// resume with the same command. Past expiries never change, so they aren't
// fetched twice.
package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"calm/internal/data"
	"calm/internal/market"
	"calm/internal/upstox"
)

// IndexKeys maps supported underlyings to Upstox instrument keys.
var IndexKeys = map[string]string{"NIFTY": "NSE_INDEX|Nifty 50", "BANKNIFTY": "NSE_INDEX|Nifty Bank"}

// bandWindow is how long before expiry the index range is measured.
const bandWindow = 30

// lookback covers the traded life of all but the longest-dated contracts, and
// the index before the first expiry.
const lookback = 400

// Config is one download.
type Config struct {
	Underlying string
	Dir        string  // data directory; manifests go in Dir/fetch/<underlying>
	BandPct    float64 // keep strikes this far outside the index's recent range
	From, To   market.Date
	Log        func(format string, args ...any)
}

// ContractRecord is what was fetched for one contract.
type ContractRecord struct {
	Key     string `json:"key"`
	Bars    int    `json:"bars"`
	Dropped int    `json:"dropped"` // candles outside the session
}

// ExpiryRecord is the manifest of a finished expiry.
type ExpiryRecord struct {
	Expiry    string           `json:"expiry"`
	Weekly    bool             `json:"weekly"`
	LotSize   int64            `json:"lot_size"`
	Listed    int              `json:"listed"`
	Low       float64          `json:"strike_low"`
	High      float64          `json:"strike_high"`
	Contracts []ContractRecord `json:"contracts"`
	FetchedAt time.Time        `json:"fetched_at"`
}

// IndexRecord is the manifest of the index data.
type IndexRecord struct {
	// Months are finished calendar months, as YYYY-MM.
	Months []string `json:"months"`
	// Closes holds each day's low and high close, for choosing strikes.
	Closes map[string][2]float64 `json:"closes"`
}

func manifestDir(cfg Config) string { return filepath.Join(cfg.Dir, "fetch", cfg.Underlying) }

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

func toBars(key string, cs []upstox.Candle, s market.Session) ([]market.Bar, int) {
	out := make([]market.Bar, 0, len(cs))
	dropped := 0
	for _, c := range cs {
		t := c.Time.In(market.IST)
		// Special sessions such as Muhurat trading fall outside normal hours.
		if !s.Contains(t) {
			dropped++
			continue
		}
		out = append(out, market.Bar{Key: key, Timeframe: market.Minute1, Time: t, End: t.Add(time.Minute),
			Open: market.Rupees(c.Open), High: market.Rupees(c.High), Low: market.Rupees(c.Low),
			Close: market.Rupees(c.Close), Volume: c.Volume, OI: c.OI})
	}
	return out, dropped
}

// Run downloads every expiry in range that isn't already finished, newest
// first, so an interrupted job still leaves the most useful data.
func Run(ctx context.Context, cfg Config, store *data.Store, c *upstox.Client) error {
	indexKey, ok := IndexKeys[cfg.Underlying]
	if !ok {
		return fmt.Errorf("no Upstox key for %s", cfg.Underlying)
	}
	session := market.NSESession
	all, err := c.Expiries(ctx, indexKey)
	if err != nil {
		return fmt.Errorf("list expiries: %w", err)
	}
	var expiries []market.Date
	for _, e := range all {
		if !e.Before(cfg.From) && !cfg.To.Before(e) {
			expiries = append(expiries, e)
		}
	}
	if len(expiries) == 0 {
		return fmt.Errorf("no %s expiries between %s and %s", cfg.Underlying, cfg.From, cfg.To)
	}
	sort.Slice(expiries, func(i, j int) bool { return expiries[j].Before(expiries[i]) })
	newest, oldest := expiries[0], expiries[len(expiries)-1]
	cfg.Log("%d expiries, %s to %s", len(expiries), oldest, newest)

	idx, err := fetchIndex(ctx, cfg, store, c, indexKey, oldest.AddDays(-lookback), newest)
	if err != nil {
		return fmt.Errorf("index candles: %w", err)
	}

	for i, e := range expiries {
		path := filepath.Join(manifestDir(cfg), e.String()+".json")
		var done ExpiryRecord
		if ok, err := readJSON(path, &done); err != nil {
			return err
		} else if ok {
			continue
		}
		rec, err := fetchExpiry(ctx, cfg, store, c, indexKey, e, idx, session)
		if err != nil {
			return fmt.Errorf("expiry %s: %w", e, err)
		}
		if err := writeJSON(path, rec); err != nil {
			return err
		}
		bars := 0
		for _, r := range rec.Contracts {
			bars += r.Bars
		}
		cfg.Log("[%d/%d] %s: %d of %d contracts, %d bars, %d requests so far", i+1, len(expiries), e,
			len(rec.Contracts), rec.Listed, bars, c.Requests)
	}
	cfg.Log("finished: %d requests", c.Requests)
	return nil
}

func fetchIndex(ctx context.Context, cfg Config, store *data.Store, c *upstox.Client, key string, from, to market.Date) (*IndexRecord, error) {
	path := filepath.Join(manifestDir(cfg), "index.json")
	rec := &IndexRecord{Closes: map[string][2]float64{}}
	if _, err := readJSON(path, rec); err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, m := range rec.Months {
		have[m] = true
	}
	today := market.DateOf(time.Now())
	indexID := market.Index(market.NSE, cfg.Underlying).Key()
	for start := (market.Date{Year: from.Year, Month: from.Month, Day: 1}); !to.Before(start); {
		next := market.DateOf(start.At(12, 0).AddDate(0, 1, 0))
		month := start.String()[:7]
		end := next.AddDays(-1)
		if !have[month] {
			cs, err := c.IndexCandles(ctx, key, start, end)
			if err != nil {
				return nil, err
			}
			bars, _ := toBars(indexID, cs, market.NSESession)
			if err := store.PutBars(ctx, cfg.Underlying, bars); err != nil {
				return nil, err
			}
			for _, b := range bars {
				d := market.DateOf(b.Time).String()
				v, ok := rec.Closes[d]
				px := b.Close.Rupees()
				if !ok {
					v = [2]float64{px, px}
				}
				rec.Closes[d] = [2]float64{min(v[0], px), max(v[1], px)}
			}
			if end.Before(today) {
				rec.Months = append(rec.Months, month)
			}
			if err := writeJSON(path, rec); err != nil {
				return nil, err
			}
			cfg.Log("index %s: %d bars", month, len(bars))
		}
		start = next
	}
	return rec, nil
}

// strikeBand returns the index range over the window before expiry, widened by
// pct. Contracts outside it are too far out of the money to be worth a request.
func strikeBand(idx *IndexRecord, e market.Date, pct float64) (float64, float64, error) {
	lo, hi := 0.0, 0.0
	for d := e.AddDays(-bandWindow); !e.Before(d); d = d.AddDays(1) {
		v, ok := idx.Closes[d.String()]
		if !ok {
			continue
		}
		if lo == 0 {
			lo, hi = v[0], v[1]
		}
		lo, hi = min(lo, v[0]), max(hi, v[1])
	}
	if lo == 0 {
		return 0, 0, fmt.Errorf("no index data in the %d days before expiry", bandWindow)
	}
	return lo * (1 - pct/100), hi * (1 + pct/100), nil
}

func fetchExpiry(ctx context.Context, cfg Config, store *data.Store, c *upstox.Client, indexKey string,
	e market.Date, idx *IndexRecord, session market.Session) (ExpiryRecord, error) {
	listed, err := c.OptionContracts(ctx, indexKey, e)
	if err != nil {
		return ExpiryRecord{}, fmt.Errorf("list contracts: %w", err)
	}
	lo, hi, err := strikeBand(idx, e, cfg.BandPct)
	if err != nil {
		return ExpiryRecord{}, err
	}
	rec := ExpiryRecord{Expiry: e.String(), Listed: len(listed), Low: lo, High: hi}
	var bars []market.Bar
	var instruments []market.Instrument
	for _, ct := range listed {
		if ct.Strike < lo || ct.Strike > hi {
			continue
		}
		right := market.Right(strings.ToUpper(ct.Type))
		if right != market.Call && right != market.Put {
			return rec, fmt.Errorf("%s: unknown option type %q", ct.Symbol, ct.Type)
		}
		inst := market.Option(market.NFO, cfg.Underlying, e, market.Rupees(ct.Strike), right, ct.LotSize)
		rec.Weekly, rec.LotSize = ct.Weekly, ct.LotSize
		cs, err := c.ExpiredCandles(ctx, ct.Key, e.AddDays(-lookback), e)
		if err != nil {
			return rec, fmt.Errorf("%s: %w", ct.Symbol, err)
		}
		b, dropped := toBars(inst.Key(), cs, session)
		bars = append(bars, b...)
		instruments = append(instruments, inst)
		rec.Contracts = append(rec.Contracts, ContractRecord{Key: inst.Key(), Bars: len(b), Dropped: dropped})
	}
	if err := store.PutBars(ctx, cfg.Underlying, bars); err != nil {
		return rec, err
	}
	if len(instruments) > 0 {
		if err := store.PutContracts(ctx, cfg.Underlying, instruments); err != nil {
			return rec, err
		}
	}
	rec.FetchedAt = time.Now()
	return rec, nil
}
