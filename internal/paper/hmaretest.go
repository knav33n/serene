// Package paper holds the rules of strategies that are paper traded live,
// written as pure replays of one day's bars so a run each minute gives the
// same answer as the last, plus whatever the newest bar adds.
package paper

import (
	"errors"
	"math"
	"time"

	"serene/internal/indicator"
	"serene/internal/market"
	"serene/internal/swing"
)

// HMARetestName is the strategy's name in messages and records.
const HMARetestName = "nifty_hma_retest"

// Leg is one watched option and its completed 1-minute bars, oldest first:
// the previous trading day's, to warm the averages, then today's.
type Leg struct {
	Instrument market.Instrument
	Bars       []market.Bar
}

// Config sets nifty_hma_retest's rules. The zero value is not usable; see
// DefaultHMARetest.
type Config struct {
	Day       market.Date
	Fast      int           // Hull moving average of closes; 13
	Slow      int           // 34; the buy limit rests on it
	SwingK    int           // a swing low is below this many bars each side
	LastEntry time.Duration // after the open: no arms or fills from then
	// FirstEntry and FirstArm, after the open, are when fills and arms may
	// start: a cross on a bar closing before FirstArm doesn't arm, and no
	// limit fills in a bar starting before FirstEntry. Zero allows both from
	// the first bar.
	FirstEntry, FirstArm time.Duration
	Exit                 time.Duration // after the open: the time exit, at that bar's open
	Capital              market.Paise
	Tick                 market.Paise
	// Immediate buys at the open of the bar after the cross instead of a
	// limit at the slow HMA. With no swing low of today below the fill, the
	// stop is today's lowest low before the fill.
	Immediate bool
}

// DefaultHMARetest is the configuration agreed on 2026-11-12, with crosses
// before 09:30 ignored from 2026-11-17.
func DefaultHMARetest(d market.Date) Config {
	return Config{Day: d, Fast: 13, Slow: 34, SwingK: 2,
		FirstEntry: 15 * time.Minute, FirstArm: 15 * time.Minute, // 09:30
		LastEntry: 5*time.Hour + 45*time.Minute, // 15:00
		Exit:      6 * time.Hour,                // 15:15
		Capital:   market.Rupees(100000), Tick: 5}
}

// Trade is the day's one trade. Open is true while it has not exited.
type Trade struct {
	Instrument market.Instrument
	Armed      time.Time // the bar whose close crossed
	EntryTime  time.Time // start of the bar the limit filled in
	Entry      market.Paise
	Lots, Qty  int64
	Stop       market.Paise // initial
	Open       bool
	ExitTime   time.Time // start of the exit bar
	Exit       market.Paise
	LastStop   market.Paise
	Reason     string // "stop", "trailed stop" or "15:15"
	DayLowStop bool   // Immediate only: no swing low below the fill, so the stop is the day's low
}

// HMARetest replays the day. Each leg arms when its fast Hull MA closes above
// the slow one after being at or below it on the bar before. While armed, a buy
// limit rests at the slow HMA of the last closed bar and fills on the first bar
// whose low reaches it, at the limit or that bar's open if lower. A close with
// the fast HMA back at or below the slow cancels the arm. The stop is the most
// recent confirmed swing low of today below the fill; with none, or with the
// capital too small for one lot, that arm is dropped and the leg waits for the
// next cross. The first leg to fill is the day's trade. From the bar after the
// fill, a low at or below the stop exits at the stop or that bar's open if
// lower, each newly confirmed higher swing low raises the stop, and the bar
// starting at the time exit exits at its open. It returns nil if nothing has
// filled.
func HMARetest(legs []Leg, cfg Config) (*Trade, error) {
	if cfg.Fast < 1 || cfg.Slow <= cfg.Fast || cfg.SwingK < 1 || cfg.Capital <= 0 || cfg.Tick <= 0 {
		return nil, errors.New("nifty_hma_retest: bad config")
	}
	var best *Trade
	var bestLeg Leg
	for _, l := range legs {
		t, err := entry(l, cfg)
		if err != nil {
			return nil, err
		}
		if t != nil && (best == nil || t.EntryTime.Before(best.EntryTime)) {
			best, bestLeg = t, l
		}
	}
	if best != nil {
		exit(bestLeg, cfg, best)
	}
	return best, nil
}

