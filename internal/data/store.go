// Package data stores market data as Parquet, queried through DuckDB.
//
// Layout under the root directory:
//
//	bars/underlying=NIFTY/date=2026-11-06.parquet   1-minute bars, one file per day
//	contracts/underlying=NIFTY.parquet              contract master
//
// Writes merge with any existing file and replace it atomically, so a
// re-download is idempotent and a crash cannot leave a half-written file.
package data

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"

	"serene/internal/market"
)

// Store is a Parquet market data store.
type Store struct {
	root string
	db   *sql.DB
}

// Open opens a store rooted at dir, creating it if needed.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("open duckdb: %w", err)
	}
	// Temp tables belong to a connection, so everything shares one.
	db.SetMaxOpenConns(1)
	return &Store{root: dir, db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

const barColumns = `key VARCHAR, ts TIMESTAMP, open BIGINT, high BIGINT, low BIGINT, close BIGINT, volume BIGINT, oi BIGINT`

var safeName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func (s *Store) barPath(underlying string, d market.Date) string {
	return filepath.Join(s.root, "bars", "underlying="+underlying, "date="+d.String()+".parquet")
}

// quote renders a SQL string literal. DuckDB's COPY and read_parquet take
// paths as literals rather than bind parameters.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// PutBars merges 1-minute bars for an underlying. A bar with the same key and
// time as a stored one replaces it.
func (s *Store) PutBars(ctx context.Context, underlying string, bars []market.Bar) error {
	if !safeName.MatchString(underlying) {
		return fmt.Errorf("invalid underlying %q", underlying)
	}
	byDay := map[market.Date][]market.Bar{}
	for _, b := range bars {
		if b.Timeframe != market.Minute1 {
			return fmt.Errorf("store holds 1m bars only, got %s for %s", b.Timeframe, b.Key)
		}
		byDay[market.DateOf(b.Time)] = append(byDay[market.DateOf(b.Time)], b)
	}
	for d, dayBars := range byDay {
		if err := s.mergeDay(ctx, s.barPath(underlying, d), dayBars); err != nil {
			return fmt.Errorf("write %s %s: %w", underlying, d, err)
		}
	}
	return nil
}

func (s *Store) mergeDay(ctx context.Context, path string, bars []market.Bar) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `CREATE OR REPLACE TEMP TABLE incoming (`+barColumns+`)`); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `DROP TABLE IF EXISTS incoming`)
	if err := appendBars(conn, bars); err != nil {
		return err
	}

	// Incoming rows sort first so they win the dedupe.
	source := `SELECT 0 AS src, * FROM incoming`
	if _, err := os.Stat(path); err == nil {
		source += ` UNION ALL SELECT 1 AS src, * FROM read_parquet(` + quote(path) + `, hive_partitioning = false)`
	}
	tmp := path + ".tmp"
	defer os.Remove(tmp)
	q := `COPY (
		SELECT key, ts, open, high, low, close, volume, oi FROM (` + source + `)
		QUALIFY row_number() OVER (PARTITION BY key, ts ORDER BY src) = 1
		ORDER BY key, ts
	) TO ` + quote(tmp) + ` (FORMAT parquet, COMPRESSION zstd)`
	if _, err := conn.ExecContext(ctx, q); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func appendBars(conn *sql.Conn, bars []market.Bar) error {
	return conn.Raw(func(dc any) error {
		a, err := duckdb.NewAppenderFromConn(dc.(driver.Conn), "", "incoming")
		if err != nil {
			return err
		}
		for _, b := range bars {
			err := a.AppendRow(b.Key, b.Time.UTC(), int64(b.Open), int64(b.High), int64(b.Low),
				int64(b.Close), b.Volume, b.OI)
			if err != nil {
				a.Close()
				return err
			}
		}
		return a.Close()
	})
}

// Days lists the stored dates for an underlying within [from, to], ascending.
func (s *Store) Days(underlying string, from, to market.Date) ([]market.Date, error) {
	matches, err := filepath.Glob(filepath.Join(s.root, "bars", "underlying="+underlying, "date=*.parquet"))
	if err != nil {
		return nil, err
	}
	var days []market.Date
	for _, m := range matches {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "date="), ".parquet")
		d, err := market.ParseDate(name)
		if err != nil {
			return nil, err
		}
		if !d.Before(from) && !to.Before(d) {
			days = append(days, d)
		}
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	return days, nil
}

