// Package swing finds swing highs in a bar series and the first close above a
// significant lower high. It only uses bars that existed at each moment.
package swing

import (
	"time"

	"serene/internal/market"
)

// Pivot is a swing high: a bar whose high is strictly above the k bars on each
// side. It is known only once the k bars after it have closed.
type Pivot struct {
	Index     int
	Time      time.Time
	High      market.Paise
	Confirmed time.Time // close of the k-th bar after the pivot
}

// Pivots returns the swing highs, oldest first.
func Pivots(bars []market.Bar, k int) []Pivot {
	var out []Pivot
	for i := k; i+k < len(bars); i++ {
		top := true
		for j := i - k; j <= i+k && top; j++ {
			top = j == i || bars[j].High < bars[i].High
		}
		if top {
			out = append(out, Pivot{Index: i, Time: bars[i].Time, High: bars[i].High, Confirmed: bars[i+k].End})
		}
	}
	return out
}

// Kind tells the two breaks apart.
type Kind int

const (
	// First is a close above a significant lower high: the fall may be over.
	First Kind = iota + 1
	// Second is a close above the first swing high made after a First break,
	// once the price has pulled back from it: the rise has resumed. It is the
	// entry.
	Second
)

func (k Kind) String() string {
	if k == Second {
		return "second"
	}
	return "first"
}

// Break is a close above a swing high.
type Break struct {
	Kind     Kind
	Level    Pivot        // the swing high that was broken
	Prior    Pivot        // the swing high before it
	Pullback market.Paise // lowest low between Level and the break
	Bar      market.Bar   // the first bar to close above Level
	BarIndex int          // Bar's index in the series
	// Blocked are the earlier bars that closed above Level but failed the
	// entry filter. Only a Second break has them.
	Blocked []market.Bar
}

// PullbackPct is how far the price fell below the broken level, in percent.
func (b Break) PullbackPct() float64 {
	return float64(b.Level.High-b.Pullback) / float64(b.Level.High) * 100
}

// Breaks scans bars in order and returns every First and Second break.
//
// While falling, a confirmed swing high below the one before it is a lower
// high, and the first close above the latest one, after a pullback of at least
// minPullbackPct below it, is a First break. The next swing high confirmed
// after that is the target, and a close above it after the same pullback is a
// Second break. If a swing high after the target is lower than the target, or
// the price falls below the low of the First break's pullback, the rise has
// failed and the scan goes back to looking for a First break.
//
// entryOK, if not nil, must also accept a bar's index for a Second break; a bar
// it refuses is recorded in Blocked and the level stays watched.
//
// A pivot confirmed by the close of bar i is only tested from bar i+1, so no
// break uses a pivot nobody could have known about yet.
func Breaks(bars []market.Bar, k int, minPullbackPct float64, entryOK func(i int) bool) []Break {
	pivots := Pivots(bars, k)
	var out []Break
	next := 0
	var prev *Pivot
	var level *Break // the level being watched: a lower high, or the target
	var first *Break // the latest First break while looking for a Second
	firstIdx := -1   // bar index of that First break
	for i, b := range bars {
		if level != nil && i > level.Level.Index {
			above := b.Close > level.Level.High && level.PullbackPct() >= minPullbackPct
			if above && level.Kind == Second && entryOK != nil && !entryOK(i) {
				level.Blocked = append(level.Blocked, b)
			} else if above {
				level.Bar, level.BarIndex = b, i
				out = append(out, *level)
				if level.Kind == First {
					first, firstIdx = level, i
				} else {
					first, firstIdx = nil, -1
				}
				level = nil
			} else if b.Low < level.Pullback {
				level.Pullback = b.Low
			}
		}
		if first != nil && i > firstIdx && b.Low < first.Pullback {
			first, firstIdx, level = nil, -1, nil // the rise failed
		}
		for next < len(pivots) && !pivots[next].Confirmed.After(b.End) {
			p := pivots[next]
			next++
			low := p.High
			for _, x := range bars[p.Index+1 : i+1] {
				low = min(low, x.Low)
			}
			switch {
			case first != nil && p.Index > firstIdx && level == nil:
				level = &Break{Kind: Second, Level: p, Prior: *prev, Pullback: low}
			case first != nil && level != nil && level.Kind == Second && p.High < level.Level.High:
				// A lower high after the target: falling again.
				first, firstIdx = nil, -1
				level = &Break{Kind: First, Level: p, Prior: level.Level, Pullback: low}
			case first == nil && prev != nil && p.High < prev.High:
				level = &Break{Kind: First, Level: p, Prior: *prev, Pullback: low}
			}
			prev = &p
		}
	}
	return out
}

