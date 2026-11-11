package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"serene/internal/data"
	"serene/internal/market"
)

// ExpiryCoverage is one expiry's download result.
type ExpiryCoverage struct {
	Expiry   string
	Weekly   bool
	LotSize  int64
	Listed   int
	Selected int
	Empty    int
	Thin     int // contracts with under a day's bars
	Bars     int
	Dropped  int
}

// Coverage reports what a download got and what it didn't.
type Coverage struct {
	Expiries []ExpiryCoverage
	Gaps     []string // missing stretches between expiries
	Empty    []string // expiries with contracts listed but no bars at all
	Days     []data.DayStat
	NoIndex  []string // days with options but no index bars
	Short    []string // days with fewer index bars than a full session
}

// fullSessionBars is 09:15 to 15:30, or 15:29 since the closing auction, as the
// index still stops then.
const fullSessionBars = 375

// Report builds the coverage report.
func Report(ctx context.Context, dir, underlying string, store *data.Store) (Coverage, error) {
	var out Coverage
	cfg := Config{Dir: dir, Underlying: underlying}
	paths, err := filepath.Glob(filepath.Join(manifestDir(cfg), "*.json"))
	if err != nil {
		return out, err
	}
	var dates []market.Date
	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), ".json")
		if name == "index" {
			continue
		}
		var rec ExpiryRecord
		if _, err := readJSON(p, &rec); err != nil {
			return out, fmt.Errorf("%s: %w", p, err)
		}
		e := ExpiryCoverage{Expiry: rec.Expiry, Weekly: rec.Weekly, LotSize: rec.LotSize, Listed: rec.Listed, Selected: len(rec.Contracts)}
		for _, c := range rec.Contracts {
			e.Bars += c.Bars
			e.Dropped += c.Dropped
			if c.Bars == 0 {
				e.Empty++
			} else if c.Bars < 100 {
				e.Thin++
			}
		}
		out.Expiries = append(out.Expiries, e)
		if e.Selected > 0 && e.Bars == 0 {
			out.Empty = append(out.Empty, e.Expiry)
		}
		if d, err := market.ParseDate(rec.Expiry); err == nil {
			dates = append(dates, d)
		}
	}
	sort.Slice(out.Expiries, func(i, j int) bool { return out.Expiries[i].Expiry < out.Expiries[j].Expiry })
	sort.Strings(out.Empty)
	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
	for i := 1; i < len(dates); i++ {
		// Holidays can move an expiry a day or two, but not a whole week.
		if gap := int(dates[i].At(0, 0).Sub(dates[i-1].At(0, 0)).Hours() / 24); gap > 9 {
			out.Gaps = append(out.Gaps, fmt.Sprintf("%s to %s (%d days)", dates[i-1], dates[i], gap))
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "bars", "underlying="+underlying)); err != nil {
		return out, nil
	}
	out.Days, err = store.DayStats(ctx, underlying, market.Index(market.NSE, underlying).Key())
	if err != nil {
		return out, err
	}
	for _, d := range out.Days {
		switch {
		case d.Contracts > 0 && d.IndexBars == 0:
			out.NoIndex = append(out.NoIndex, d.Date)
		case d.IndexBars > 0 && d.IndexBars < fullSessionBars:
			out.Short = append(out.Short, fmt.Sprintf("%s (%d)", d.Date, d.IndexBars))
		}
	}
	return out, nil
}
