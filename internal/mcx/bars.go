package mcx

import (
	"slices"
	"strconv"
	"time"

	"serene/internal/market"
)

// PulseNames are the MCX commodities the research commands default to; the
// rest trade too thinly to read structure from.
var PulseNames = []string{"CRUDEOIL", "NATURALGAS", "GOLD", "SILVER", "COPPER", "ZINC", "ALUMINIUM"}

// SwingMultiple is the default swing reversal as a multiple of the median
// bar range.
const SwingMultiple = 2.5

// CleanStaleOpens fixes a Kite fault: when a session opens late, as on a
// holiday with only the evening session, its first minute can carry the
// previous session's opening price as its open, high or low. A price equal to
// that and far from the bar's close is replaced by the bar's own prices.
func CleanStaleOpens(bars []market.Bar) []market.Bar {
	out := slices.Clone(bars)
	if len(out) < 2 {
		return out
	}
	far := 5 * medianRange(out)
	var prevOpen market.Paise
	for i := range out {
		if i > 0 && market.DateOf(out[i].Time) == market.DateOf(out[i-1].Time) {
			continue
		}
		b := out[i]
		if i > 0 && abs(prevOpen-b.Close) > far {
			next := b.Close
			if i+1 < len(out) {
				next = out[i+1].Open
			}
			if b.Open == prevOpen {
				b.Open = b.Close
			}
			if b.High == prevOpen {
				b.High = max(b.Open, b.Close, next)
			}
			if b.Low == prevOpen {
				b.Low = min(b.Open, b.Close, next)
			}
			out[i] = b
		}
		prevOpen = bars[i].Open
	}
	return out
}

func abs(p market.Paise) market.Paise {
	if p < 0 {
		return -p
	}
	return p
}

func medianRange(bars []market.Bar) market.Paise {
	rs := make([]market.Paise, len(bars))
	for i, b := range bars {
		rs[i] = b.High - b.Low
	}
	slices.Sort(rs)
	return rs[len(rs)/2]
}

// Bucket merges bars into bars of length d, aligned to IST clock time.
func Bucket(bars []market.Bar, d time.Duration) []market.Bar {
	var out []market.Bar
	for _, b := range bars {
		t := b.Time.In(market.IST)
		start := t.Truncate(d) // IST is a whole number of minutes from UTC
		if d >= time.Hour { // whole frames counted from the 09:00 open
			open := time.Date(t.Year(), t.Month(), t.Day(), 9, 0, 0, 0, market.IST)
			start = open.Add(t.Sub(open) / d * d)
			if t.Before(open) {
				start = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, market.IST)
			}
		}
		if n := len(out); n > 0 && out[n-1].Time.Equal(start) {
			o := &out[n-1]
			o.High, o.Low, o.Close = max(o.High, b.High), min(o.Low, b.Low), b.Close
			o.Volume += b.Volume
			o.OI = b.OI
			o.End = b.End
			continue
		}
		nb := b
		nb.Time = start
		out = append(out, nb)
	}
	return out
}

// Price formats rupees without trailing zeros.
func Price(p market.Paise) string {
	return strconv.FormatFloat(p.Rupees(), 'f', -1, 64)
}