// Low is a swing low on closes: a bar whose close is strictly below the closes
// of the k bars on each side. It is known once the k bars after it have closed.
type Low struct {
	Index     int
	Time      time.Time
	Close     market.Paise
	Confirmed time.Time
}

// LowsOnClose returns the swing lows on closes, oldest first.
func LowsOnClose(bars []market.Bar, k int) []Low {
	var out []Low
	for i := k; i+k < len(bars); i++ {
		bottom := true
		for j := i - k; j <= i+k && bottom; j++ {
			bottom = j == i || bars[j].Close > bars[i].Close
		}
		if bottom {
			out = append(out, Low{Index: i, Time: bars[i].Time, Close: bars[i].Close, Confirmed: bars[i+k].End})
		}
	}
	return out
}

// Trade is a long option position from a Second break to its exit.
type Trade struct {
	Entry      Break
	EntryPrice market.Paise // the break bar's close
	Initial    market.Paise // the starting stop; 0 for none
	Stop       market.Paise // the stop that was hit, or the last one
	StopFrom   *Low         // the higher low the stop came from; nil for the initial stop
	Exit       market.Bar
	ExitPrice  market.Paise
	Reason     Reason
	// Worst and Best are the lowest low and highest high from the bar after
	// the entry to the exit bar.
	Worst, Best market.Paise
}

// Reason is why a trade closed.
type Reason int

const (
	Stopped  Reason = iota + 1 // the price traded below the stop
	Flat                       // the time exit
	DataEnds                   // the last bar of the series
	Target                     // the price reached the profit target
	Held                       // the maximum holding time ran out
)

func (r Reason) String() string {
	switch r {
	case Stopped:
		return "stop"
	case Flat:
		return "time"
	case Target:
		return "target"
	case Held:
		return "held"
	}
	return "end"
}

// PnLPct is the trade's return on premium, in percent.
func (t Trade) PnLPct() float64 {
	return (t.ExitPrice.Rupees()/t.EntryPrice.Rupees() - 1) * 100
}

// MAEPct is how far the price went below the entry before the exit, in
// percent of the entry: the maximum adverse excursion. It is 0 if it never did.
func (t Trade) MAEPct() float64 {
	return max(0, (1-t.Worst.Rupees()/t.EntryPrice.Rupees())*100)
}

// MFEPct is the best price before the exit, in percent above the entry.
func (t Trade) MFEPct() float64 {
	return max(0, (t.Best.Rupees()/t.EntryPrice.Rupees()-1)*100)
}

// Exit is how a trade closes.
type Exit struct {
	// Initial returns the starting stop for an entry. Nil uses the low of the
	// pullback before the break; a result of 0 means no initial stop.
	Initial func(e Break) market.Paise
	// TrailK is the bars on each side of a higher low on closes. The stop rises
	// to each one above it, confirmed after the entry. 0 turns trailing off.
	TrailK int
	// FlatAt, if not zero, is the time of day after which no trade opens and
	// any open one sells at the close of the first bar starting at or after it.
	FlatAt time.Duration
	// EntryFrom, if not zero, is the time of day before which no trade opens.
	// Breaks before it still count as structure.
	EntryFrom time.Duration
	// TargetPct, if not zero, sells when the price reaches this percent above
	// the entry: at the target, or at the open if the bar opens above it. A bar
	// that reaches both the stop and the target counts as stopped, since a
	// 1-minute bar can't say which came first.
	TargetPct float64
	// MaxHold, if not zero, sells at the close of the first bar ending at
	// least this long after the entry.
	MaxHold time.Duration
}

