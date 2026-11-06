// Package portfolio tracks positions, cash and completed trades.
package portfolio

import (
	"sort"
	"time"

	"calm/internal/market"
)

// Position is an open holding. Qty is signed; OpenValue is the signed cost of
// the open quantity, so it reaches exactly zero when flat.
type Position struct {
	Instrument market.Instrument
	Qty        int64
	OpenValue  market.Paise
	Tag        string

	opened  time.Time
	side    market.Side
	entered market.Paise // value of all entries
	entryQ  int64
	exited  market.Paise // value of all exits
	exitQ   int64
	gross   market.Paise
	charges market.Paise
}

// Trade is a completed round trip in one contract.
type Trade struct {
	Tag        string
	Key        string
	Side       market.Side
	Qty        int64
	EntryPrice float64 // average, rupees
	ExitPrice  float64 // average, rupees
	EntryTime  time.Time
	ExitTime   time.Time
	Gross      market.Paise
	Charges    market.Paise
	Settled    bool
}

// Net is the trade's P&L after charges.
func (t Trade) Net() market.Paise { return t.Gross - t.Charges }

// Portfolio is cash plus open positions. Cash is the ground truth for P&L.
type Portfolio struct {
	Cash      market.Paise
	Charges   market.Paise
	Trades    []Trade
	positions map[string]*Position
}

// New returns a portfolio holding the starting cash.
func New(cash market.Paise) *Portfolio {
	return &Portfolio{Cash: cash, positions: map[string]*Position{}}
}

// Apply books a fill.
func (p *Portfolio) Apply(f market.Fill) {
	key := f.Instrument.Key()
	pos := p.positions[key]
	if pos == nil {
		pos = &Position{Instrument: f.Instrument, Tag: f.Tag, opened: f.Time, side: f.Side}
		p.positions[key] = pos
	}
	signed := int64(f.Side) * f.Qty
	p.Cash -= market.Paise(signed)*f.Price + f.Charges
	p.Charges += f.Charges
	pos.charges += f.Charges

	remaining := f.Qty
	if pos.Qty != 0 && int64(f.Side)*pos.Qty < 0 {
		held := abs(pos.Qty)
		closed := min(remaining, held)
		removed := pos.OpenValue * market.Paise(closed) / market.Paise(held)
		sign := market.Paise(1)
		if pos.Qty < 0 {
			sign = -1
		}
		pos.gross += sign*market.Paise(closed)*f.Price - removed
		pos.OpenValue -= removed
		pos.Qty += int64(f.Side) * closed
		pos.exited += market.Paise(closed) * f.Price
		pos.exitQ += closed
		remaining -= closed
		if pos.Qty == 0 {
			p.finish(pos, f)
			delete(p.positions, key)
			if remaining == 0 {
				return
			}
			// The fill flipped the position; the rest opens a new one.
			pos = &Position{Instrument: f.Instrument, Tag: f.Tag, opened: f.Time, side: f.Side}
			p.positions[key] = pos
		}
	}
	if remaining > 0 {
		value := market.Paise(remaining) * f.Price
		pos.OpenValue += value * market.Paise(f.Side)
		pos.Qty += int64(f.Side) * remaining
		pos.entered += value
		pos.entryQ += remaining
	}
}

func (p *Portfolio) finish(pos *Position, f market.Fill) {
	p.Trades = append(p.Trades, Trade{
		Tag:        pos.Tag,
		Key:        pos.Instrument.Key(),
		Side:       pos.side,
		Qty:        pos.entryQ,
		EntryPrice: pos.entered.Rupees() / float64(pos.entryQ),
		ExitPrice:  pos.exited.Rupees() / float64(pos.exitQ),
		EntryTime:  pos.opened,
		ExitTime:   f.Time,
		Gross:      pos.gross,
		Charges:    pos.charges,
		Settled:    f.Kind == market.Settled,
	})
}

// Positions returns open positions ordered by key.
func (p *Portfolio) Positions() []Position {
	out := make([]Position, 0, len(p.positions))
	for _, pos := range p.positions {
		out = append(out, *pos)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instrument.Key() < out[j].Instrument.Key() })
	return out
}

// Equity returns cash plus open positions at their marks.
func (p *Portfolio) Equity(mark func(key string) (market.Paise, bool)) market.Paise {
	e := p.Cash
	for key, pos := range p.positions {
		m, ok := mark(key)
		if !ok {
			// Unmarked positions stay at cost rather than jump to zero.
			e += pos.OpenValue
			continue
		}
		e += market.Paise(pos.Qty) * m
	}
	return e
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
