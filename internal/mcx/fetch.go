// Package mcx downloads MCX commodity futures from Kite into the store.
//
// Neither Kite nor Upstox serves intraday candles for expired MCX contracts,
// so history comes in two parts:
//
//   - daily: Kite's continuous series, which stitches each commodity's expired
//     near-month contracts, back to when Kite's records start;
//   - minute: 1-minute bars for every future still trading, from its first
//     trade. Run again before contracts expire to keep them.
//
// A manifest per contract records how far its minute bars are stored, so a
// rerun only fetches the days since.
package mcx

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"calm/internal/data"
	"calm/internal/kite"
	"calm/internal/market"
)

// Config is one download.
type Config struct {
	Dir    string   // data directory; manifests go in Dir/fetch
	Names  []string // commodities to fetch; all when empty
	Daily  bool
	Minute bool
	// Lookback bounds the search back for a new contract's first trade.
	Lookback time.Duration
	Log      func(format string, args ...any)
}

// Since is the earliest date asked of Kite's continuous series.
var Since = market.Date{Year: 2000, Month: 1, Day: 1}

// Manifest records a contract's stored minute bars.
type Manifest struct {
	Key     string      `json:"key"`
	Symbol  string      `json:"symbol"`
	LotSize int64       `json:"lot_size"`
	First   market.Date `json:"first"`   // first bar stored; zero if none yet
	Through time.Time   `json:"through"` // fetched up to here
	Bars    int         `json:"bars"`
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

// UpstoxLots reads lot sizes, in units of the commodity, from Upstox's public
// MCX instrument file, keyed by exchange token. Kite gives MCX lot sizes as 1.
func UpstoxLots(ctx context.Context) (map[string]int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://assets.upstox.com/market-quote/instruments/exchange/MCX.json.gz", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstox MCX instruments: HTTP %d", resp.StatusCode)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Token   string `json:"exchange_token"`
		LotSize int64  `json:"lot_size"`
	}
	if err := json.NewDecoder(gz).Decode(&rows); err != nil {
		return nil, fmt.Errorf("upstox MCX instruments: %w", err)
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.Token] = r.LotSize
	}
	return out, nil
}

func future(in kite.Instrument, lot int64) market.Instrument {
	return market.Instrument{Exchange: market.MCX, Kind: market.KindFuture, Underlying: in.Name,
		Expiry: in.Expiry, LotSize: lot}
}

func toBars(key string, tf market.Timeframe, cs []kite.Candle) []market.Bar {
	out := make([]market.Bar, len(cs))
	for i, c := range cs {
		t := c.Time.In(market.IST)
		end := t.Add(time.Minute)
		if tf == market.Daily {
			d := market.DateOf(t)
			t, end = d.At(0, 0), d.AddDays(1).At(0, 0)
		}
		out[i] = market.Bar{Key: key, Timeframe: tf, Time: t, End: end,
			Open: market.Rupees(c.Open), High: market.Rupees(c.High), Low: market.Rupees(c.Low),
			Close: market.Rupees(c.Close), Volume: c.Volume, OI: c.OI}
	}
	return out
}

