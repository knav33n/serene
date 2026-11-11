// Package dow marks Dow theory structure: swing highs and lows, labelled
// higher or lower than the last of their kind, and the trend they make.
//
// Swings come from a zigzag: a high is a swing once price falls a threshold
// below it, and a low once price rises a threshold above it. A swing is
// stamped with the bar of its extreme but is only known at Confirmed, so
// nothing here uses a bar before it closed.
package dow

import (
	"slices"
	"sort"
	"time"

	"serene/internal/market"
)

// Label names a swing against the previous swing of its kind.
type Label string

const (
	HH Label = "HH"
	LH Label = "LH"
	HL Label = "HL"
	LL Label = "LL"
	H  Label = "H" // first high, nothing to compare with
	L  Label = "L"
)

// Swing is a confirmed swing high or low.
type Swing struct {
	High      bool
	Price     market.Paise
	Time      time.Time // start of the bar that made the extreme
	Confirmed time.Time // end of the bar that completed the reversal
	Label     Label
}

// Trend is the structure the last swing high and low make.
type Trend int

const (
	Unknown Trend = iota
	Up            // higher high and higher low
	Down          // lower high and lower low
	Mixed         // one of each: a range or a turn
)

func (t Trend) String() string {
	return [...]string{"unknown", "up", "down", "mixed"}[t]
}

// Swings runs the zigzag over bars, oldest first, and returns the confirmed
// swings plus the extreme of the leg still running.
func Swings(bars []market.Bar, threshold market.Paise) (swings []Swing, running Swing) {
	if len(bars) == 0 || threshold <= 0 {
		return nil, Swing{}
	}
	dir := 0 // 1 rising leg, -1 falling leg
	hi, lo := bars[0], bars[0]
	var lastH, lastL *market.Paise
	add := func(s Swing) {
		if s.High {
			s.Label = H
			if lastH != nil {
				s.Label = LH
				if s.Price > *lastH {
					s.Label = HH
				}
			}
			p := s.Price
			lastH = &p
		} else {
			s.Label = L
			if lastL != nil {
				s.Label = LL
				if s.Price > *lastL {
					s.Label = HL
				}
			}
			p := s.Price
			lastL = &p
		}
		swings = append(swings, s)
	}
	for _, b := range bars {
		if dir >= 0 && b.High > hi.High {
			hi = b
		}
		if dir <= 0 && b.Low < lo.Low {
			lo = b
		}
		switch {
		case dir == 0 && hi.High-lo.Low >= threshold:
			if hi.Time.Before(lo.Time) {
				add(Swing{High: true, Price: hi.High, Time: hi.Time, Confirmed: b.End})
				dir, lo = -1, b
			} else {
				add(Swing{Price: lo.Low, Time: lo.Time, Confirmed: b.End})
				dir, hi = 1, b
			}
		case dir == 1 && hi.High-b.Low >= threshold:
			add(Swing{High: true, Price: hi.High, Time: hi.Time, Confirmed: b.End})
			dir, lo = -1, b
		case dir == -1 && b.High-lo.Low >= threshold:
			add(Swing{Price: lo.Low, Time: lo.Time, Confirmed: b.End})
			dir, hi = 1, b
		}
	}
	if dir == 1 {
		running = Swing{High: true, Price: hi.High, Time: hi.Time}
	} else {
		running = Swing{Price: lo.Low, Time: lo.Time}
	}
	return swings, running
}

// At returns the swings confirmed by t.
func At(swings []Swing, t time.Time) []Swing {
	i := sort.Search(len(swings), func(i int) bool { return swings[i].Confirmed.After(t) })
	return swings[:i]
}

// Last returns the latest swing high and low in swings, if any.
func Last(swings []Swing) (high, low *Swing) {
	for i := len(swings) - 1; i >= 0 && (high == nil || low == nil); i-- {
		s := &swings[i]
		if s.High && high == nil {
			high = s
		} else if !s.High && low == nil {
			low = s
		}
	}
	return high, low
}

// TrendOf reads the trend from the latest swing high and low.
func TrendOf(swings []Swing) Trend {
	h, l := Last(swings)
	if h == nil || l == nil {
		return Unknown
	}
	switch {
	case h.Label == H || l.Label == L:
		return Unknown
	case h.Label == HH && l.Label == HL:
		return Up
	case h.Label == LH && l.Label == LL:
		return Down
	}
	return Mixed
}

// Break is price trading through the last confirmed swing.
type Break struct {
	Up    bool
	Level market.Paise
	Time  time.Time // the bar that broke it
	// ChangeOfCharacter is a break against the trend it ended: through a swing
	// high in a downtrend or a swing low in an uptrend. Otherwise it is a break
	// of structure that continues the trend.
	ChangeOfCharacter bool
}

// Breaks returns the first break of each swing level, for bars at or after
// from; a level already broken before from is not reported again. Each bar is
// judged against the swings confirmed before it began.
func Breaks(bars []market.Bar, swings []Swing, from time.Time) []Break {
	var out []Break
	done := map[time.Time]bool{}
	add := func(b Break) {
		if !b.Time.Before(from) {
			out = append(out, b)
		}
	}
	for _, b := range bars {
		known := At(swings, b.Time)
		h, l := Last(known)
		tr := TrendOf(known)
		if h != nil && !done[h.Confirmed] && b.High > h.Price {
			done[h.Confirmed] = true
			add(Break{Up: true, Level: h.Price, Time: b.Time, ChangeOfCharacter: tr == Down})
		}
		if l != nil && !done[l.Confirmed] && b.Low < l.Price {
			done[l.Confirmed] = true
			add(Break{Level: l.Price, Time: b.Time, ChangeOfCharacter: tr == Up})
		}
	}
	return out
}

// WithRunning adds the running leg's extreme to swings when it has already
// gone past the last swing of its kind, as a provisional HH or LL: a trend
// turns when that swing is broken, not when the next reversal confirms it.
func WithRunning(swings []Swing, running Swing) []Swing {
	h, l := Last(swings)
	switch {
	case running.High && h != nil && running.Price > h.Price:
		running.Label = HH
	case !running.High && l != nil && running.Price < l.Price:
		running.Label = LL
	default:
		return swings
	}
	return append(slices.Clone(swings), running)
}
