// Package sim fills orders against historical bars.
package sim

import (
	"context"
	"fmt"
	"math"
	"time"

	"calm/internal/market"
)

// Config sets the fill assumptions.
type Config struct {
	SlippageBps float64       // of price, against the trader
	MinTicks    int64         // minimum slippage in ticks
	Tick        market.Paise  // price increment
	MaxWait     time.Duration // cancel an order that cannot fill for this long
}

// DefaultConfig is a deliberately cautious starting point. Calibrate it
// against real spreads once paper mode has recorded them.
func DefaultConfig() Config {
	return Config{SlippageBps: 30, MinTicks: 2, Tick: 5, MaxWait: 3 * time.Minute}
}

// Broker fills an order at the open of the first minute in which every leg has
// a traded candle. All legs fill together, so a backtest does not model legging
// risk. A minute with no candle or no volume means no trade: the data carries the
// last price through untraded minutes, and those stale prices are never used.
type Broker struct {
	cfg      Config
	pending  []market.Order
	Unfilled int // orders cancelled without filling
}

// New returns a simulated broker.
func New(cfg Config) (*Broker, error) {
	if cfg.Tick <= 0 {
		return nil, fmt.Errorf("tick must be positive")
	}
	return &Broker{cfg: cfg}, nil
}

// Submit queues an order. It cannot fill before the next minute's open, which
// is what keeps a decision from trading on the bar that produced it.
func (b *Broker) Submit(_ context.Context, o market.Order) error {
	for _, l := range o.Legs {
		if l.Qty() <= 0 {
			return fmt.Errorf("order %d: %s has no quantity (lots %d, lot size %d)",
				o.ID, l.Instrument.Key(), l.Lots, l.Instrument.LotSize)
		}
	}
	b.pending = append(b.pending, o)
	return nil
}

// OnMinute fills pending orders.
func (b *Broker) OnMinute(_ context.Context, t time.Time, bars map[string]market.Bar) ([]market.Fill, error) {
	var fills []market.Fill
	keep := b.pending[:0]
	for _, o := range b.pending {
		if legFills, ok := b.tryFill(o, t, bars); ok {
			fills = append(fills, legFills...)
		} else if t.Sub(o.Placed) > b.cfg.MaxWait {
			b.Unfilled++
		} else {
			keep = append(keep, o)
		}
	}
	b.pending = keep
	return fills, nil
}

func (b *Broker) tryFill(o market.Order, t time.Time, bars map[string]market.Bar) ([]market.Fill, bool) {
	fills := make([]market.Fill, 0, len(o.Legs))
	for _, l := range o.Legs {
		bar, ok := bars[l.Instrument.Key()]
		if !ok || bar.Volume == 0 {
			return nil, false
		}
		fills = append(fills, market.Fill{
			OrderID: o.ID, Tag: o.Tag, Instrument: l.Instrument, Side: l.Side, Qty: l.Qty(),
			Price: b.slip(bar.Open, l.Side), Time: t, Kind: market.Traded,
		})
	}
	return fills, true
}

// slip moves the price against the trader, then rounds to a tick the same way.
func (b *Broker) slip(price market.Paise, side market.Side) market.Paise {
	slip := max(float64(price)*b.cfg.SlippageBps/1e4, float64(b.cfg.MinTicks*int64(b.cfg.Tick)))
	tick := float64(b.cfg.Tick)
	if side == market.Buy {
		return market.Paise(math.Ceil((float64(price)+slip)/tick)) * b.cfg.Tick
	}
	return max(market.Paise(math.Floor((float64(price)-slip)/tick))*b.cfg.Tick, b.cfg.Tick)
}

// EndOfDay cancels unfilled orders.
func (b *Broker) EndOfDay(context.Context) error {
	b.Unfilled += len(b.pending)
	b.pending = nil
	return nil
}

// Pending counts open orders.
func (b *Broker) Pending() int { return len(b.pending) }
