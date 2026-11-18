package regime

import (
	"math"
	"sort"
)

// Leaders follows the idea that the sectors and stocks still making all-time
// highs while the broad market corrects lead the next rally. It is a screen,
// not a tested signal; the thresholds are fixed in advance.
const (
	LeaderBench      = "NIFTY Smallcap 100"
	LeaderNearHigh   = 5.0 // % below the all-time high a leader may be
	leaderMinPeakAge = 21  // a benchmark peak younger than this gives no "since peak" window
	leaderFallback   = 126 // the window used instead, in trading days
	newHighDays      = 20  // an all-time high this recent counts as new
)

// LeaderWindow is the stretch strength is measured over, as panel indices.
type LeaderWindow struct {
	From, To  int
	SincePeak bool // From is the benchmark's all-time high; else the fallback
}

// WindowFor measures from the benchmark's all-time high up to i, or over the
// last leaderFallback days when that high is under leaderMinPeakAge days old.
func WindowFor(bench []float64, first, i int) LeaderWindow {
	peak := first
	for j := first; j <= i; j++ {
		if bench[j] >= bench[peak] {
			peak = j
		}
	}
	if i-peak >= leaderMinPeakAge {
		return LeaderWindow{From: peak, To: i, SincePeak: true}
	}
	return LeaderWindow{From: max(first, i-leaderFallback), To: i}
}

// LeaderRow is one index or stock on the report date. Returns are percent;
// a number is NaN when its history is too short.
type LeaderRow struct {
	Name, Group string
	First       int // index of its first close
	Close       float64
	FromATH     float64 // % below the highest close since First
	From1Y      float64 // % below the 1-year high close
	ATHAge      int     // trading days since that all-time high
	NewHigh     bool    // an all-time high within newHighDays
	Window      float64 // return over the window
	Excess      float64 // Window minus the benchmark's
	RSHigh      bool    // ratio to the benchmark at its 1-year high
	R3M, R6M    float64
	Leader      bool // near its all-time high and ahead of the benchmark
}

// LeaderStats describes close, aligned with bench and starting at first, on
// day i. Closes after first must be filled, without NaN.
func LeaderStats(name string, close, bench []float64, first, i int, w LeaderWindow) LeaderRow {
	r := LeaderRow{Name: name, First: first, Close: close[i], From1Y: math.NaN(), Window: math.NaN(),
		Excess: math.NaN(), R3M: math.NaN(), R6M: math.NaN()}
	ath := first
	for j := first; j <= i; j++ {
		if close[j] >= close[ath] {
			ath = j
		}
	}
	r.FromATH = (close[i]/close[ath] - 1) * 100
	r.ATHAge = i - ath
	r.NewHigh = r.ATHAge < newHighDays
	if i-first >= highDays {
		r.From1Y = drawdown(close[i-highDays+1:i+1], highDays)[highDays-1]
	}
	ret := func(j int) float64 {
		if j < first {
			return math.NaN()
		}
		return (close[i]/close[j] - 1) * 100
	}
	r.R3M, r.R6M = ret(i-63), ret(i-126)
	if w.From >= first {
		r.Window = ret(w.From)
		r.Excess = r.Window - (bench[i]/bench[w.From]-1)*100
	}
	if i-first >= highDays {
		rs := ratio(close[i-highDays+1:i+1], bench[i-highDays+1:i+1])
		r.RSHigh = drawdown(rs, highDays)[highDays-1] >= 0
	}
	r.Leader = r.FromATH >= -LeaderNearHigh && r.Excess > 0
	return r
}

// LeaderReport is the sector screen on one day.
type LeaderReport struct {
	Window  LeaderWindow
	Bench   []LeaderRow // the benchmark, then the market
	Sectors []LeaderRow // best excess first
}

// Leaders screens every index in groups against the benchmark on day i.
func Leaders(p *Panel, i int, bench string, groups ...string) (LeaderReport, bool) {
	b, ok := p.Series[bench]
	if !ok || i < b.First || math.IsNaN(b.Close[i]) {
		return LeaderReport{}, false
	}
	rep := LeaderReport{Window: WindowFor(b.Close, b.First, i)}
	for _, name := range []string{bench, Market} {
		if s, ok := p.Series[name]; ok && i >= s.First {
			rep.Bench = append(rep.Bench, LeaderStats(name, s.Close, b.Close, s.First, i, rep.Window))
		}
	}
	want := map[string]bool{}
	for _, g := range groups {
		want[g] = true
	}
	for _, s := range p.Series {
		if !want[s.Meta.Group] || i < s.First || math.IsNaN(s.Close[i]) {
			continue
		}
		r := LeaderStats(s.Name, s.Close, b.Close, s.First, i, rep.Window)
		r.Group = s.Meta.Group
		rep.Sectors = append(rep.Sectors, r)
	}
	SortLeaders(rep.Sectors)
	return rep, true
}

// SortLeaders orders rows by excess, best first, NaN last, then by name.
func SortLeaders(rows []LeaderRow) {
	sort.Slice(rows, func(a, b int) bool {
		x, y := rows[a].Excess, rows[b].Excess
		switch {
		case math.IsNaN(x) != math.IsNaN(y):
			return !math.IsNaN(x)
		case x != y && !math.IsNaN(x):
			return x > y
		}
		return rows[a].Name < rows[b].Name
	})
}