// today is the index of the first bar on cfg.Day, or len(bars).
func today(bars []market.Bar, d market.Date) int {
	for i, b := range bars {
		if !market.DateOf(b.Time).Before(d) {
			return i
		}
	}
	return len(bars)
}

func entry(l Leg, cfg Config) (*Trade, error) {
	if l.Instrument.LotSize <= 0 {
		return nil, errors.New("nifty_hma_retest: " + l.Instrument.Key() + " has no lot size")
	}
	bars := l.Bars
	t0 := today(bars, cfg.Day)
	open := market.NSESession.Open(cfg.Day)
	lastEntry := open.Add(cfg.LastEntry)
	firstEntry, firstArm := open.Add(cfg.FirstEntry), open.Add(cfg.FirstArm)
	ef, es := indicator.HMA(bars, cfg.Fast), indicator.HMA(bars, cfg.Slow)
	lows := swing.PivotLows(bars, cfg.SwingK)
	var armed bool
	var armedAt time.Time
	for i := max(t0, 1); i < len(bars); i++ {
		b := bars[i]
		if !b.Time.Before(lastEntry) {
			break
		}
		if cfg.Immediate && armed {
			armed = false
			if !b.Time.Before(firstEntry) && b.Open > 0 {
				price := b.Open
				stop, ok := lastLowBelow(lows, t0, b.Time, price)
				dayLow := !ok
				if dayLow {
					stop, ok = dayLowBelow(bars[t0:i], price)
				}
				lots := int64(cfg.Capital) / (int64(price) * l.Instrument.LotSize)
				if ok && lots > 0 {
					return &Trade{Instrument: l.Instrument, Armed: armedAt, EntryTime: b.Time, Entry: price,
						Lots: lots, Qty: lots * l.Instrument.LotSize, Stop: stop, LastStop: stop, Open: true, DayLowStop: dayLow}, nil
				}
			}
		}
		if !cfg.Immediate && armed && !math.IsNaN(es[i-1]) && !b.Time.Before(firstEntry) {
			limit := floorTick(market.Rupees(es[i-1]), cfg.Tick)
			if b.Low <= limit {
				price := min(limit, b.Open)
				armed = false
				stop, ok := lastLowBelow(lows, t0, b.Time, price)
				lots := int64(cfg.Capital) / (int64(price) * l.Instrument.LotSize)
				if ok && price > 0 && lots > 0 {
					return &Trade{Instrument: l.Instrument, Armed: armedAt, EntryTime: b.Time, Entry: price,
						Lots: lots, Qty: lots * l.Instrument.LotSize, Stop: stop, LastStop: stop, Open: true}, nil
				}
			}
		}
		// The close of bar i: cancel or arm for the next bar.
		if math.IsNaN(es[i]) || math.IsNaN(es[i-1]) {
			continue
		}
		above := ef[i] > es[i]
		switch {
		case !above:
			armed = false
		case !(ef[i-1] > es[i-1]) && !b.End.Before(firstArm):
			armed, armedAt = true, b.Time
		}
	}
	return nil, nil
}

// lastLowBelow is the most recent swing low of today confirmed by the start
// of bar `at` and below price.
func lastLowBelow(lows []swing.PivotLow, t0 int, at time.Time, price market.Paise) (market.Paise, bool) {
	for k := len(lows) - 1; k >= 0; k-- {
		p := lows[k]
		if p.Index < t0 || p.Confirmed.After(at) {
			continue
		}
		if p.Low < price {
			return p.Low, true
		}
	}
	return 0, false
}

// dayLowBelow is the lowest low of bars, if below price.
func dayLowBelow(bars []market.Bar, price market.Paise) (market.Paise, bool) {
	if len(bars) == 0 {
		return 0, false
	}
	lo := bars[0].Low
	for _, b := range bars[1:] {
		lo = min(lo, b.Low)
	}
	return lo, lo < price
}

