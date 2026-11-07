package adjust

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"calm/internal/data"
	"calm/internal/market"
)

// UpstoxFrom is the first date Upstox has daily candles for.
var UpstoxFrom = market.Date{Year: 2000, Month: 1, Day: 1}

// Security is what is known of one symbol's adjustments.
type Security struct {
	ISIN    string      `json:"isin"`
	Aliases []string    `json:"aliases,omitempty"` // earlier symbols of the same company
	Checked market.Date `json:"checked"`           // newest date compared with Upstox
	Error   string      `json:"error,omitempty"`   // why Upstox couldn't be compared
	Events  []Event     `json:"events,omitempty"`
}

// State is kept in Dir/adjust/state.json, so a long first run resumes and the
// daily job knows what it has scanned.
type State struct {
	Securities map[string]*Security `json:"securities"`
	Pending    []string             `json:"pending,omitempty"` // symbols waiting for a check
	Scanned    *market.Date         `json:"scanned,omitempty"` // newest date scanned for gaps
}

func statePath(dir string) string { return filepath.Join(dir, "adjust", "state.json") }

// Load reads the state, which is empty before the first run.
func Load(dir string) (*State, error) {
	s := &State{Securities: map[string]*Security{}}
	b, err := os.ReadFile(statePath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	if s.Securities == nil {
		s.Securities = map[string]*Security{}
	}
	return s, nil
}

// Save writes the state atomically.
func (s *State) Save(dir string) error {
	b, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	path := statePath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", b, 0o644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// AddPending queues symbols for a check, once each.
func (s *State) AddPending(symbols ...string) {
	for _, sym := range symbols {
		if !slices.Contains(s.Pending, sym) {
			s.Pending = append(s.Pending, sym)
		}
	}
	sort.Strings(s.Pending)
}

// Candles fetches adjusted daily prices for an ISIN.
type Candles func(ctx context.Context, isin string, from, to market.Date) ([]Day, error)

// Fatal tells a failure that should stop the run, such as a rejected token,
// from one that only concerns a symbol, such as an ISIN Upstox doesn't know.
type Fatal func(error) bool

// Check compares symbols with Upstox and records their events. It saves the
// state as it goes, so an interrupted run loses at most a few symbols.
func Check(ctx context.Context, dir string, st *State, store *data.Store, fetch Candles, fatal Fatal,
	todo []data.Listed, log func(string, ...any)) error {
	for i, l := range todo {
		aliases, err := store.EquityAliases(ctx, l.Symbol)
		if err != nil {
			return err
		}
		hist, err := store.EquityHistory(ctx, append([]string{l.Symbol}, aliases...)...)
		if err != nil {
			return err
		}
		if len(hist) == 0 {
			continue
		}
		last := hist[len(hist)-1].Date
		from := hist[0].Date
		if from.Before(UpstoxFrom) {
			from = UpstoxFrom
		}
		sec := &Security{ISIN: l.ISIN, Aliases: aliases, Checked: last}
		var up []Day
		for lo := from; !last.Before(lo); lo = lo.AddDays(3650) {
			// Upstox allows a decade of daily candles per request.
			hi := lo.AddDays(3649)
			if last.Before(hi) {
				hi = last
			}
			c, err := fetch(ctx, l.ISIN, lo, hi)
			if err != nil {
				if fatal(err) {
					return fmt.Errorf("%s: %w", l.Symbol, err)
				}
				sec.Error = err.Error()
				break
			}
			up = append(up, c...)
		}
		if sec.Error == "" {
			bh := make([]Day, len(hist))
			for k, h := range hist {
				bh[k] = Day{h.Date, h.Open.Rupees(), h.High.Rupees(), h.Low.Rupees(), h.Close.Rupees()}
			}
			sec.Events = Detect(up, bh)
		}
		st.Securities[l.Symbol] = sec
		st.Pending = slices.DeleteFunc(st.Pending, func(s string) bool { return s == l.Symbol })
		if (i+1)%25 == 0 || i+1 == len(todo) {
			if err := st.Save(dir); err != nil {
				return err
			}
			log("checked %d of %d, up to %s", i+1, len(todo), l.Symbol)
		}
	}
	return st.Save(dir)
}

// Write stores the factor and event tables for every checked symbol.
func Write(ctx context.Context, st *State, store *data.Store) (events int, err error) {
	var fs []data.FactorRow
	var es []data.EventRow
	syms := make([]string, 0, len(st.Securities))
	for s := range st.Securities {
		syms = append(syms, s)
	}
	sort.Strings(syms)
	for _, sym := range syms {
		sec := st.Securities[sym]
		spans := Spans(sec.Events)
		// An earlier symbol carries the same factors, unless it is now some
		// other company's and checked in its own right.
		for _, s := range append([]string{sym}, sec.Aliases...) {
			if _, own := st.Securities[s]; s != sym && own {
				continue
			}
			for _, sp := range spans {
				fs = append(fs, data.FactorRow{Symbol: s, From: sp.From, To: sp.To, Factor: sp.Factor, FactorAll: sp.FactorAll})
			}
		}
		for _, e := range sec.Events {
			es = append(es, data.EventRow{Symbol: sym, ISIN: sec.ISIN, Kind: e.Kind, ExDate: e.ExDate,
				Ratio: e.Ratio, Measured: e.Measured, Num: e.Num, Den: e.Den})
		}
	}
	return len(es), store.ReplaceEquityAdjustments(ctx, fs, es)
}