// Hold is how long the trade was open, from the entry bar's close to the
// exit: the exit bar's close, or its start for a stop or target hit inside it.
func (t Trade) Hold() time.Duration {
	end := t.Exit.End
	if t.Reason == Stopped || t.Reason == Target {
		end = t.Exit.Time
	}
	return end.Sub(t.Entry.Bar.End)
}

// Trades buys each Second break at its close and sells when the price trades
// below the stop, at the time exit, or on the last bar. A bar that opens below
// the stop fills at its open, otherwise at the stop. Second breaks while a
// position is open are skipped.
func Trades(bars []market.Bar, k int, minPullbackPct float64, entryOK func(i int) bool, x Exit) []Trade {
	var lows []Low
	if x.TrailK > 0 {
		lows = LowsOnClose(bars, x.TrailK)
	}
	late := func(b market.Bar) bool {
		if x.FlatAt == 0 {
			return false
		}
		d := market.DateOf(b.Time)
		return !b.Time.Before(d.At(0, 0).Add(x.FlatAt))
	}
	var out []Trade
	busyUntil := -1
	for _, e := range Breaks(bars, k, minPullbackPct, entryOK) {
		if e.Kind != Second || e.BarIndex <= busyUntil || late(e.Bar) ||
			(x.EntryFrom != 0 && e.Bar.Time.Before(market.DateOf(e.Bar.Time).At(0, 0).Add(x.EntryFrom))) {
			continue
		}
		stop := e.Pullback
		if x.Initial != nil {
			stop = x.Initial(e)
		}
		t := Trade{Entry: e, EntryPrice: e.Bar.Close, Initial: stop, Stop: stop, Reason: DataEnds,
			Worst: e.Bar.Close, Best: e.Bar.Close}
		next := 0
		for next < len(lows) && !lows[next].Confirmed.After(e.Bar.End) {
			next++
		}
		exit := len(bars) - 1
		for i := e.BarIndex + 1; i < len(bars); i++ {
			b := bars[i]
			if t.Stop > 0 && b.Low < t.Stop {
				t.Reason, t.ExitPrice, exit = Stopped, min(t.Stop, b.Open), i
				t.Worst = min(t.Worst, t.ExitPrice)
				t.Best = max(t.Best, b.High)
				break
			}
			if x.TargetPct > 0 {
				if target := market.Rupees(t.EntryPrice.Rupees() * (1 + x.TargetPct/100)); b.High >= target {
					t.Reason, t.ExitPrice, exit = Target, max(target, b.Open), i
					t.Worst, t.Best = min(t.Worst, b.Low), max(t.Best, t.ExitPrice)
					break
				}
			}
			t.Worst, t.Best = min(t.Worst, b.Low), max(t.Best, b.High)
			if x.MaxHold > 0 && b.End.Sub(e.Bar.End) >= x.MaxHold {
				t.Reason, exit = Held, i
				break
			}
			if late(b) {
				t.Reason, exit = Flat, i
				break
			}
			for next < len(lows) && !lows[next].Confirmed.After(b.End) {
				l := lows[next]
				next++
				if l.Index > e.BarIndex && l.Close > t.Stop {
					t.Stop, t.StopFrom = l.Close, &l
				}
			}
		}
		t.Exit = bars[exit]
		if t.Reason != Stopped && t.Reason != Target {
			t.ExitPrice = t.Exit.Close
		}
		out = append(out, t)
		busyUntil = exit
	}
	return out
}