// DayBars returns one day's 1-minute bars for an underlying, ordered by time
// then key. Replay depends on that order being deterministic.
func (s *Store) DayBars(ctx context.Context, underlying string, d market.Date) ([]market.Bar, error) {
	path := s.barPath(underlying, d)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no data for %s on %s: %w", underlying, d, err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key, ts, open, high, low, close, volume, oi
		FROM read_parquet(`+quote(path)+`, hive_partitioning = false) ORDER BY ts, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []market.Bar
	for rows.Next() {
		var (
			b                   market.Bar
			ts                  time.Time
			open, high, low, cl int64
		)
		if err := rows.Scan(&b.Key, &ts, &open, &high, &low, &cl, &b.Volume, &b.OI); err != nil {
			return nil, err
		}
		b.Timeframe = market.Minute1
		b.Time = ts.In(market.IST)
		b.End = b.Time.Add(time.Minute)
		b.Open, b.High, b.Low, b.Close = market.Paise(open), market.Paise(high), market.Paise(low), market.Paise(cl)
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) contractPath(underlying string) string {
	return filepath.Join(s.root, "contracts", "underlying="+underlying+".parquet")
}

// PutContracts merges contracts into the contract master for an underlying.
func (s *Store) PutContracts(ctx context.Context, underlying string, cs []market.Instrument) error {
	if !safeName.MatchString(underlying) {
		return fmt.Errorf("invalid underlying %q", underlying)
	}
	path := s.contractPath(underlying)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `CREATE OR REPLACE TEMP TABLE incoming_contracts (key VARCHAR, lot_size BIGINT)`); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `DROP TABLE IF EXISTS incoming_contracts`)
	err = conn.Raw(func(dc any) error {
		a, err := duckdb.NewAppenderFromConn(dc.(driver.Conn), "", "incoming_contracts")
		if err != nil {
			return err
		}
		for _, c := range cs {
			if c.LotSize <= 0 {
				a.Close()
				return fmt.Errorf("contract %s has no lot size", c.Key())
			}
			if err := a.AppendRow(c.Key(), c.LotSize); err != nil {
				a.Close()
				return err
			}
		}
		return a.Close()
	})
	if err != nil {
		return err
	}
	source := `SELECT 0 AS src, * FROM incoming_contracts`
	if _, err := os.Stat(path); err == nil {
		source += ` UNION ALL SELECT 1 AS src, * FROM read_parquet(` + quote(path) + `, hive_partitioning = false)`
	}
	tmp := path + ".tmp"
	defer os.Remove(tmp)
	q := `COPY (SELECT key, lot_size FROM (` + source + `)
		QUALIFY row_number() OVER (PARTITION BY key ORDER BY src) = 1 ORDER BY key
	) TO ` + quote(tmp) + ` (FORMAT parquet, COMPRESSION zstd)`
	if _, err := conn.ExecContext(ctx, q); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Contracts returns the contract master for an underlying.
func (s *Store) Contracts(ctx context.Context, underlying string) ([]market.Instrument, error) {
	path := s.contractPath(underlying)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no contracts for %s: %w", underlying, err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key, lot_size FROM read_parquet(`+quote(path)+`, hive_partitioning = false) ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []market.Instrument
	for rows.Next() {
		var key string
		var lot int64
		if err := rows.Scan(&key, &lot); err != nil {
			return nil, err
		}
		inst, err := market.ParseKey(key)
		if err != nil {
			return nil, err
		}
		inst.LotSize = lot
		out = append(out, inst)
	}
	return out, rows.Err()
}

// DayStat summarises one stored day.
type DayStat struct {
	Date      string
	Contracts int // distinct instruments, excluding the index
	Bars      int
	IndexBars int
}

// DayStats summarises every stored day of an underlying.
func (s *Store) DayStats(ctx context.Context, underlying, indexKey string) ([]DayStat, error) {
	if !safeName.MatchString(underlying) {
		return nil, fmt.Errorf("invalid underlying %q", underlying)
	}
	glob := filepath.Join(s.root, "bars", "underlying="+underlying, "date=*.parquet")
	rows, err := s.db.QueryContext(ctx, `SELECT regexp_extract(filename, 'date=([0-9-]+)', 1) AS day,
			count(DISTINCT key) FILTER (WHERE key <> $1), count(*), count(*) FILTER (WHERE key = $1)
		FROM read_parquet(`+quote(glob)+`, hive_partitioning = false, filename = true)
		GROUP BY day ORDER BY day`, indexKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DayStat
	for rows.Next() {
		var d DayStat
		if err := rows.Scan(&d.Date, &d.Contracts, &d.Bars, &d.IndexBars); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