// Run downloads the configured series.
func Run(ctx context.Context, cfg Config, store *data.Store, kc *kite.Client, lots map[string]int64) error {
	all, err := kc.Instruments(ctx, "MCX")
	if err != nil {
		return fmt.Errorf("instrument list: %w", err)
	}
	byName := map[string][]kite.Instrument{}
	for _, in := range all {
		if in.Type != "FUT" || in.Expiry.IsZero() {
			continue
		}
		if len(cfg.Names) > 0 && !slices.Contains(cfg.Names, in.Name) {
			continue
		}
		byName[in.Name] = append(byName[in.Name], in)
	}
	if len(byName) == 0 {
		return fmt.Errorf("no MCX futures match %v", cfg.Names)
	}
	names := make([]string, 0, len(byName))
	for n, fs := range byName {
		names = append(names, n)
		sort.Slice(fs, func(i, j int) bool { return fs[i].Expiry.Before(fs[j].Expiry) })
	}
	sort.Strings(names)
	cfg.Log("%d commodities, %d futures trading", len(names), func() (n int) {
		for _, fs := range byName {
			n += len(fs)
		}
		return
	}())

	for i, name := range names {
		futs := byName[name]
		var contracts []market.Instrument
		for _, f := range futs {
			if lot := lots[f.ExchangeToken]; lot > 0 {
				contracts = append(contracts, future(f, lot))
			} else {
				cfg.Log("%s: no lot size from Upstox, contract master skipped", f.Symbol)
			}
		}
		if len(contracts) > 0 {
			if err := store.PutContracts(ctx, name, contracts); err != nil {
				return err
			}
		}
		if cfg.Daily {
			n, first, err := fetchDaily(ctx, store, kc, name, futs[0])
			if err != nil {
				return fmt.Errorf("%s daily: %w", name, err)
			}
			cfg.Log("[%d/%d] %s daily: %d days from %s", i+1, len(names), name, n, first)
		}
		if cfg.Minute {
			for _, f := range futs {
				m, err := fetchMinute(ctx, cfg, store, kc, f, lots[f.ExchangeToken])
				if err != nil {
					return fmt.Errorf("%s minute: %w", f.Symbol, err)
				}
				first := "no trades"
				if !m.First.IsZero() {
					first = "from " + m.First.String()
				}
				cfg.Log("[%d/%d] %s: %d bars %s, %d requests so far", i+1, len(names), f.Symbol, m.Bars, first, kc.Requests)
			}
		}
	}
	cfg.Log("finished: %d requests", kc.Requests)
	return nil
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// fetchDaily replaces a commodity's continuous daily series, asked of its
// nearest future.
func fetchDaily(ctx context.Context, store *data.Store, kc *kite.Client, name string, near kite.Instrument) (int, market.Date, error) {
	key := market.Instrument{Exchange: market.MCX, Kind: market.KindIndex, Underlying: name}.Key()
	now := time.Now()
	var bars []market.Bar
	for from := Since.At(0, 0); from.Before(now); from = from.Add(kite.DaySpan) {
		to := earlier(from.Add(kite.DaySpan-time.Second), now)
		cs, err := kc.Candles(ctx, near.Token, "day", from, to, true)
		if err != nil {
			return 0, market.Date{}, err
		}
		bars = append(bars, toBars(key, market.Daily, cs)...)
	}
	if len(bars) == 0 {
		return 0, market.Date{}, nil
	}
	if err := store.ReplaceDaily(ctx, name, bars); err != nil {
		return 0, market.Date{}, err
	}
	return len(bars), market.DateOf(bars[0].Time), nil
}

// fetchMinute brings one contract's minute bars up to now. The first time, it
// walks back a window at a time until a window before the first trade is empty.
func fetchMinute(ctx context.Context, cfg Config, store *data.Store, kc *kite.Client, f kite.Instrument, lot int64) (Manifest, error) {
	key := future(f, lot).Key()
	path := filepath.Join(cfg.Dir, "fetch", f.Name, f.Expiry.String()+".json")
	m := Manifest{Key: key, Symbol: f.Symbol, LotSize: lot}
	if _, err := readJSON(path, &m); err != nil {
		return m, err
	}
	now := time.Now()
	put := func(cs []kite.Candle) error {
		if len(cs) == 0 {
			return nil
		}
		bars := toBars(key, market.Minute1, cs)
		if err := store.PutBars(ctx, f.Name, bars); err != nil {
			return err
		}
		m.Bars += len(bars)
		if d := market.DateOf(bars[0].Time); m.First.IsZero() || d.Before(m.First) {
			m.First = d
		}
		return nil
	}
	if !m.Through.IsZero() {
		// Refetch the last stored day, which may have been part-done.
		from := market.DateOf(m.Through).At(0, 0)
		for ; from.Before(now); from = from.Add(kite.MinuteSpan) {
			cs, err := kc.Candles(ctx, f.Token, "minute", from, earlier(from.Add(kite.MinuteSpan-time.Second), now), false)
			if err != nil {
				return m, err
			}
			// Bars of the refetched day replace, so they aren't counted twice.
			n := 0
			for _, c := range cs {
				if !c.Time.Before(m.Through) {
					n++
				}
			}
			before := m.Bars
			if err := put(cs); err != nil {
				return m, err
			}
			m.Bars = before + n
		}
	} else {
		seen := false
		for to := now; now.Sub(to) < cfg.Lookback; to = to.Add(-kite.MinuteSpan) {
			cs, err := kc.Candles(ctx, f.Token, "minute", to.Add(-kite.MinuteSpan+time.Second), to, false)
			if err != nil {
				return m, err
			}
			if len(cs) == 0 && seen {
				break
			}
			seen = seen || len(cs) > 0
			if err := put(cs); err != nil {
				return m, err
			}
		}
	}
	m.Through = now
	return m, writeJSON(path, m)
}