func exit(l Leg, cfg Config, t *Trade) {
	bars := l.Bars
	flat := market.NSESession.Open(cfg.Day).Add(cfg.Exit)
	lows := swing.PivotLows(bars, cfg.SwingK)
	next := 0 // the next swing low to consider for the trail
	for next < len(lows) && !lows[next].Confirmed.After(t.EntryTime) {
		next++
	}
	for i := 0; i < len(bars); i++ {
		b := bars[i]
		if b.Time.Before(t.EntryTime) {
			continue
		}
		if b.Time.After(t.EntryTime) {
			if !b.Time.Before(flat) {
				t.Open, t.ExitTime, t.Exit, t.Reason = false, b.Time, b.Open, "15:15"
				return
			}
			if b.Low <= t.LastStop {
				t.Open, t.ExitTime, t.Exit = false, b.Time, min(t.LastStop, b.Open)
				t.Reason = "stop"
				if t.LastStop > t.Stop {
					t.Reason = "trailed stop"
				}
				return
			}
		}
		// Swing lows confirmed at this bar's close, the fill bar's included,
		// raise the stop.
		for next < len(lows) && !lows[next].Confirmed.After(b.End) {
			if lows[next].Low > t.LastStop {
				t.LastStop = lows[next].Low
			}
			next++
		}
	}
}

func floorTick(p, tick market.Paise) market.Paise {
	return p / tick * tick
}

// Arm is a leg's buy for the bar after its last one: a limit at the slow HMA
// and the stop a fill there would take.
type Arm struct {
	Instrument  market.Instrument
	ArmedAt     time.Time
	Limit, Stop market.Paise
	Close       market.Paise // the last bar's close
}

// Arms returns each leg armed after its last bar, by the replay's rules, for
// live orders. Unlike the replay, a touch of the limit doesn't use up an arm:
// a live order the touch didn't fill keeps resting. A leg with no swing low
// below the limit yet is left out, and nothing is armed before cfg.FirstEntry
// or from cfg.LastEntry.
func Arms(legs []Leg, cfg Config) []Arm {
	var out []Arm
	for _, l := range legs {
		bars := l.Bars
		t0 := today(bars, cfg.Day)
		n := len(bars)
		if n < 2 || t0 >= n {
			continue
		}
		open := market.NSESession.Open(cfg.Day)
		if !bars[n-1].End.Before(open.Add(cfg.LastEntry)) || bars[n-1].End.Before(open.Add(cfg.FirstEntry)) {
			continue
		}
		ef, es := indicator.HMA(bars, cfg.Fast), indicator.HMA(bars, cfg.Slow)
		var armed bool
		var armedAt time.Time
		for i := max(t0, 1); i < n; i++ {
			if math.IsNaN(es[i]) || math.IsNaN(es[i-1]) {
				continue
			}
			switch {
			case !(ef[i] > es[i]):
				armed = false
			case !(ef[i-1] > es[i-1]) && !bars[i].End.Before(open.Add(cfg.FirstArm)):
				armed, armedAt = true, bars[i].Time
			}
		}
		if !armed {
			continue
		}
		limit := floorTick(market.Rupees(es[n-1]), cfg.Tick)
		stop, ok := lastLowBelow(swing.PivotLows(bars, cfg.SwingK), t0, bars[n-1].End, limit)
		if ok && limit > 0 {
			out = append(out, Arm{Instrument: l.Instrument, ArmedAt: armedAt, Limit: limit, Stop: stop, Close: bars[n-1].Close})
		}
	}
	return out
}

// StopFor is the stop a buy at price, filled in the bar starting at, takes:
// the most recent of today's swing lows confirmed by then below price.
func StopFor(l Leg, cfg Config, at time.Time, price market.Paise) (market.Paise, bool) {
	return lastLowBelow(swing.PivotLows(l.Bars, cfg.SwingK), today(l.Bars, cfg.Day), at, price)
}

// Manage runs the replay's exit rules over l's bars for a trade filled live:
// it raises t.LastStop and closes t when a bar hits the stop or the time exit
// comes.
func Manage(l Leg, cfg Config, t *Trade) {
	exit(l, cfg, t)
}
