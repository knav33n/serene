package paper

import (
	"math"
	"time"

	"serene/internal/market"
)

// PDLSwingName is the frozen same-day version of the PDL-break put swing
// (serene research pdlswing, variant cap400/day; docs/strategies/pdl_swing_day.md).
const PDLSwingName = "pdl_swing_day"

// PDLSwingCap is the most a put may cost at the signal: the deepest ITM
// strike at or below it is bought.
const PDLSwingCap = 400.0

// PDLSwingMinDays is how many trading days the expiry must be after entry.
const PDLSwingMinDays = 4

// PDLDay is one daily index bar in rupees.
type PDLDay struct {
	D          market.Date
	O, H, L, C float64
}

// PDLSetup reports whether day y sets up a put entry on day y+1: close below
// the 20 EMA, no green body of 1% or more, and either a green day, a narrow
// range day (range no larger than each of the 3 before) or an inside day.
func PDLSetup(days []PDLDay, ema []float64, y int) bool {
	if y < 3 || y >= len(days) || math.IsNaN(ema[y]) {
		return false
	}
	b := days[y]
	if b.C >= ema[y] || (b.C-b.O)/b.O >= 0.01 {
		return false
	}
	r := b.H - b.L
	nr4 := true
	for k := 1; k <= 3; k++ {
		if days[y-k].H-days[y-k].L < r {
			nr4 = false
		}
	}
	inside := b.H <= days[y-1].H && b.L >= days[y-1].L
	return b.C > b.O || nr4 || inside
}

// PDLBreak is the index of the first minute bar of the day whose low is below
// pdl, ending before 15:00, or -1.
func PDLBreak(idx []market.Bar, d market.Date, pdl float64) int {
	cutoff := d.At(15, 0)
	for i, b := range idx {
		if market.DateOf(b.Time) != d {
			continue
		}
		if !b.End.Before(cutoff) {
			break
		}
		if b.Low.Rupees() < pdl {
			return i
		}
	}
	return -1
}

// PDLFill is the first option bar opening within 3 minutes of the signal
// bar's end, or -1.
func PDLFill(opt []market.Bar, sigEnd time.Time) int {
	for i, b := range opt {
		if b.Time.Before(sigEnd) {
			continue
		}
		if b.Time.Sub(sigEnd) > 3*time.Minute {
			return -1
		}
		return i
	}
	return -1
}

// PDLExit is the time exit: the open of the first bar at or after 15:15. ok
// is false until such a bar exists, unless final, when the day's last close
// is used.
func PDLExit(opt []market.Bar, d market.Date, final bool) (px market.Paise, at time.Time, ok bool) {
	flat := d.At(15, 15)
	var last *market.Bar
	for i := range opt {
		b := &opt[i]
		if market.DateOf(b.Time) != d {
			continue
		}
		if !b.Time.Before(flat) {
			return b.Open, b.Time, true
		}
		last = b
	}
	if final && last != nil {
		return last.Close, last.End, true
	}
	return 0, time.Time{}, false
}
