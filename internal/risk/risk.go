// Package risk is the pre-trade gate. The engine asks it about every order in
// backtests, paper and live alike, so research results show what the limits
// would have prevented.
//
// An order that only reduces positions is an exit, and exits are never
// refused: a halt must stop new risk, not trap the risk already held.
package risk

import (
	"fmt"
	"time"

	"serene/internal/market"
)

// Limits are the gate's settings. A zero limit is off.
type Limits struct {
	// MaxDailyLoss halts new entries for the rest of the IST day once the
	// day's loss, net of charges and marked to market, reaches it. The halt
	// holds even if the loss recovers.
	MaxDailyLoss market.Paise
	// MaxLotsPerLeg caps any one leg of an order.
	MaxLotsPerLeg int64
	// MaxOpenLots caps lots held across every position after the order.
	MaxOpenLots int64
	// MaxOrdersPerMinute caps entries in any 60 seconds.
	MaxOrdersPerMinute int
	// MaxQuoteAge refuses an entry if any leg's latest bar ended longer ago,
	// so an entry is never priced off a contract that has stopped trading.
	MaxQuoteAge time.Duration
	// MaxLimitAwayPct refuses a limit leg priced further than this from the
	// leg's last price, which catches a mistyped or miscalculated limit.
	MaxLimitAwayPct float64
}

// Holding is a position the gate needs to know about.
type Holding struct {
	Qty     int64 // signed
	LotSize int64
}

// State is what the gate sees when an order arrives.
type State struct {
	Now            time.Time
	DayStartEquity market.Paise
	Equity         market.Paise // now, marked to market, net of charges
	Held           map[string]Holding
	Last           func(key string) (market.Bar, bool)
}

// Verdict is the gate's answer.
type Verdict struct {
	Allowed bool
	Exit    bool
	Reason  string // stable code when refused
	Detail  string
}

// Gate holds the limits and the little state they need: recent orders and
// any halt.
type Gate struct {
	lim     Limits
	sent    []time.Time
	haltDay market.Date
	manual  string
}

// New returns a gate, refusing negative limits.
func New(l Limits) (*Gate, error) {
	if l.MaxDailyLoss < 0 || l.MaxLotsPerLeg < 0 || l.MaxOpenLots < 0 || l.MaxOrdersPerMinute < 0 ||
		l.MaxQuoteAge < 0 || l.MaxLimitAwayPct < 0 {
		return nil, fmt.Errorf("risk limits must not be negative: %+v", l)
	}
	return &Gate{lim: l}, nil
}

// Halt stops new entries until Reset, whatever the limits say. It is the
// operator's kill switch.
func (g *Gate) Halt(reason string) {
	if reason == "" {
		reason = "halted by hand"
	}
	g.manual = reason
}

// Reset clears a halt set by hand. A daily-loss halt ends with its day.
func (g *Gate) Reset() { g.manual = "" }

// Halted reports whether entries are stopped at now, and why.
func (g *Gate) Halted(now time.Time) (bool, string) {
	if g.manual != "" {
		return true, g.manual
	}
	if g.haltDay == market.DateOf(now) {
		return true, "daily loss limit reached today"
	}
	return false, ""
}

func refuse(reason, format string, a ...any) Verdict {
	return Verdict{Reason: reason, Detail: fmt.Sprintf(format, a...)}
}

// Check decides on an order. An allowed entry counts towards the order rate.
func (g *Gate) Check(s State, legs []market.Leg) Verdict {
	if IsExit(s.Held, legs) {
		return Verdict{Allowed: true, Exit: true}
	}
	if halted, why := g.Halted(s.Now); halted {
		reason := "halted"
		if g.manual == "" {
			reason = "daily_loss"
		}
		return refuse(reason, "%s", why)
	}
	if loss := s.DayStartEquity - s.Equity; g.lim.MaxDailyLoss > 0 && loss >= g.lim.MaxDailyLoss {
		g.haltDay = market.DateOf(s.Now)
		return refuse("daily_loss", "down %s today, limit %s: entries halted for the day", loss, g.lim.MaxDailyLoss)
	}
	if g.lim.MaxOrdersPerMinute > 0 {
		keep := g.sent[:0]
		for _, t := range g.sent {
			if s.Now.Sub(t) < time.Minute {
				keep = append(keep, t)
			}
		}
		g.sent = keep
		if len(g.sent) >= g.lim.MaxOrdersPerMinute {
			return refuse("order_rate", "%d entries in the last minute, limit %d", len(g.sent), g.lim.MaxOrdersPerMinute)
		}
	}
	after := map[string]Holding{}
	for k, h := range s.Held {
		after[k] = h
	}
	for _, l := range legs {
		key := l.Instrument.Key()
		if g.lim.MaxLotsPerLeg > 0 && l.Lots > g.lim.MaxLotsPerLeg {
			return refuse("max_lots", "%s: %d lots, limit %d per leg", key, l.Lots, g.lim.MaxLotsPerLeg)
		}
		h := after[key]
		h.Qty += int64(l.Side) * l.Qty()
		h.LotSize = l.Instrument.LotSize
		after[key] = h
		if g.lim.MaxQuoteAge > 0 || (g.lim.MaxLimitAwayPct > 0 && l.Limit > 0) {
			bar, ok := s.Last(key)
			if !ok {
				return refuse("stale_quote", "%s: no price seen", key)
			}
			if age := s.Now.Sub(bar.End); g.lim.MaxQuoteAge > 0 && age > g.lim.MaxQuoteAge {
				return refuse("stale_quote", "%s: last traded %s ago, limit %s", key, age, g.lim.MaxQuoteAge)
			}
			if g.lim.MaxLimitAwayPct > 0 && l.Limit > 0 && bar.Close > 0 {
				away := 100 * float64(abs(l.Limit-bar.Close)) / float64(bar.Close)
				if away > g.lim.MaxLimitAwayPct {
					return refuse("price_collar", "%s: limit %s is %.1f%% from last %s, limit %.1f%%",
						key, l.Limit, away, bar.Close, g.lim.MaxLimitAwayPct)
				}
			}
		}
	}
	if g.lim.MaxOpenLots > 0 {
		var lots int64
		for _, h := range after {
			if h.LotSize > 0 {
				lots += abs(h.Qty) / h.LotSize
			}
		}
		if lots > g.lim.MaxOpenLots {
			return refuse("max_open_lots", "%d lots held after this order, limit %d", lots, g.lim.MaxOpenLots)
		}
	}
	g.sent = append(g.sent, s.Now)
	return Verdict{Allowed: true}
}

// IsExit reports whether an order only reduces positions: every instrument it
// trades is held, and the order moves it towards flat without passing it.
// The gate decides this from what is held, never from what a strategy claims.
func IsExit(held map[string]Holding, legs []market.Leg) bool {
	if len(legs) == 0 {
		return false
	}
	delta := map[string]int64{}
	for _, l := range legs {
		delta[l.Instrument.Key()] += int64(l.Side) * l.Qty()
	}
	for k, d := range delta {
		h := held[k].Qty
		if h == 0 || d == 0 || (d > 0) == (h > 0) || abs(d) > abs(h) {
			return false
		}
	}
	return true
}

func abs[T ~int64](v T) T {
	if v < 0 {
		return -v
	}
	return v
}
