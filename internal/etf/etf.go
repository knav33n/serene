// Package etf downloads daily and 1-minute history for every ETF traded on
// NSE, from Kite and Upstox, into Parquet files grouped by the country of the
// fund's holdings. Days the brokers lack are filled from the NSE bhavcopy.
package etf

import (
	"cmp"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"

	"serene/internal/kite"
	"serene/internal/market"
	"serene/internal/upstox"
)

// Country of each fund's holdings; every ETF not listed is india.
var Country = map[string]string{
	"MON100": "us", "MONQ50": "us", "MAFANG": "us", "MASPTOP50": "us",
	"HNGSNGBEES": "hongkong", "MAHKTECH": "hongkong",
}

// Aliases are earlier symbols of a fund that share no ISIN with its current
// one, so the bhavcopy can't link them: N100 split 1:10 into a new ISIN on
// 2021-06-16 and relisted as MON100. Renames that keep the ISIN, like SBI's
// twelve on 2026-10-30, are found without help.
var Aliases = map[string][]string{
	"MON100": {"N100"},
}

var countryOrder = map[string]int{"us": 0, "hongkong": 1, "india": 2}

// ETF is one fund under its current symbol.
type ETF struct {
	Symbol, Country string
	// Symbols are every name it has traded under, Symbol first.
	Symbols []string
	// ISINs are oldest first, so the last is the current one.
	ISINs       []string
	First, Last time.Time
}

// ISIN is the fund's current ISIN, which Upstox keys its history by.
func (e ETF) ISIN() string {
	if len(e.ISINs) == 0 {
		return ""
	}
	return e.ISINs[len(e.ISINs)-1]
}

// Bar is one daily or 1-minute candle in rupees.
type Bar struct {
	T                      time.Time
	Open, High, Low, Close float64
	Volume                 int64
	Source                 string // kite, upstox, bhav or bhav_adj
}

// Fetcher downloads into Dir. Kite, Upstox or both may be set.
type Fetcher struct {
	Dir      string
	BhavGlob string
	Kite     *kite.Client
	Upstox   *upstox.Client
	Log      io.Writer

	db     *sql.DB
	tokens map[string]int64
}

// Open starts an in-memory DuckDB for reading the bhavcopy and writing Parquet.
// Its memory is capped because a full run holds a million 1-minute bars.
func Open(dir, bhavGlob string) (*Fetcher, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`SET memory_limit = '512MB'; SET threads = 2; SET preserve_insertion_order = false`); err != nil {
		db.Close()
		return nil, err
	}
	return &Fetcher{Dir: dir, BhavGlob: filepath.ToSlash(bhavGlob), Log: os.Stdout, db: db}, nil
}

// Close releases DuckDB.
func (f *Fetcher) Close() error { return f.db.Close() }

func (f *Fetcher) logf(format string, a ...any) {
	fmt.Fprintf(f.Log, time.Now().In(market.IST).Format("15:04:05 ")+format+"\n", a...)
}

// listing is one symbol's run under one ISIN in the bhavcopy.
type listing struct {
	symbol, isin string
	first, last  time.Time
}

// Universe lists the ETFs traded since since, one per fund under its current
// symbol, ordered us, hongkong, india, then by symbol.
func (f *Fetcher) Universe(since time.Time) ([]ETF, error) {
	rows, err := f.db.Query(`SELECT symbol, isin, min(date), max(date) FROM read_parquet('` + f.BhavGlob + `',
		hive_partitioning = false) WHERE series = 'EQ' AND isin LIKE 'INF%' GROUP BY ALL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ls []listing
	for rows.Next() {
		var l listing
		if err := rows.Scan(&l.symbol, &l.isin, &l.first, &l.last); err != nil {
			return nil, err
		}
		ls = append(ls, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Aliases may have traded before ISINs were recorded.
	for _, als := range Aliases {
		for _, al := range als {
			var a, b sql.NullTime
			if err := f.db.QueryRow(`SELECT min(date), max(date) FROM read_parquet('`+f.BhavGlob+`', hive_partitioning = false)
				WHERE symbol = $1 AND series = 'EQ' AND coalesce(isin, '') = ''`, al).Scan(&a, &b); err != nil {
				return nil, err
			}
			if a.Valid {
				ls = append(ls, listing{al, "", a.Time, b.Time})
			}
		}
	}
	return buildUniverse(ls, since), nil
}

// buildUniverse keeps, for each ISIN traded since since, the symbol it last
// traded under, then links each one to its earlier names through shared ISINs
// and Aliases, never crossing into another current ETF.
func buildUniverse(ls []listing, since time.Time) []ETF {
	current := map[string]listing{} // isin -> its latest listing
	for _, l := range ls {
		if l.isin == "" {
			continue
		}
		if c, ok := current[l.isin]; !ok || l.last.After(c.last) {
			current[l.isin] = l
		}
	}
	live := map[string]bool{}
	for _, l := range current {
		if !l.last.Before(since) {
			live[l.symbol] = true
		}
	}
	var out []ETF
	for sym := range live {
		syms := map[string]bool{sym: true}
		for _, a := range Aliases[sym] {
			syms[a] = true
		}
		isins := map[string]bool{}
		for grew := true; grew; {
			grew = false
			for _, l := range ls {
				in := syms[l.symbol] || (l.isin != "" && isins[l.isin])
				if !in || (l.symbol != sym && live[l.symbol]) {
					continue
				}
				if !syms[l.symbol] {
					syms[l.symbol], grew = true, true
				}
				if l.isin != "" && !isins[l.isin] {
					isins[l.isin], grew = true, true
				}
			}
		}
		e := ETF{Symbol: sym, Country: cmp.Or(Country[sym], "india"), Symbols: []string{sym}}
		lastByISIN := map[string]time.Time{}
		for _, l := range ls {
			if !syms[l.symbol] || (l.isin != "" && !isins[l.isin]) {
				continue
			}
			if e.First.IsZero() || l.first.Before(e.First) {
				e.First = l.first
			}
			if l.last.After(e.Last) {
				e.Last = l.last
			}
			if l.isin != "" && l.last.After(lastByISIN[l.isin]) {
				lastByISIN[l.isin] = l.last
			}
		}
		for s := range syms {
			if s != sym {
				e.Symbols = append(e.Symbols, s)
			}
		}
		slices.Sort(e.Symbols[1:])
		for i := range lastByISIN {
			e.ISINs = append(e.ISINs, i)
		}
		slices.SortFunc(e.ISINs, func(a, b string) int { return lastByISIN[a].Compare(lastByISIN[b]) })
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b ETF) int {
		return cmp.Or(countryOrder[a.Country]-countryOrder[b.Country], strings.Compare(a.Symbol, b.Symbol))
	})
	return out
}

// NewestBhavDay is the last day in the bhavcopy store.
func (f *Fetcher) NewestBhavDay() (time.Time, error) {
	var d sql.NullTime
	err := f.db.QueryRow(`SELECT max(date) FROM read_parquet('` + f.BhavGlob + `', hive_partitioning = false)`).Scan(&d)
	if err == nil && !d.Valid {
		err = fmt.Errorf("no bhavcopy days in %s: run serene equities fetch", f.BhavGlob)
	}
	return d.Time, err
}
