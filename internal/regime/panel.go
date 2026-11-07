// Package regime turns stored index history into a sector view and a market
// regime. Everything here is a pure function of the panel, so a report can be
// reproduced for any past date.
//
// The rules are fixed in advance rather than fitted: a handful of bear markets
// can't validate thresholds statistically. Signals built on moving averages
// confirm after a turn, so a regime describes the market and doesn't predict it.
package regime

import (
	"math"
	"sort"

	"calm/internal/market"
)

// Meta is what the catalog knows about an index.
type Meta struct {
	Group   string
	Breadth bool
}

// maxGap is the longest run of missing trading days a series may carry forward.
// A longer gap restarts the series. NSE's 2016-07-07 and 2023-04-12 files hold
// isolated back-calculated rows for indices launched years later, and carrying
// those forward would invent a flat history.
const maxGap = 10

// rebasePct is how far a published previous close must differ from the stored
// close to count as a rebase. A missing day's file moves it far less.
const rebasePct = 10.0

// Rebase is a day NSE changed an index's level. Nifty High Beta 50 doubled this
// way on 2014-08-04, with no real move.
type Rebase struct {
	Date  market.Date
	Ratio float64 // published previous close over stored close
}

// Series is one index aligned to the panel's dates. Close is NaN before the
// start of its current run of records, and carries forward over short gaps.
// Other fields are NaN or 0 on days with no record.
type Series struct {
	Name   string
	Meta   Meta
	First  int // index of the first date of the current run
	Close  []float64
	Volume []float64
	PE     []float64

	Rebases []Rebase
	prev    []float64 // published previous close
}

// Panel holds every index on a common calendar.
type Panel struct {
	Dates  []market.Date
	Series map[string]*Series
}

// NewPanel aligns stored rows, which may be in any order. meta describes a
// canonical name.
func NewPanel(rows []market.IndexDay, meta func(string) Meta) *Panel {
	seen := map[market.Date]bool{}
	var dates []market.Date
	for _, r := range rows {
		if !seen[r.Date] {
			seen[r.Date] = true
			dates = append(dates, r.Date)
		}
	}
	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
	pos := make(map[market.Date]int, len(dates))
	for i, d := range dates {
		pos[d] = i
	}
	p := &Panel{Dates: dates, Series: map[string]*Series{}}
	for _, r := range rows {
		s, ok := p.Series[r.Name]
		if !ok {
			s = &Series{Name: r.Name, Meta: meta(r.Name), First: len(dates), Close: nans(len(dates)),
				Volume: make([]float64, len(dates)), PE: nans(len(dates)), prev: make([]float64, len(dates))}
			p.Series[r.Name] = s
		}
		i := pos[r.Date]
		s.First = min(s.First, i)
		s.Close[i] = r.Close.Rupees()
		s.Volume[i] = float64(r.Volume)
		s.PE[i] = r.PE
		s.prev[i] = r.PrevClose.Rupees()
	}
	for _, s := range p.Series {
		s.settle()
		// VIX's published previous close doesn't follow its own close, and bond
		// indices don't rebase, so only equity groups are adjusted.
		if s.Meta.Group != "other" {
			s.adjust(dates)
		}
		s.fill()
	}
	return p
}

// settle drops everything before the last long gap.
func (s *Series) settle() {
	gap := 0
	for i := s.First; i < len(s.Close); i++ {
		if math.IsNaN(s.Close[i]) {
			gap++
			continue
		}
		if gap > maxGap {
			for j := s.First; j < i; j++ {
				s.Close[j], s.Volume[j], s.PE[j] = math.NaN(), 0, math.NaN()
			}
			s.First = i
		}
		gap = 0
	}
}

// adjust scales history before each rebase, working back from today so that
// the latest close stays as published.
func (s *Series) adjust(dates []market.Date) {
	factor := 1.0
	next := -1
	for i := len(s.Close) - 1; i >= s.First; i-- {
		raw := s.Close[i]
		if math.IsNaN(raw) {
			continue
		}
		if next >= 0 && s.prev[next] > 0 {
			if r := s.prev[next] / raw; math.Abs(r-1)*100 > rebasePct {
				factor *= r
				s.Rebases = append(s.Rebases, Rebase{dates[next], r})
			}
		}
		s.Close[i] = raw * factor
		next = i
	}
}

// fill carries closes over the short gaps that remain.
func (s *Series) fill() {
	for i := s.First + 1; i < len(s.Close); i++ {
		if math.IsNaN(s.Close[i]) {
			s.Close[i] = s.Close[i-1]
		}
	}
}

func nans(n int) []float64 {
	v := make([]float64, n)
	for i := range v {
		v[i] = math.NaN()
	}
	return v
}

// sma returns the n-day simple average, NaN until n values exist.
func sma(x []float64, n int) []float64 {
	out := nans(len(x))
	sum, count := 0.0, 0
	for i, v := range x {
		if !math.IsNaN(v) {
			sum += v
			count++
		}
		if i >= n && !math.IsNaN(x[i-n]) {
			sum -= x[i-n]
			count--
		}
		if count == n {
			out[i] = sum / float64(n)
		}
	}
	return out
}

// ratio divides two aligned series.
func ratio(a, b []float64) []float64 {
	out := nans(len(a))
	for i := range a {
		if b[i] != 0 {
			out[i] = a[i] / b[i] // NaN carries through
		}
	}
	return out
}

// drawdown returns each close's fall from the highest close of the last n days,
// as a negative percentage.
func drawdown(x []float64, n int) []float64 {
	out := nans(len(x))
	var q []int // indices with falling closes, so q[0] is the window's high
	for i, v := range x {
		if math.IsNaN(v) {
			continue
		}
		for len(q) > 0 && x[q[len(q)-1]] <= v {
			q = q[:len(q)-1]
		}
		q = append(q, i)
		for q[0] <= i-n {
			q = q[1:]
		}
		out[i] = (v/x[q[0]] - 1) * 100
	}
	return out
}

func stddev(x []float64) float64 {
	if len(x) < 2 {
		return math.NaN()
	}
	mean := 0.0
	for _, v := range x {
		mean += v
	}
	mean /= float64(len(x))
	sum := 0.0
	for _, v := range x {
		sum += (v - mean) * (v - mean)
	}
	return math.Sqrt(sum / float64(len(x)-1))
}
