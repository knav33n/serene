// Package adjust finds the splits and bonuses in NSE's unadjusted bhavcopy
// prices and turns them into factors.
//
// NSE's own previous close is not adjusted on an ex-date, so the bhavcopy alone
// can't tell a split from a crash. Upstox's daily candles are adjusted, so the
// ratio of an Upstox price to the bhavcopy price on the same day is flat between
// corporate actions and steps on each ex-date. Each step is one event. A step
// that is a small fraction, such as 1/2 for a 1:1 bonus or 1/5 for a split from
// ₹10 to ₹2, is snapped to that fraction exactly; anything else, such as a
// demerger, keeps the measured ratio and is marked as other.
package adjust

import (
	"math"
	"slices"
	"sort"

	"calm/internal/market"
)

// Kinds of event.
const (
	KindSplitBonus = "split_bonus" // a ratio of small whole numbers
	KindOther      = "other"       // demergers, rights issues and large dividends
)

// Event is one ex-date. Prices before it are multiplied by Num/Den to be
// comparable with prices from it on; Ratio is that product as a float.
type Event struct {
	ExDate   market.Date `json:"ex_date"`
	Ratio    float64     `json:"ratio"`
	Num      int         `json:"num,omitempty"`
	Den      int         `json:"den,omitempty"`
	Kind     string      `json:"kind"`
	Measured float64     `json:"measured"` // the ratio seen in the data, before snapping
}

// Day is one day's prices from either source, in rupees for Upstox and paise
// for the bhavcopy; only ratios between the two are used.
type Day struct {
	Date                   market.Date
	Open, High, Low, Close float64
}

const (
	// minStep is the smallest change in level counted as an event. The
	// smallest bonus usually seen, 1:20, is a step of 4.8%.
	minStep = 0.02
	// holdDays is how many days a new level must hold, so one bad candle
	// isn't taken for an event.
	holdDays = 3
	// holdTol is how close those days must stay to the new level.
	holdTol = 0.005
	// snapTol is how close a measured ratio must be to a candidate to snap.
	// Upstox's own factors are off by up to about 0.3%: Wipro's 1:3 bonus in
	// 2019 measures 0.7519 on every day either side.
	snapTol = 0.003
	// maxOther is the largest ratio kept for an event that isn't a split or
	// bonus. Demergers and rights issues lower the price; smaller steps, and
	// any upward one, come from Upstox rounding prices it has adjusted down to
	// a few rupees, as in Bajaj Finance's 2011 candles, or from its errors.
	maxOther = 0.97
	// segmentDays is how many days either side of an ex-date measure its ratio.
	segmentDays = 20
)

// ratio is a day's Upstox-to-bhavcopy level: the median of the four price
// ratios, so one field Upstox fills differently, as on the 2025-02-01 special
// session's close, can't move it.
func ratio(u, b Day) float64 {
	var r []float64
	for _, p := range [][2]float64{{u.Open, b.Open}, {u.High, b.High}, {u.Low, b.Low}, {u.Close, b.Close}} {
		if p[0] > 0 && p[1] > 0 {
			r = append(r, p[0]/p[1])
		}
	}
	if len(r) == 0 {
		return 0
	}
	return median(r)
}

type fraction struct{ num, den int }

// candidates are the ratios splits and bonuses produce: an a:b bonus gives
// b/(a+b), a split from face value ₹x to ₹y gives y/x, a consolidation the
// inverse, and a split and bonus on one ex-date the product. The set is kept
// to ratios seen in practice, because a wider one catches demergers too:
// Reliance's 2023 demerger measured 0.9532, close to a 1:20 bonus.
var candidates = func() []fraction {
	seen := map[fraction]bool{}
	add := func(n, d int) {
		g := gcd(n, d)
		seen[fraction{n / g, d / g}] = true
	}
	var bonuses []fraction
	for a := 1; a <= 10; a++ {
		for b := 1; b <= 10; b++ {
			if a <= 5 && b <= 5 || a == 1 || b == 1 {
				bonuses = append(bonuses, fraction{b, a + b})
			}
		}
	}
	values := []int{1, 2, 5, 10, 100}
	var splits []fraction
	for _, x := range values {
		for _, y := range values {
			if x != y {
				splits = append(splits, fraction{y, x})
			}
		}
	}
	for _, f := range bonuses {
		add(f.num, f.den)
	}
	for _, f := range splits {
		add(f.num, f.den)
		if f.num < f.den {
			for _, g := range bonuses {
				add(f.num*g.num, f.den*g.den)
			}
		}
	}
	out := make([]fraction, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].num*out[j].den < out[j].num*out[i].den })
	return out
}()

