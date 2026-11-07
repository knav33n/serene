package sim

import (
	"math"
	"time"

	"calm/internal/market"
)

// FillModel prices one leg in the minute starting at t, or reports that it
// cannot fill then. The broker's queueing, waiting and cancelling are the same
// whichever model prices the fills, so backtest and paper share one order path.
type FillModel interface {
	Price(l market.Leg, t time.Time, bars map[string]market.Bar) (market.Paise, bool)
}

// limitFiller is a fill model that honours limit prices.
type limitFiller interface{ FillsLimits() bool }

// BarOpen fills at the minute's open, moved against the trader by slippage
// and rounded to a tick. It is the backtest model: bars have no bid or ask.
type BarOpen struct {
	SlippageBps float64
	MinTicks    int64
	Tick        market.Paise
}

// Price fills when the leg's instrument has a candle this minute.
func (m BarOpen) Price(l market.Leg, _ time.Time, bars map[string]market.Bar) (market.Paise, bool) {
	bar, ok := bars[l.Instrument.Key()]
	if !ok {
		return 0, false
	}
	return m.slip(bar.Open, l.Side), true
}

// slip moves the price against the trader, then rounds to a tick the same way.
func (m BarOpen) slip(price market.Paise, side market.Side) market.Paise {
	slip := max(float64(price)*m.SlippageBps/1e4, float64(m.MinTicks*int64(m.Tick)))
	tick := float64(m.Tick)
	if side == market.Buy {
		return market.Paise(math.Ceil((float64(price)+slip)/tick)) * m.Tick
	}
	return max(market.Paise(math.Floor((float64(price)-slip)/tick))*m.Tick, m.Tick)
}

// Quote is the best bid and ask for an instrument and when they were seen.
type Quote struct {
	Bid, Ask market.Paise
	Time     time.Time
}

// QuoteSource gives the latest quote for an instrument key.
type QuoteSource interface {
	Quote(key string) (Quote, bool)
}

// QuoteFill is the paper model: a buy pays the ask and a sell hits the bid. A
// quote older than MaxAge is never used, so a silent feed cannot price a fill.
// A limit leg fills only when the touch is at or better than its limit.
type QuoteFill struct {
	Quotes QuoteSource
	MaxAge time.Duration
}

// FillsLimits reports that limit orders are supported.
func (QuoteFill) FillsLimits() bool { return true }

// Price fills at the touch if the quote is fresh and two-sided where needed.
func (m QuoteFill) Price(l market.Leg, t time.Time, _ map[string]market.Bar) (market.Paise, bool) {
	q, ok := m.Quotes.Quote(l.Instrument.Key())
	if !ok || t.Sub(q.Time) > m.MaxAge {
		return 0, false
	}
	if l.Side == market.Buy {
		if q.Ask <= 0 || (l.Limit > 0 && q.Ask > l.Limit) {
			return 0, false
		}
		return q.Ask, true
	}
	if q.Bid <= 0 || (l.Limit > 0 && q.Bid < l.Limit) {
		return 0, false
	}
	return q.Bid, true
}
