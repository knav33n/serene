package regime

import (
	"math"
	"sort"
)

// Quadrants of relative rotation against the market.
const (
	Leading   = "Leading"
	Weakening = "Weakening"
	Lagging   = "Lagging"
	Improving = "Improving"
)

const (
	rsTrendDays    = 63
	rsMomentumDays = 21
	volDays        = 60
	volumeDays     = 20
	unusualZ       = 2.0
	unusualVolume  = 2.0
	volumeZ        = 1.0
	minRotation    = 1.0 // % trend needed before a new quadrant is news
	quadrantLook   = 5   // a quadrant change is measured over a week
)

// Returns is a lookback in trading days and its label.
var Returns = []struct {
	Label string
	Days  int
}{{"1D", 1}, {"1W", 5}, {"1M", 21}, {"3M", 63}, {"6M", 126}, {"1Y", 252}}

// Row is one index on the report date. Numbers are NaN when its history is too short.
type Row struct {
	Name, Group string
	Close       float64
	Change      []float64 // % return for each of Returns
	FromHigh    float64   // % from the 1-year high
	Above50     bool
	Above200    bool
	Has200      bool
	Z           float64 // today's move in standard deviations
	VolumeRatio float64 // today's volume over its 20-day average
	Unusual     bool
	Trend       float64 // % of sector/market ratio above its 63-day mean
	Momentum    float64 // % change in that ratio over 21 days
	Quadrant    string
	WasQuadrant string // a week earlier
}

func quadrant(trend, momentum float64) string {
	switch {
	case math.IsNaN(trend) || math.IsNaN(momentum):
		return ""
	case trend >= 0 && momentum >= 0:
		return Leading
	case trend >= 0:
		return Weakening
	case momentum < 0:
		return Lagging
	}
	return Improving
}

// rotation returns trend and momentum at i.
func rotation(rs, mean []float64, i int) (float64, float64) {
	if i < rsMomentumDays || math.IsNaN(mean[i]) {
		return math.NaN(), math.NaN()
	}
	return (rs[i]/mean[i] - 1) * 100, (rs[i]/rs[i-rsMomentumDays] - 1) * 100
}

// Sectors builds a row for each index in the named groups at date index i,
// best one-day move first. It skips an index with no record on that day.
func Sectors(p *Panel, i int, groups ...string) []Row {
	want := map[string]bool{}
	for _, g := range groups {
		want[g] = true
	}
	m := p.Series[Market]
	var out []Row
	for _, s := range p.Series {
		if !want[s.Meta.Group] || i < s.First || math.IsNaN(s.Close[i]) {
			continue
		}
		r := Row{Name: s.Name, Group: s.Meta.Group, Close: s.Close[i], Z: math.NaN(), VolumeRatio: math.NaN(),
			Trend: math.NaN(), Momentum: math.NaN()}
		for _, ret := range Returns {
			c := math.NaN()
			if j := i - ret.Days; j >= s.First {
				c = (s.Close[i]/s.Close[j] - 1) * 100
			}
			r.Change = append(r.Change, c)
		}
		r.FromHigh = drawdown(s.Close[:i+1], highDays)[i]
		ma50, ma200 := sma(s.Close[:i+1], mediumMA), sma(s.Close[:i+1], longMA)
		r.Above50 = s.Close[i] > ma50[i]
		r.Above200 = s.Close[i] > ma200[i]
		r.Has200 = !math.IsNaN(ma200[i])
		if i-volDays-1 >= s.First {
			rets := make([]float64, 0, volDays)
			for j := i - volDays; j < i; j++ {
				rets = append(rets, (s.Close[j]/s.Close[j-1]-1)*100)
			}
			if sd := stddev(rets); sd > 0 {
				r.Z = r.Change[0] / sd
			}
		}
		if i >= volumeDays && s.Volume[i] > 0 {
			sum, n := 0.0, 0.0
			for j := i - volumeDays; j < i; j++ {
				if s.Volume[j] > 0 {
					sum += s.Volume[j]
					n++
				}
			}
			if n == volumeDays {
				r.VolumeRatio = s.Volume[i] / (sum / n)
			}
		}
		// Heavy volume only counts with a real price move, because one stock's
		// block deal can double a small sector's volume on a quiet day.
		r.Unusual = math.Abs(r.Z) >= unusualZ || (r.VolumeRatio >= unusualVolume && math.Abs(r.Z) >= volumeZ)
		if m != nil && s.Name != Market {
			rs := ratio(s.Close[:i+1], m.Close[:i+1])
			mean := sma(rs, rsTrendDays)
			r.Trend, r.Momentum = rotation(rs, mean, i)
			r.Quadrant = quadrant(r.Trend, r.Momentum)
			if i >= quadrantLook {
				r.WasQuadrant = quadrant(rotation(rs, mean, i-quadrantLook))
			}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(a, b int) bool {
		x, y := out[a].Change[0], out[b].Change[0]
		if x != y && !math.IsNaN(x) && !math.IsNaN(y) {
			return x > y
		}
		return out[a].Name < out[b].Name
	})
	return out
}

// PEPercentile returns the share of days since from at or below today's P/E,
// and today's P/E. It returns NaN without enough history.
func PEPercentile(p *Panel, name string, from, i int) (pct, pe float64) {
	s, ok := p.Series[name]
	if !ok || math.IsNaN(s.PE[i]) {
		return math.NaN(), math.NaN()
	}
	below, n := 0, 0
	for j := from; j <= i; j++ {
		if v := s.PE[j]; !math.IsNaN(v) {
			n++
			if v <= s.PE[i] {
				below++
			}
		}
	}
	if n < 250 {
		return math.NaN(), s.PE[i]
	}
	return float64(below) / float64(n) * 100, s.PE[i]
}
