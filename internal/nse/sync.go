package nse

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

	"serene/internal/data"
	"serene/internal/market"
)

const (
	// recheckDays is how many of the newest files are compared with the source
	// on each run, because NSE sometimes rewrites a published file.
	recheckDays = 5
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
	return filepath.Join(dir, "raw", fmt.Sprint(d.Year), "ind_close_all_"+d.String()+".csv")
}

// RawDates lists the stored raw files, ascending.
func RawDates(dir string) ([]market.Date, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "raw", "*", "ind_close_all_*.csv"))
	if err != nil {
		return nil, err
	}
	out := make([]market.Date, 0, len(matches))
	for _, m := range matches {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "ind_close_all_"), ".csv")
		d, err := market.ParseDate(name)
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

func weekend(d market.Date) bool {
	w := d.Weekday()
	return w == time.Saturday || w == time.Sunday
}

// Download fetches every day in range that isn't stored or known to have no
// file, and rechecks the newest files. It stops at the first real failure, and
// what it has saved stays, so the same command resumes. Weekends are probed
// because exchanges hold rare special sessions on them.
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
		body, err := c.Fetch(ctx, d, false)
		if err != nil {
			return res, fmt.Errorf("recheck %s: %w", d, err)
		}
		old, err := os.ReadFile(rawPath(cfg.Dir, d))
		if err != nil {
			return res, err
		}
		if !bytes.Equal(old, body) {
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
		body, err := c.Fetch(ctx, d, weekend(d))
		if errors.Is(err, ErrNotPublished) {
			if d.Before(settled) {
				missing[d.String()] = true
			}
		} else if err != nil {
			return res, fmt.Errorf("%s: %w", d, err)
		} else {
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

// Rebuild re-parses the raw files of some years into the store and returns
// every published name the catalog doesn't know.
func Rebuild(ctx context.Context, dir string, cat *Catalog, store *data.Store, years []int) ([]string, error) {
	all, err := RawDates(dir)
	if err != nil {
		return nil, err
	}
	byYear := map[int][]market.Date{}
	for _, d := range all {
		byYear[d.Year] = append(byYear[d.Year], d)
	}
	unknown := map[string]bool{}
	for _, y := range years {
		var rows []market.IndexDay
		for _, d := range byYear[y] {
			raw, err := os.ReadFile(rawPath(dir, d))
			if err != nil {
				return nil, err
			}
			day, err := Parse(raw, d)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", d, err)
			}
			kept, names, err := cat.Apply(day)
			if err != nil {
				return nil, err
			}
			for _, n := range names {
				unknown[n] = true
			}
			rows = append(rows, kept...)
		}
		if err := store.ReplaceIndexYear(ctx, y, rows); err != nil {
			return nil, fmt.Errorf("write %d: %w", y, err)
		}
	}
	out := make([]string, 0, len(unknown))
	for n := range unknown {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// ExpectedDate is the newest close a morning run should find: the last weekday
// before today. It may be a holiday, which only the missing file can reveal.
func ExpectedDate(today market.Date) market.Date {
	d := today.AddDays(-1)
	for weekend(d) {
		d = d.AddDays(-1)
	}
	return d
}

// Sent records what the daily job has told you, so a repeat run stays quiet.
type Sent struct {
	Reported string `json:"reported"` // last close whose summary was sent
	Attached string `json:"attached"` // last close whose report file was sent
	Noted    string `json:"noted"`    // last date noted as having no file
}

// Step is what the daily job still owes for a close.
type Step int

const (
	SendAll    Step = iota // summary and file
	AttachOnly             // the summary went but the file failed
	Done
)

// Next says what remains for a close. Tracking the two sends apart means a
// failed upload is retried without repeating the message.
func (s Sent) Next(close market.Date) Step {
	switch d := close.String(); {
	case s.Reported != d:
		return SendAll
	case s.Attached != d:
		return AttachOnly
	}
	return Done
}

func sentPath(dir string) string { return filepath.Join(dir, "fetch", "sent.json") }

// LoadSent reads the record, which is empty on the first run.
func LoadSent(dir string) (Sent, error) {
	var s Sent
	b, err := os.ReadFile(sentPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// SaveSent stores the record.
func SaveSent(dir string, s Sent) error {
	b, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	return writeAtomic(sentPath(dir), b)
}

// Break is a day where an index's published previous close disagrees with the
// stored close before it, which means a wrong alias or a gap in the data.
type Break struct {
	Name         string
	Date         market.Date
	From, To     string // published names on the two days
	Stored, Said market.Paise
	Gap          bool // the index skipped at least one trading day
}

// Continuity checks every index in rows, which must be ordered by date.
// Differences within tolerance percent are ignored, as NSE rounds.
func Continuity(rows []market.IndexDay, calendar []market.Date, tolerance float64) []Break {
	pos := make(map[market.Date]int, len(calendar))
	for i, d := range calendar {
		pos[d] = i
	}
	last := map[string]market.IndexDay{}
	var out []Break
	for _, r := range rows {
		p, ok := last[r.Name]
		last[r.Name] = r
		if !ok || r.PrevClose <= 0 {
			continue
		}
		diff := float64(r.PrevClose-p.Close) / float64(p.Close) * 100
		if diff < 0 {
			diff = -diff
		}
		if diff > tolerance {
			out = append(out, Break{r.Name, r.Date, p.Source, r.Source, p.Close, r.PrevClose, pos[r.Date]-pos[p.Date] > 1})
		}
	}
	return out
}
