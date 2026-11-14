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

	"serene/internal/market"
)

// Reference tables beside the bars, used by the Fyers ingest:
//
//	expiries/underlying=NIFTY.parquet   every expiry the broker lists (kind, expiry)
//	lots/underlying=NIFTY.parquet       lot size from a date onward (from_date, lot_size, source)
//	probe.parquet                       what a probe of the broker's history found

// mergeTable merges rows into the Parquet file at rel, keeping the incoming row
// when its key columns match a stored one. columns is a DDL column list.
func (s *Store) mergeTable(ctx context.Context, rel, columns string, keys []string, rows [][]driver.Value) error {
	path := filepath.Join(s.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `CREATE OR REPLACE TEMP TABLE incoming_rows (`+columns+`)`); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `DROP TABLE IF EXISTS incoming_rows`)
	err = conn.Raw(func(dc any) error {
		a, err := duckdb.NewAppenderFromConn(dc.(driver.Conn), "", "incoming_rows")
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := a.AppendRow(r...); err != nil {
				a.Close()
				return err
			}
		}
		return a.Close()
	})
	if err != nil {
		return err
	}
	var names []string
	for _, c := range strings.Split(columns, ",") {
		names = append(names, strings.Fields(c)[0])
	}
	cols := strings.Join(names, ", ")
	source := `SELECT 0 AS src, ` + cols + ` FROM incoming_rows`
	if _, err := os.Stat(path); err == nil {
		source += ` UNION ALL SELECT 1 AS src, ` + cols + ` FROM read_parquet(` + quote(path) + `, hive_partitioning = false)`
	}
	key := strings.Join(keys, ", ")
	tmp := path + ".tmp"
	defer os.Remove(tmp)
	q := `COPY (SELECT ` + cols + ` FROM (` + source + `)
		QUALIFY row_number() OVER (PARTITION BY ` + key + ` ORDER BY src) = 1 ORDER BY ` + key + `
	) TO ` + quote(tmp) + ` (FORMAT parquet, COMPRESSION zstd)`
	if _, err := conn.ExecContext(ctx, q); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func dateOfDay(t time.Time) market.Date {
	return market.Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}
}

// Expiry kinds.
const (
	ExpiryOption = "option"
	ExpiryFuture = "future"
)

// PutExpiries merges listed expiry dates of one kind for an underlying.
func (s *Store) PutExpiries(ctx context.Context, underlying, kind string, dates []market.Date) error {
	if !safeName.MatchString(underlying) {
		return fmt.Errorf("invalid underlying %q", underlying)
	}
	rows := make([][]driver.Value, len(dates))
	for i, d := range dates {
		rows[i] = []driver.Value{kind, utcDay(d)}
	}
	return s.mergeTable(ctx, filepath.Join("expiries", "underlying="+underlying+".parquet"),
		`kind VARCHAR, expiry DATE`, []string{"kind", "expiry"}, rows)
}

// StoredExpiries returns an underlying's stored expiries of one kind, ascending.
func (s *Store) StoredExpiries(ctx context.Context, underlying, kind string) ([]market.Date, error) {
	if !safeName.MatchString(underlying) {
		return nil, fmt.Errorf("invalid underlying %q", underlying)
	}
	path := filepath.Join(s.root, "expiries", "underlying="+underlying+".parquet")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT expiry FROM read_parquet(`+quote(path)+`, hive_partitioning = false)
		WHERE kind = $1 ORDER BY expiry`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []market.Date
	for rows.Next() {
		var t time.Time
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, dateOfDay(t))
	}
	return out, rows.Err()
}

// Lot is an underlying's lot size for contracts expiring on or after From.
type Lot struct {
	From   market.Date
	Size   int64
	Source string // where the size came from, such as upstox, symbol_master or manual
}

// PutLots merges lot sizes for an underlying. A row with the same date replaces
// the stored one.
func (s *Store) PutLots(ctx context.Context, underlying string, lots []Lot) error {
	if !safeName.MatchString(underlying) {
		return fmt.Errorf("invalid underlying %q", underlying)
	}
	rows := make([][]driver.Value, len(lots))
	for i, l := range lots {
		if l.Size <= 0 {
			return fmt.Errorf("lot size %d for %s from %s", l.Size, underlying, l.From)
		}
		rows[i] = []driver.Value{utcDay(l.From), l.Size, l.Source}
	}
	return s.mergeTable(ctx, filepath.Join("lots", "underlying="+underlying+".parquet"),
		`from_date DATE, lot_size BIGINT, source VARCHAR`, []string{"from_date"}, rows)
}

// Lots returns an underlying's lot sizes, oldest first.
func (s *Store) Lots(ctx context.Context, underlying string) ([]Lot, error) {
	if !safeName.MatchString(underlying) {
		return nil, fmt.Errorf("invalid underlying %q", underlying)
	}
	path := filepath.Join(s.root, "lots", "underlying="+underlying+".parquet")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT from_date, lot_size, source
		FROM read_parquet(`+quote(path)+`, hive_partitioning = false) ORDER BY from_date`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Lot
	for rows.Next() {
		var t time.Time
		var l Lot
		if err := rows.Scan(&t, &l.Size, &l.Source); err != nil {
			return nil, err
		}
		l.From = dateOfDay(t)
		out = append(out, l)
	}
	return out, rows.Err()
}

// LotOn returns the lot size in force for an expiry: the latest row on or
// before it. ok is false when no row covers the date.
func LotOn(lots []Lot, expiry market.Date) (int64, bool) {
	var size int64
	for _, l := range lots {
		if expiry.Before(l.From) {
			break
		}
		size = l.Size
	}
	return size, size > 0
}

// ProbeRow is one finding of a probe of a broker's history.
type ProbeRow struct {
	Check  string // e.g. earliest_minute
	Symbol string
	Value  string
	Note   string
	At     time.Time
}

// PutProbe merges probe findings, keyed by check and symbol.
func (s *Store) PutProbe(ctx context.Context, rows []ProbeRow) error {
	out := make([][]driver.Value, len(rows))
	for i, r := range rows {
		out[i] = []driver.Value{r.Check, r.Symbol, r.Value, r.Note, r.At.UTC()}
	}
	return s.mergeTable(ctx, "probe.parquet",
		`check_name VARCHAR, symbol VARCHAR, value VARCHAR, note VARCHAR, checked_at TIMESTAMP`,
		[]string{"check_name", "symbol"}, out)
}
