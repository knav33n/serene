package data

import (
	"context"
	"database/sql/driver"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"

	"calm/internal/market"
)

const equityColumns = `date DATE, symbol VARCHAR, series VARCHAR, isin VARCHAR, open BIGINT, high BIGINT, low BIGINT,
	close BIGINT, last BIGINT, prev_close BIGINT, volume BIGINT, turnover DOUBLE, trades BIGINT`

func (s *Store) equityPath(year int) string {
	return filepath.Join(s.root, "stocks", fmt.Sprintf("year=%d.parquet", year))
}

// ReplaceEquityYear writes a whole year of bhavcopy records, replacing any
// stored file atomically. Prices are in paise, turnover in rupees.
func (s *Store) ReplaceEquityYear(ctx context.Context, year int, rows []market.EquityDay) error {
	path := s.equityPath(year)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `CREATE OR REPLACE TEMP TABLE incoming_equity (`+equityColumns+`)`); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `DROP TABLE IF EXISTS incoming_equity`)
	err = conn.Raw(func(dc any) error {
		a, err := duckdb.NewAppenderFromConn(dc.(driver.Conn), "", "incoming_equity")
		if err != nil {
			return err
		}
		for _, r := range rows {
			if r.Date.Year != year {
				a.Close()
				return fmt.Errorf("%s row in the %d file", r.Date, year)
			}
			day := time.Date(r.Date.Year, r.Date.Month, r.Date.Day, 0, 0, 0, 0, time.UTC)
			var isin any
			if r.ISIN != "" {
				isin = r.ISIN
			}
			err := a.AppendRow(day, r.Symbol, r.Series, isin, int64(r.Open), int64(r.High), int64(r.Low),
				int64(r.Close), int64(r.Last), int64(r.PrevClose), r.Volume, r.Turnover, r.Trades)
			if err != nil {
				a.Close()
				return err
			}
		}
		return a.Close()
	})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	defer os.Remove(tmp)
	q := `COPY (SELECT * FROM incoming_equity ORDER BY date, symbol, series) TO ` + quote(tmp) +
		` (FORMAT parquet, COMPRESSION zstd)`
	if _, err := conn.ExecContext(ctx, q); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// EquityDays returns one security's records within [from, to], by date.
