package data

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"

	"calm/internal/market"
)

const indexColumns = `date DATE, name VARCHAR, source VARCHAR, open BIGINT, high BIGINT, low BIGINT, close BIGINT,
	prev_close BIGINT, volume BIGINT, turnover_cr DOUBLE, pe DOUBLE, pb DOUBLE, yield DOUBLE`

func (s *Store) indexPath(year int) string {
	return filepath.Join(s.root, "indices", fmt.Sprintf("year=%d.parquet", year))
}

func nullable(v float64) any {
	if math.IsNaN(v) {
		return nil
	}
	return v
}

// ReplaceIndexYear writes a whole year of index records, replacing any stored
// file atomically. A year is always rebuilt from the raw files, so a corrected
// alias can't leave rows behind under the old name.
func (s *Store) ReplaceIndexYear(ctx context.Context, year int, rows []market.IndexDay) error {
	path := s.indexPath(year)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `CREATE OR REPLACE TEMP TABLE incoming_index (`+indexColumns+`)`); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `DROP TABLE IF EXISTS incoming_index`)
	err = conn.Raw(func(dc any) error {
		a, err := duckdb.NewAppenderFromConn(dc.(driver.Conn), "", "incoming_index")
		if err != nil {
			return err
		}
		for _, r := range rows {
			if r.Date.Year != year {
				a.Close()
				return fmt.Errorf("%s row in the %d file", r.Date, year)
			}
			day := time.Date(r.Date.Year, r.Date.Month, r.Date.Day, 0, 0, 0, 0, time.UTC)
			err := a.AppendRow(day, r.Name, r.Source, int64(r.Open), int64(r.High), int64(r.Low), int64(r.Close),
				int64(r.PrevClose), r.Volume, nullable(r.TurnoverCr), nullable(r.PE), nullable(r.PB), nullable(r.Yield))
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
	q := `COPY (SELECT * FROM incoming_index ORDER BY date, name) TO ` + quote(tmp) + ` (FORMAT parquet, COMPRESSION zstd)`
	if _, err := conn.ExecContext(ctx, q); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// IndexDays returns stored records within [from, to], ordered by date then name.
func (s *Store) IndexDays(ctx context.Context, from, to market.Date) ([]market.IndexDay, error) {
	glob := filepath.Join(s.root, "indices", "year=*.parquet")
	if m, err := filepath.Glob(glob); err != nil {
		return nil, err
	} else if len(m) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT date, name, source, open, high, low, close, prev_close, volume,
			turnover_cr, pe, pb, yield
		FROM read_parquet(`+quote(glob)+`, hive_partitioning = false)
		WHERE date BETWEEN CAST($1 AS DATE) AND CAST($2 AS DATE) ORDER BY date, name`, from.String(), to.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []market.IndexDay
	for rows.Next() {
		var (
			r                     market.IndexDay
			day                   time.Time
			o, h, l, c, p         int64
			turnover, pe, pb, yld sql.NullFloat64
		)
		if err := rows.Scan(&day, &r.Name, &r.Source, &o, &h, &l, &c, &p, &r.Volume, &turnover, &pe, &pb, &yld); err != nil {
			return nil, err
		}
		y, m, d := day.Date()
		r.Date = market.Date{Year: y, Month: m, Day: d}
		r.Open, r.High, r.Low, r.Close, r.PrevClose = market.Paise(o), market.Paise(h), market.Paise(l), market.Paise(c), market.Paise(p)
		r.TurnoverCr, r.PE, r.PB, r.Yield = orNaN(turnover), orNaN(pe), orNaN(pb), orNaN(yld)
		out = append(out, r)
	}
	return out, rows.Err()
}

func orNaN(v sql.NullFloat64) float64 {
	if v.Valid {
		return v.Float64
	}
	return math.NaN()
}
