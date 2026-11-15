package etf

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"serene/internal/market"
)

// Path is where an ETF's bars at frame ("daily" or "1m") are kept.
func (f *Fetcher) Path(e ETF, frame string) string {
	return filepath.Join(f.Dir, e.Country, frame, e.Symbol+".parquet")
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// read loads a written file, or nothing when there is none.
func (f *Fetcher) read(path string) ([]Bar, error) {
	if !exists(path) {
		return nil, nil
	}
	rows, err := f.db.Query(`SELECT ts, open, high, low, close, volume, source FROM read_parquet('` +
		filepath.ToSlash(path) + `') ORDER BY ts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bar
	for rows.Next() {
		var b Bar
		if err := rows.Scan(&b.T, &b.Open, &b.High, &b.Low, &b.Close, &b.Volume, &b.Source); err != nil {
			return nil, err
		}
		b.T = b.T.In(market.IST)
		out = append(out, b)
	}
	return out, rows.Err()
}

// merge sorts bars and keeps one per time, the later in the slice winning.
// Bars with a zero or negative price are dropped: Kite has a few old ones,
// and they aren't trades.
func merge(bars []Bar) ([]Bar, int) {
	slices.SortStableFunc(bars, func(a, b Bar) int { return a.T.Compare(b.T) })
	out := bars[:0]
	for _, b := range bars {
		if n := len(out); n > 0 && out[n-1].T.Equal(b.T) {
			out[n-1] = b
		} else {
			out = append(out, b)
		}
	}
	n := len(out)
	out = slices.DeleteFunc(out, func(b Bar) bool { return b.Open <= 0 || b.High <= 0 || b.Low <= 0 || b.Close <= 0 })
	return out, n - len(out)
}

// write replaces path with bars, already merged.
func (f *Fetcher) write(e ETF, path string, bars []Bar) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "etf_"+e.Symbol+"_*.csv")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w := csv.NewWriter(tmp)
	w.Write([]string{"ts", "open", "high", "low", "close", "volume", "source"})
	ff := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	for _, b := range bars {
		w.Write([]string{b.T.In(market.IST).Format(time.RFC3339), ff(b.Open), ff(b.High), ff(b.Low), ff(b.Close),
			strconv.FormatInt(b.Volume, 10), b.Source})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Write beside the target, then rename, so a crash never leaves half a file.
	part := path + ".part"
	_, err = f.db.Exec(fmt.Sprintf(`COPY (SELECT '%s' AS symbol, '%s' AS country, CAST(ts AS TIMESTAMPTZ) AS ts,
			open, high, low, close, volume, source
		FROM read_csv('%s', header = true, columns = {'ts': 'VARCHAR', 'open': 'DOUBLE', 'high': 'DOUBLE', 'low': 'DOUBLE',
			'close': 'DOUBLE', 'volume': 'BIGINT', 'source': 'VARCHAR'}) ORDER BY ts)
		TO '%s' (FORMAT parquet, COMPRESSION zstd)`, e.Symbol, e.Country, filepath.ToSlash(tmp.Name()), filepath.ToSlash(part)))
	if err != nil {
		os.Remove(part)
		return err
	}
	return os.Rename(part, path)
}

// WriteUniverse saves the list of ETFs as universe.csv.
func (f *Fetcher) WriteUniverse(etfs []ETF) error {
	file, err := os.Create(filepath.Join(f.Dir, "universe.csv"))
	if err != nil {
		return err
	}
	w := csv.NewWriter(file)
	w.Write([]string{"symbol", "country", "symbols", "isins", "first", "last"})
	for _, e := range etfs {
		w.Write([]string{e.Symbol, e.Country, strings.Join(e.Symbols, ";"), strings.Join(e.ISINs, ";"),
			e.First.Format(time.DateOnly), e.Last.Format(time.DateOnly)})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// Coverage summarises every file written as coverage.csv.
func (f *Fetcher) Coverage() error {
	root := filepath.ToSlash(f.Dir)
	_, err := f.db.Exec(`COPY (
		WITH d AS (SELECT country, symbol, min(ts)::DATE AS daily_first, max(ts)::DATE AS daily_last, count(*) AS daily_rows,
				count(*) FILTER (WHERE source LIKE 'bhav%') AS daily_from_bhav
			FROM read_parquet('` + root + `/*/daily/*.parquet') GROUP BY ALL),
		m AS (SELECT symbol, min(ts) AS m1_first, max(ts) AS m1_last, count(*) AS m1_rows
			FROM read_parquet('` + root + `/*/1m/*.parquet') GROUP BY ALL)
		SELECT d.*, m.m1_first, m.m1_last, coalesce(m.m1_rows, 0) AS m1_rows FROM d LEFT JOIN m USING (symbol)
		ORDER BY country, symbol) TO '` + root + `/coverage.csv' (HEADER)`)
	return err
}