func (s *Store) EquityDays(ctx context.Context, symbol, series string, from, to market.Date) ([]market.EquityDay, error) {
	glob := filepath.Join(s.root, "stocks", "year=*.parquet")
	if m, err := filepath.Glob(glob); err != nil || len(m) == 0 {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT date, symbol, series, coalesce(isin, ''), open, high, low, close, last,
			prev_close, volume, turnover, trades
		FROM read_parquet(`+quote(glob)+`, hive_partitioning = false)
		WHERE symbol = $1 AND series = $2 AND date BETWEEN CAST($3 AS DATE) AND CAST($4 AS DATE) ORDER BY date`,
		symbol, series, from.String(), to.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []market.EquityDay
	for rows.Next() {
		var (
			r                 market.EquityDay
			day               time.Time
			o, h, l, c, la, p int64
		)
		if err := rows.Scan(&day, &r.Symbol, &r.Series, &r.ISIN, &o, &h, &l, &c, &la, &p, &r.Volume, &r.Turnover, &r.Trades); err != nil {
			return nil, err
		}
		y, m, d := day.Date()
		r.Date = market.Date{Year: y, Month: m, Day: d}
		r.Open, r.High, r.Low, r.Close = market.Paise(o), market.Paise(h), market.Paise(l), market.Paise(c)
		r.Last, r.PrevClose = market.Paise(la), market.Paise(p)
		out = append(out, r)
	}
	return out, rows.Err()
}

// EquitySeries are the series that hold a company's shares, as opposed to its
// bonds, gold bonds or units.
const EquitySeries = `('EQ', 'BE', 'BZ', 'SM', 'ST')`

func (s *Store) equityGlob() (string, bool, error) {
	glob := filepath.Join(s.root, "stocks", "year=*.parquet")
	m, err := filepath.Glob(glob)
	return glob, len(m) > 0, err
}

// EquityAliases returns the other symbols that have shared an ISIN with a
// symbol, which is how a rename shows, such as INFOSYSTCH becoming INFY in
// 2011. ISINs are in the files from 2011-06-22, so earlier renames don't show.
func (s *Store) EquityAliases(ctx context.Context, symbol string) ([]string, error) {
	glob, ok, err := s.equityGlob()
	if err != nil || !ok {
		return nil, err
	}
	src := `read_parquet(` + quote(glob) + `, hive_partitioning = false)`
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT symbol FROM `+src+` WHERE series IN `+EquitySeries+`
		AND symbol <> $1 AND isin IN (SELECT isin FROM `+src+` WHERE symbol = $1 AND isin IS NOT NULL
			AND series IN `+EquitySeries+`) ORDER BY symbol`, symbol)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// EquityHistory returns the share prices of one company, traded under any of
// the symbols given, on every date, across the equity series a stock moves
// between. On a date with two rows, EQ is kept, else the one with more volume.
func (s *Store) EquityHistory(ctx context.Context, symbols ...string) ([]market.EquityDay, error) {
	glob, ok, err := s.equityGlob()
	if err != nil || !ok || len(symbols) == 0 {
		return nil, err
	}
	list := make([]string, len(symbols))
	for i, sym := range symbols {
		list[i] = quote(sym)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT date, symbol, series, coalesce(isin, ''), open, high, low, close, volume
		FROM read_parquet(`+quote(glob)+`, hive_partitioning = false)
		WHERE symbol IN (`+strings.Join(list, ", ")+`) AND series IN `+EquitySeries+`
		QUALIFY row_number() OVER (PARTITION BY date ORDER BY series = 'EQ' DESC, volume DESC) = 1
		ORDER BY date`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []market.EquityDay
	for rows.Next() {
		var (
			r          market.EquityDay
			day        time.Time
			o, h, l, c int64
		)
		if err := rows.Scan(&day, &r.Symbol, &r.Series, &r.ISIN, &o, &h, &l, &c, &r.Volume); err != nil {
			return nil, err
		}
		y, m, d := day.Date()
		r.Date = market.Date{Year: y, Month: m, Day: d}
		r.Open, r.High, r.Low, r.Close = market.Paise(o), market.Paise(h), market.Paise(l), market.Paise(c)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Listed is one symbol in the latest file, with the ISIN it trades under.
type Listed struct{ Symbol, ISIN string }

// EquityListed returns the equity symbols in the newest stored file.
func (s *Store) EquityListed(ctx context.Context) ([]Listed, error) {
	glob, ok, err := s.equityGlob()
	if err != nil || !ok {
		return nil, err
	}
	src := `read_parquet(` + quote(glob) + `, hive_partitioning = false)`
	rows, err := s.db.QueryContext(ctx, `SELECT symbol, isin FROM `+src+` WHERE series IN `+EquitySeries+`
		AND date = (SELECT max(date) FROM `+src+`) AND isin IS NOT NULL
		QUALIFY row_number() OVER (PARTITION BY symbol ORDER BY series = 'EQ' DESC) = 1 ORDER BY symbol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Listed
	for rows.Next() {
		var l Listed
		if err := rows.Scan(&l.Symbol, &l.ISIN); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// EquityGaps returns the symbols that, on a date from since on, opened and
// closed below 0.92 or above 1.9 times their previous close, or changed ISIN.
// Every split and bonus ratio is at most 10/11, and every consolidation at
// least 2, so these are the days one may have happened; only a check against
// adjusted prices confirms it. A face value change also brings a new ISIN.
func (s *Store) EquityGaps(ctx context.Context, since market.Date) ([]string, error) {
	glob, ok, err := s.equityGlob()
	if err != nil || !ok {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `WITH d AS (
			SELECT symbol, date, open, close, isin,
				lag(close) OVER w AS pc, lag(isin) OVER w AS pisin
			FROM read_parquet(`+quote(glob)+`, hive_partitioning = false)
			WHERE series IN `+EquitySeries+` AND date >= CAST($1 AS DATE) - INTERVAL 30 DAY
			WINDOW w AS (PARTITION BY symbol ORDER BY date)
			QUALIFY row_number() OVER (PARTITION BY symbol, date ORDER BY series = 'EQ' DESC, volume DESC) = 1)
		SELECT DISTINCT symbol FROM d WHERE date >= CAST($1 AS DATE) AND pc > 0 AND
			(open / pc < 0.92 AND close / pc < 0.92 OR open / pc > 1.9 AND close / pc > 1.9
				OR isin IS DISTINCT FROM pisin AND pisin IS NOT NULL)
		ORDER BY symbol`, since.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sym string
		if err := rows.Scan(&sym); err != nil {
			return nil, err
		}
		out = append(out, sym)
	}
	return out, rows.Err()
}

// FactorRow is one span of a symbol's cumulative adjustment factor.
type FactorRow struct {
	Symbol            string
	From, To          market.Date
	Factor, FactorAll float64 // splits and bonuses; every adjustment
}

// EventRow is one detected split, bonus or other adjustment.
type EventRow struct {
	Symbol, ISIN, Kind string
	ExDate             market.Date
	Ratio, Measured    float64
	Num, Den           int
}

func utcDay(d market.Date) time.Time { return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC) }

// ReplaceEquityAdjustments writes the adjustment tables, replacing any
// stored ones: adjust/factors.parquet, to join with the price files, and
// adjust/events.parquet, to read.
func (s *Store) ReplaceEquityAdjustments(ctx context.Context, factors []FactorRow, events []EventRow) error {
	dir := filepath.Join(s.root, "adjust")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	write := func(table, cols, order, path string, fill func(a *duckdb.Appender) error) error {
		if _, err := conn.ExecContext(ctx, `CREATE OR REPLACE TEMP TABLE `+table+` (`+cols+`)`); err != nil {
			return err
		}
		defer conn.ExecContext(ctx, `DROP TABLE IF EXISTS `+table)
		err := conn.Raw(func(dc any) error {
			a, err := duckdb.NewAppenderFromConn(dc.(driver.Conn), "", table)
			if err != nil {
				return err
			}
			if err := fill(a); err != nil {
				a.Close()
				return err
			}
			return a.Close()
		})
		if err != nil {
			return err
		}
		tmp := path + ".tmp"
		defer os.Remove(tmp)
		q := `COPY (SELECT * FROM ` + table + ` ORDER BY ` + order + `) TO ` + quote(tmp) + ` (FORMAT parquet, COMPRESSION zstd)`
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return err
		}
		return os.Rename(tmp, path)
	}
	err = write("incoming_factors", `symbol VARCHAR, from_date DATE, to_date DATE, factor DOUBLE, factor_all DOUBLE`, "symbol, from_date",
		filepath.Join(dir, "factors.parquet"), func(a *duckdb.Appender) error {
			for _, f := range factors {
				if err := a.AppendRow(f.Symbol, utcDay(f.From), utcDay(f.To), f.Factor, f.FactorAll); err != nil {
					return err
				}
			}
			return nil
		})
	if err != nil {
		return err
	}
	return write("incoming_events", `symbol VARCHAR, isin VARCHAR, ex_date DATE, kind VARCHAR, num INTEGER, den INTEGER,
		ratio DOUBLE, measured DOUBLE`, "symbol, ex_date", filepath.Join(dir, "events.parquet"), func(a *duckdb.Appender) error {
		for _, e := range events {
			var num, den any
			if e.Num > 0 {
				num, den = int32(e.Num), int32(e.Den)
			}
			if err := a.AppendRow(e.Symbol, e.ISIN, utcDay(e.ExDate), e.Kind, num, den, e.Ratio, e.Measured); err != nil {
				return err
			}
		}
		return nil
	})
}

// EquityFactors returns a symbol's factor spans, oldest first, or none if it
// has no adjustments stored.
func (s *Store) EquityFactors(ctx context.Context, symbol string) ([]FactorRow, error) {
	path := filepath.Join(s.root, "adjust", "factors.parquet")
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT from_date, to_date, factor, factor_all FROM read_parquet(`+quote(path)+`)
		WHERE symbol = $1 ORDER BY from_date`, symbol)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FactorRow
	for rows.Next() {
		f := FactorRow{Symbol: symbol}
		var from, to time.Time
		if err := rows.Scan(&from, &to, &f.Factor, &f.FactorAll); err != nil {
			return nil, err
		}
		f.From, f.To = market.DateOf(from.Add(12*time.Hour)), market.DateOf(to.Add(12*time.Hour))
		out = append(out, f)
	}
	return out, rows.Err()
}
