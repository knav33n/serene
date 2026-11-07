// Package sim fills orders against historical bars.
package sim

import (
	"context"
	"fmt"
	"sort"
	"time"

	"calm/internal/broker"
	"calm/internal/market"
	"calm/internal/oms"
)

// Config sets the fill assumptions.
type Config struct {
	SlippageBps float64       // of price, against the trader
	MinTicks    int64         // minimum slippage in ticks
	Tick        market.Paise  // price increment
	MaxWait     time.Duration // cancel an order that cannot fill for this long
	// Fills prices each leg. Nil fills at the bar's open with the slippage
	// above, which is the backtest model.
	Fills FillModel
}

// DefaultConfig is a deliberately cautious starting point. Calibrate it
// against real spreads once paper mode has recorded them.
func DefaultConfig() Config {
	return Config{SlippageBps: 30, MinTicks: 2, Tick: 5, MaxWait: 3 * time.Minute}
}

// Broker fills an order in the first minute in which its fill model can price
// every leg; with BarOpen, that is the open of the first minute in which every
// leg has a traded candle. All legs fill together, so it does not model legging
// risk. A minute with no candle or no volume means no trade: the data carries
// the last price through untraded minutes, and those stale prices are never used.
type Broker struct {
	cfg       Config
	fills     FillModel
	pending   []*oms.Order
	orders    []*oms.Order
	positions map[string]int64
	expiry    map[string]market.Date
	today     market.Date
	Unfilled  int // orders cancelled without filling
}

var (
	_ broker.Broker  = (*Broker)(nil)
	_ broker.Account = (*Broker)(nil)
)

// New returns a simulated broker.
func New(cfg Config) (*Broker, error) {
	if cfg.Tick <= 0 {
		return nil, fmt.Errorf("tick must be positive")
	}
	fills := cfg.Fills
	if fills == nil {
		fills = BarOpen{SlippageBps: cfg.SlippageBps, MinTicks: cfg.MinTicks, Tick: cfg.Tick}
	}
	return &Broker{cfg: cfg, fills: fills, positions: map[string]int64{}, expiry: map[string]market.Date{}}, nil
}

// Submit queues an order. It cannot fill before the next minute's open, which
// is what keeps a decision from trading on the bar that produced it.
func (b *Broker) Submit(_ context.Context, o market.Order) error {
	if lf, ok := b.fills.(limitFiller); o.Kind == market.LimitOrder && !(ok && lf.FillsLimits()) {
		return fmt.Errorf("order %d: this fill model fills market orders only", o.ID)
	}
	for _, l := range o.Legs {
		if l.Qty() <= 0 {
			return fmt.Errorf("order %d: %s has no quantity (lots %d, lot size %d)",
				o.ID, l.Instrument.Key(), l.Lots, l.Instrument.LotSize)
		}
	}
	t := oms.New(o)
	for i := range t.LegStates {
		if err := t.Move(i, oms.Sent, ""); err != nil {
			return err
		}
		if err := t.Move(i, oms.Acked, ""); err != nil {
			return err
		}
	}
	b.pending = append(b.pending, t)
	b.orders = append(b.orders, t)
	return nil
}

// OnMinute fills pending orders.
func (b *Broker) OnMinute(_ context.Context, t time.Time, bars map[string]market.Bar) ([]market.Fill, error) {
	b.today = market.DateOf(t)
	var fills []market.Fill
	keep := b.pending[:0]
	for _, o := range b.pending {
		if legFills, ok := b.tryFill(o.Order, t, bars); ok {
			for i, f := range legFills {
				if err := o.Fill(i, f.Qty); err != nil {
					return nil, err
				}
				b.positions[f.Instrument.Key()] += int64(f.Side) * f.Qty
				b.expiry[f.Instrument.Key()] = f.Instrument.Expiry
			}
			fills = append(fills, legFills...)
		} else if t.Sub(o.Placed) > b.cfg.MaxWait {
			if err := b.cancel(o, "not filled within max wait"); err != nil {
				return nil, err
			}
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
		price, ok := b.fills.Price(l, t, bars)
		if !ok {
			return nil, false
		}
		fills = append(fills, market.Fill{
			OrderID: o.ID, Tag: o.Tag, Instrument: l.Instrument, Side: l.Side, Qty: l.Qty(),
			Price: price, Time: t, Kind: market.Traded,
		})
	}
	return fills, true
}

func (b *Broker) cancel(o *oms.Order, reason string) error {
	for i, l := range o.LegStates {
		if !l.State.Terminal() {
			if err := o.Move(i, oms.Cancelled, reason); err != nil {
				return err
			}
		}
	}
	return nil
}

// Cancel withdraws a pending order.
func (b *Broker) Cancel(_ context.Context, id int64) error {
	for i, o := range b.pending {
		if o.ID == id {
			if err := b.cancel(o, "cancelled"); err != nil {
				return err
			}
			b.pending = append(b.pending[:i], b.pending[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("order %d: %w", id, broker.ErrNotOpen)
}

// EndOfDay cancels unfilled orders and drops contracts that expired today, as
// the exchange does once it has settled them.
func (b *Broker) EndOfDay(context.Context) error {
	for _, o := range b.pending {
		if err := b.cancel(o, "end of day"); err != nil {
			return err
		}
	}
	b.Unfilled += len(b.pending)
	b.pending = nil
	for k, exp := range b.expiry {
		if !exp.IsZero() && !b.today.Before(exp) {
			delete(b.positions, k)
			delete(b.expiry, k)
		}
	}
	return nil
}

// Pending counts open orders.
func (b *Broker) Pending() int { return len(b.pending) }

// Positions returns net positions from the simulator's own fills.
func (b *Broker) Positions(context.Context) ([]broker.Position, error) {
	var out []broker.Position
	for k, q := range b.positions {
		if q != 0 {
			out = append(out, broker.Position{Key: k, Qty: q})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Orders returns copies of every order received.
func (b *Broker) Orders(context.Context) ([]oms.Order, error) {
	out := make([]oms.Order, len(b.orders))
	for i, o := range b.orders {
		out[i] = *o
		out[i].LegStates = append([]oms.Leg(nil), o.LegStates...)
	}
	return out, nil
}
