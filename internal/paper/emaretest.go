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

// EMARetestName is the strategy's name in messages and records.
const EMARetestName = "nifty_ema_retest"

// Leg is one watched option and its completed 1-minute bars, oldest first:
// the previous trading day's, to warm the EMAs, then today's.
type Leg struct {
	Instrument market.Instrument
	Bars       []market.Bar
}

// Config sets nifty_ema_retest's rules. The zero value is not usable; see
// DefaultEMARetest.
type Config struct {
	Day       market.Date
	Fast      int           // EMA of closes; 13
	Slow      int           // 34; the buy limit rests on it
	SwingK    int           // a swing low is below this many bars each side
	LastEntry time.Duration // after the open: no arms or fills from then
	Exit      time.Duration // after the open: the time exit, at that bar's open
	Capital   market.Paise
	Tick      market.Paise
}

// DefaultEMARetest is the paper-trading configuration agreed on 2026-11-12.
func DefaultEMARetest(d market.Date) Config {
	return Config{Day: d, Fast: 13, Slow: 34, SwingK: 2,
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
}

// EMARetest replays the day. Each leg arms when its fast EMA closes above the
// slow EMA after being at or below it on the bar before. While armed, a buy
// limit rests at the slow EMA of the last closed bar and fills on the first bar
// whose low reaches it, at the limit or that bar's open if lower. A close with
// the fast EMA back at or below the slow cancels the arm. The stop is the most
// recent confirmed swing low of today below the fill; with none, or with the
// capital too small for one lot, that arm is dropped and the leg waits for the
// next cross. The first leg to fill is the day's trade. From the bar after the
// fill, a low at or below the stop exits at the stop or that bar's open if
// lower, each newly confirmed higher swing low raises the stop, and the bar
// starting at the time exit exits at its open. It returns nil if nothing has
// filled.
func EMARetest(legs []Leg, cfg Config) (*Trade, error) {
	if cfg.Fast < 1 || cfg.Slow <= cfg.Fast || cfg.SwingK < 1 || cfg.Capital <= 0 || cfg.Tick <= 0 {
		return nil, errors.New("nifty_ema_retest: bad config")
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
		return nil, errors.New("nifty_ema_retest: " + l.Instrument.Key() + " has no lot size")
	}
	bars := l.Bars
	t0 := today(bars, cfg.Day)
	open := market.NSESession.Open(cfg.Day)
	lastEntry := open.Add(cfg.LastEntry)
	ef, es := indicator.EMA(bars, cfg.Fast), indicator.EMA(bars, cfg.Slow)
	lows := swing.PivotLows(bars, cfg.SwingK)
	var armed bool
	var armedAt time.Time
	for i := max(t0, 1); i < len(bars); i++ {
		b := bars[i]
		if !b.Time.Before(lastEntry) {
			break
		}
		if armed && !math.IsNaN(es[i-1]) {
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
		case !(ef[i-1] > es[i-1]):
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
