package bhav

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"calm/internal/data"
	"calm/internal/market"
)

const (
	// recheckDays is how many of the newest files are compared with the source
	// on each run, because NSE sometimes rewrites a published file.
	recheckDays = 3
	// settleDays is how old a date must be before a 404 is remembered. Newer
	// ones may just not be published yet.
	settleDays = 3
)

// Config is one sync.
type Config struct {
	Dir      string // raw files in Dir/raw/YYYY, known gaps in Dir/fetch/missing.json
	From, To market.Date
	Log      func(format string, args ...any)
}

// Result reports what a download changed.
type Result struct {
	New     []market.Date
	Changed []market.Date // published files that differ from our copy
}

func rawPath(dir string, d market.Date) string {
	return filepath.Join(dir, "raw", fmt.Sprint(d.Year), d.String()+".csv.zip")
}

// RawDates lists the stored raw files, ascending.
func RawDates(dir string) ([]market.Date, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "raw", "*", "*.csv.zip"))
	if err != nil {
		return nil, err
	}
	out := make([]market.Date, 0, len(matches))
	for _, m := range matches {
		d, err := market.ParseDate(strings.TrimSuffix(filepath.Base(m), ".csv.zip"))
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out, nil
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func missingPath(dir string) string { return filepath.Join(dir, "fetch", "missing.json") }

func loadMissing(dir string) (map[string]bool, error) {
	out := map[string]bool{}
	b, err := os.ReadFile(missingPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	for _, d := range list {
		out[d] = true
	}
	return out, nil
}

func saveMissing(dir string, m map[string]bool) error {
	list := make([]string, 0, len(m))
	for d := range m {
		list = append(list, d)
	}
	sort.Strings(list)
	b, err := json.MarshalIndent(list, "", " ")
	if err != nil {
		return err
	}
	return writeAtomic(missingPath(dir), b)
}

// sameCSV compares two zips by content, since a re-zipped file with the same
// rows carries a new timestamp.
func sameCSV(a, b []byte) bool {
	x, err1 := Unzip(a)
	y, err2 := Unzip(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// Download fetches every day in range that isn't stored or known to have no
// file, and rechecks the newest files. It stops at the first real failure, and
// what it has saved stays, so the same command resumes. Weekends are probed
// because NSE holds rare special sessions on them.
func Download(ctx context.Context, cfg Config, c *Client) (Result, error) {
	var res Result
	have, err := RawDates(cfg.Dir)
	if err != nil {
		return res, err
	}
	stored := map[market.Date]bool{}
	for _, d := range have {
		stored[d] = true
	}
	missing, err := loadMissing(cfg.Dir)
	if err != nil {
		return res, err
	}
	defer saveMissing(cfg.Dir, missing)

	for _, d := range have[max(0, len(have)-recheckDays):] {
		body, err := c.Fetch(ctx, d)
		if err != nil {
			return res, fmt.Errorf("recheck %s: %w", d, err)
		}
		old, err := os.ReadFile(rawPath(cfg.Dir, d))
		if err != nil {
			return res, err
		}
		if !sameCSV(old, body) {
			if err := writeAtomic(rawPath(cfg.Dir, d), body); err != nil {
				return res, err
			}
			res.Changed = append(res.Changed, d)
			cfg.Log("%s: file changed at source", d)
		}
	}

	settled := market.DateOf(time.Now()).AddDays(-settleDays)
	checked := 0
	for d := cfg.From; !cfg.To.Before(d); d = d.AddDays(1) {
		if stored[d] || missing[d.String()] {
			continue
		}
		body, err := c.Fetch(ctx, d)
		if errors.Is(err, ErrNotPublished) {
			if d.Before(settled) {
				missing[d.String()] = true
			}
		} else if err != nil {
			return res, fmt.Errorf("%s: %w", d, err)
		} else {
			if _, err := Unzip(body); err != nil {
				return res, fmt.Errorf("%s: %w", d, err)
			}
			if err := writeAtomic(rawPath(cfg.Dir, d), body); err != nil {
				return res, err
			}
			res.New = append(res.New, d)
		}
		if checked++; checked%200 == 0 {
			cfg.Log("up to %s: %d new files, %d requests", d, len(res.New), c.Requests)
			if err := saveMissing(cfg.Dir, missing); err != nil {
				return res, err
			}
		}
	}
	cfg.Log("finished: %d new, %d changed, %d requests", len(res.New), len(res.Changed), c.Requests)
	return res, nil
}

// Years returns the distinct years of some dates.
func Years(dates []market.Date) []int {
	seen := map[int]bool{}
	var out []int
	for _, d := range dates {
		if !seen[d.Year] {
			seen[d.Year] = true
			out = append(out, d.Year)
		}
	}
	sort.Ints(out)
	return out
}

// Rebuild re-parses the raw files of some years into the store.
func Rebuild(ctx context.Context, dir string, store *data.Store, years []int) error {
	all, err := RawDates(dir)
	if err != nil {
		return err
	}
	byYear := map[int][]market.Date{}
	for _, d := range all {
		byYear[d.Year] = append(byYear[d.Year], d)
	}
	for _, y := range years {
		var rows []market.EquityDay
		for _, d := range byYear[y] {
			raw, err := os.ReadFile(rawPath(dir, d))
			if err != nil {
				return err
			}
			body, err := Unzip(raw)
			if err != nil {
				return fmt.Errorf("%s: %w", d, err)
			}
			day, err := Parse(body, d)
			if err != nil {
				return fmt.Errorf("%s: %w", d, err)
			}
			rows = append(rows, day...)
		}
		if err := store.ReplaceEquityYear(ctx, y, rows); err != nil {
			return fmt.Errorf("write %d: %w", y, err)
		}
	}
	return nil
}

// Unbuilt returns the years with raw files but no store file, as left by a run
// that stopped before its rebuild.
func Unbuilt(dir string) ([]int, error) {
	dates, err := RawDates(dir)
	if err != nil {
		return nil, err
	}
	var out []int
	for _, y := range Years(dates) {
		if _, err := os.Stat(filepath.Join(dir, "stocks", fmt.Sprintf("year=%d.parquet", y))); errors.Is(err, os.ErrNotExist) {
			out = append(out, y)
		}
	}
	return out, nil
}
