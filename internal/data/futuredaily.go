package data

import (
	"context"
	"database/sql/driver"
	"fmt"
	"os"
	"path/filepath"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"

	"serene/internal/market"
)

// Continuous daily futures sit beside the 1-minute bars:
//
//	daily/underlying=CRUDEOIL.parquet   one row a day, the near contract rolled at expiry

func (s *Store) dailyPath(underlying string) string {
	return filepath.Join(s.root, "daily", "underlying="+underlying+".parquet")
}

// ReplaceDaily writes an underlying's whole daily history, replacing any stored
// file atomically. Prices are in paise.
func (s *Store) ReplaceDaily(ctx context.Context, underlying string, bars []market.Bar) error {
	if !safeName.MatchString(underlying) {
		return fmt.Errorf("invalid underlying %q", underlying)
	}
	path := s.dailyPath(underlying)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `CREATE OR REPLACE TEMP TABLE incoming_daily (date DATE, open BIGINT,
		high BIGINT, low BIGINT, close BIGINT, volume BIGINT, oi BIGINT)`); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `DROP TABLE IF EXISTS incoming_daily`)
	err = conn.Raw(func(dc any) error {
		a, err := duckdb.NewAppenderFromConn(dc.(driver.Conn), "", "incoming_daily")
		if err != nil {
			return err
		}
		for _, b := range bars {
			if b.Timeframe != market.Daily {
				a.Close()
				return fmt.Errorf("daily file got a %s bar", b.Timeframe)
			}
			d := market.DateOf(b.Time)
			day := time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
			if err := a.AppendRow(day, int64(b.Open), int64(b.High), int64(b.Low), int64(b.Close), b.Volume, b.OI); err != nil {
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
	q := `COPY (SELECT * FROM incoming_daily QUALIFY row_number() OVER (PARTITION BY date) = 1 ORDER BY date)
		TO ` + quote(tmp) + ` (FORMAT parquet, COMPRESSION zstd)`
	if _, err := conn.ExecContext(ctx, q); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Daily returns an underlying's stored continuous daily bars, by date.
func (s *Store) Daily(ctx context.Context, underlying string) ([]market.Bar, error) {
	if !safeName.MatchString(underlying) {
		return nil, fmt.Errorf("invalid underlying %q", underlying)
	}
	path := s.dailyPath(underlying)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no daily data for %s: %w", underlying, err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT date, open, high, low, close, volume, oi
		FROM read_parquet(`+quote(path)+`, hive_partitioning = false) ORDER BY date`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []market.Bar
	for rows.Next() {
		var (
			b                   market.Bar
			day                 time.Time
			open, high, low, cl int64
		)
		if err := rows.Scan(&day, &open, &high, &low, &cl, &b.Volume, &b.OI); err != nil {
			return nil, err
		}
		b.Key, b.Timeframe = underlying, market.Daily
		b.Time = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, market.IST)
		b.End = b.Time.AddDate(0, 0, 1)
		b.Open, b.High, b.Low, b.Close = market.Paise(open), market.Paise(high), market.Paise(low), market.Paise(cl)
		out = append(out, b)
	}
	return out, rows.Err()
}