// Snap returns the corporate-action ratio nearest to r, if one lies within
// tolerance.
func Snap(r float64) (num, den int, ok bool) {
	best := math.Inf(1)
	for _, f := range candidates {
		if e := math.Abs(float64(f.num)/float64(f.den)/r - 1); e < best {
			best, num, den = e, f.num, f.den
		}
	}
	if best > snapTol {
		return 0, 0, false
	}
	return num, den, true
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func median(v []float64) float64 {
	v = slices.Clone(v)
	slices.Sort(v)
	if len(v)%2 == 1 {
		return v[len(v)/2]
	}
	return (v[len(v)/2-1] + v[len(v)/2]) / 2
}

// Detect finds the events in one security's history. Both inputs may be in
// any order; only dates present in both are compared.
func Detect(upstox, bhav []Day) []Event {
	byDate := make(map[market.Date]Day, len(bhav))
	for _, b := range bhav {
		byDate[b.Date] = b
	}
	type point struct {
		date market.Date
		q    float64
	}
	var pts []point
	for _, u := range upstox {
		if b, ok := byDate[u.Date]; ok {
			if q := ratio(u, b); q > 0 {
				pts = append(pts, point{u.Date, q})
			}
		}
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].date.Before(pts[j].date) })

	// Find where the level steps.
	var steps []int
	level := 0.0
	for i, p := range pts {
		if level == 0 {
			level = p.q
			continue
		}
		if math.Abs(p.q/level-1) <= minStep {
			continue
		}
		// The new level must hold for the next few days, or to the end.
		held := true
		for _, n := range pts[i+1 : min(len(pts), i+holdDays)] {
			if math.Abs(n.q/p.q-1) > holdTol {
				held = false
				break
			}
		}
		if held {
			steps = append(steps, i)
			level = p.q
		}
	}
	// Measure each step from the median level of the days either side, up to
	// the neighbouring steps, which rounding in either source can't move.
	var out []Event
	for k, i := range steps {
		lo, hi := 0, len(pts)
		if k > 0 {
			lo = steps[k-1]
		}
		if k+1 < len(steps) {
			hi = steps[k+1]
		}
		lo, hi = max(lo, i-segmentDays), min(hi, i+segmentDays)
		var before, after []float64
		for _, p := range pts[lo:i] {
			before = append(before, p.q)
		}
		for _, p := range pts[i:hi] {
			after = append(after, p.q)
		}
		// Before the ex-date Upstox divides by the adjustment, so the level
		// before is the ratio times the level after.
		measured := median(before) / median(after)
		// A step found against a drifting level can measure as almost none.
		if math.Abs(measured-1) <= minStep {
			continue
		}
		e := Event{ExDate: pts[i].date, Ratio: measured, Measured: measured, Kind: KindOther}
		if n, d, ok := Snap(measured); ok {
			e.Num, e.Den, e.Kind, e.Ratio = n, d, KindSplitBonus, float64(n)/float64(d)
		} else if measured > maxOther {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Span is a run of dates sharing one cumulative factor: a bhavcopy price on a
// date in [From, To] times Factor is comparable with today's prices. Factor
// counts splits and bonuses only; FactorAll counts every adjustment, as
// demergers and rights issues also break a price series.
type Span struct {
	From, To          market.Date
	Factor, FactorAll float64
}

// Spans turns a security's events into factor spans covering all dates.
func Spans(events []Event) []Span {
	ev := slices.Clone(events)
	sort.Slice(ev, func(i, j int) bool { return ev[i].ExDate.Before(ev[j].ExDate) })
	lo := market.Date{Year: 1900, Month: 1, Day: 1}
	hi := market.Date{Year: 9999, Month: 12, Day: 31}
	sb := func(e Event) float64 {
		if e.Kind == KindSplitBonus {
			return e.Ratio
		}
		return 1
	}
	f, all := 1.0, 1.0
	for _, e := range ev {
		f, all = f*sb(e), all*e.Ratio
	}
	var out []Span
	from := lo
	for _, e := range ev {
		out = append(out, Span{from, e.ExDate.AddDays(-1), f, all})
		f, all = f/sb(e), all/e.Ratio
		from = e.ExDate
	}
	return append(out, Span{from, hi, 1, 1})
}
