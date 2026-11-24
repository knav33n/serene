package live

// MCX futures: a frozen always-in strategy (goldm_hmasma_10m, silverm_hmasma_10m)
// traded with one lot on Zerodha, held overnight (NRML). StepMCX runs once a
// minute and moves the account's position toward the strategy's side, one
// order at a time:
//   - holding the wrong side or the wrong contract: close it;
//   - flat and the strategy has a side: open one lot in the contract to hold.
//
// A reversal is therefore two orders, a close and then an open, a minute or
// so apart, never a two-lot order. Orders are LIMITs priced through the last
// close by Buffer so they fill at once, repriced each minute until they do.
//
// Anything that doesn't add up (a rejected order, the broker holding less than
// serene thinks, an order that won't fill) halts the strategy: it then places
// nothing until the state file is fixed by hand, and the position, if any, is
// left as it is.

import (
	"context"
	"fmt"
	"math"
	"time"

	"serene/internal/market"
)

// MCXTarget is what the strategy wants now.
type MCXTarget struct {
	Side market.Side // 0 flat
	// Key is the contract to hold: the nearest expiry not within the roll
	// window.
	Key string
	// Last is each contract's last close, for pricing orders.
	Last map[string]market.Paise
	Tick market.Paise
}

// MCXOrder is the order serene has working.
type MCXOrder struct {
	ID     string       `json:"id"` // empty when the place request failed
	Tag    string       `json:"tag"`
	Key    string       `json:"key"`
	Side   market.Side  `json:"side"`
	Qty    int64        `json:"qty"`
	Price  market.Paise `json:"price"`
	Mods   int          `json:"mods"`
	Close  bool         `json:"close"` // closes the position rather than opening one
	Placed time.Time    `json:"placed"`
	Why    string       `json:"why"`
}

// MCXState is one strategy's account state, kept between runs.
type MCXState struct {
	Strategy  string       `json:"strategy"`
	Seq       int          `json:"seq"`
	Key       string       `json:"key,omitempty"` // contract held
	Qty       int64        `json:"qty,omitempty"` // lots, signed: + long, - short
	Entry     market.Paise `json:"entry,omitempty"`
	EntryTime time.Time    `json:"entry_time,omitzero"`
	Order     *MCXOrder    `json:"order,omitempty"`
	Halted    string       `json:"halted,omitempty"`
}

// MCXConfig is the live settings.
type MCXConfig struct {
	Lots    int64   // per position
	Buffer  float64 // how far through the last close a LIMIT is priced, as a fraction
	MaxMods int     // reprices before an unfilled order halts the strategy
	TagCode string  // short code for order tags
}

// MCXFill is a filled order.
type MCXFill struct {
	Key   string
	Side  market.Side
	Qty   int64
	Price market.Paise
	Time  time.Time
	Close bool
	Why   string
	// For a close: the trade it ends.
	Entry     market.Paise
	EntryTime time.Time
}

// MCXEvent is something to report.
type MCXEvent struct {
	Fill *MCXFill // nil for a halt or a note
	Text string
}

func sign(q int64) market.Side {
	switch {
	case q > 0:
		return market.Buy
	case q < 0:
		return market.Sell
	}
	return 0
}

// limitThrough is a LIMIT for side priced Buffer through last, on the tick.
func limitThrough(side market.Side, last, tick market.Paise, buf float64) market.Paise {
	if tick <= 0 {
		tick = 1
	}
	p := float64(last) * (1 + float64(side)*buf)
	t := float64(tick)
	if side == market.Buy {
		return market.Paise(math.Ceil(p/t) * t)
	}
	return market.Paise(math.Floor(p/t) * t)
}

func (st *MCXState) halt(why string) []MCXEvent {
	st.Halted = why
	return []MCXEvent{{Text: fmt.Sprintf(":octagonal_sign: *%s halted:* %s. Nothing more is placed until data/live state is fixed by hand.", st.Strategy, why)}}
}

// StepMCX runs one minute.
func StepMCX(ctx context.Context, b Broker, st *MCXState, tgt MCXTarget, cfg MCXConfig, now time.Time) ([]MCXEvent, error) {
	if st.Halted != "" {
		return nil, nil
	}
	if o := st.Order; o != nil {
		return stepMCXOrder(ctx, b, st, tgt, cfg, now)
	}
	held := sign(st.Qty)
	switch {
	case held != 0 && (held != tgt.Side || st.Key != tgt.Key):
		inst, err := market.ParseKey(st.Key)
		if err != nil {
			return nil, err
		}
		pos, err := b.Position(ctx, inst)
		if err != nil {
			return nil, err
		}
		// The account may hold more (another system, a manual trade), never less.
		if int64(held)*pos < int64(held)*st.Qty {
			return st.halt(fmt.Sprintf("Zerodha shows %d lot(s) of %s, serene expected %d", pos, st.Key, st.Qty)), nil
		}
		why := "cross"
		if held == tgt.Side {
			why = "roll to " + tgt.Key
		} else if tgt.Side == 0 {
			why = "flat"
		}
		return nil, placeMCX(ctx, b, st, tgt, cfg, now, st.Key, -held, abs(st.Qty), true, why)
	case held == 0 && tgt.Side != 0:
		return nil, placeMCX(ctx, b, st, tgt, cfg, now, tgt.Key, tgt.Side, cfg.Lots, false, "enter")
	}
	return nil, nil
}

func abs(q int64) int64 {
	if q < 0 {
		return -q
	}
	return q
}

func placeMCX(ctx context.Context, b Broker, st *MCXState, tgt MCXTarget, cfg MCXConfig, now time.Time,
	key string, side market.Side, qty int64, closing bool, why string) error {
	last, ok := tgt.Last[key]
	if !ok || last <= 0 {
		return fmt.Errorf("no last price for %s", key)
	}
	inst, err := market.ParseKey(key)
	if err != nil {
		return err
	}
	st.Seq++
	o := &MCXOrder{Tag: fmt.Sprintf("s%s%s%d", cfg.TagCode, now.In(market.IST).Format("0102"), st.Seq), Key: key, Side: side,
		Qty: qty, Price: limitThrough(side, last, tgt.Tick, cfg.Buffer), Close: closing, Placed: now, Why: why}
	st.Order = o // kept before the request: if it fails, its tag is looked up next run
	id, err := b.Place(ctx, OrderReq{Instrument: inst, Side: side, Type: "LIMIT", Qty: qty, Price: o.Price, Tag: o.Tag})
	if err != nil {
		return fmt.Errorf("place %s %s: %w", why, key, err)
	}
	o.ID = id
	return nil
}

func stepMCXOrder(ctx context.Context, b Broker, st *MCXState, tgt MCXTarget, cfg MCXConfig, now time.Time) ([]MCXEvent, error) {
	o := st.Order
	if o.ID == "" {
		os, found, err := b.ByTag(ctx, o.Tag)
		if err != nil {
			return nil, err
		}
		if !found {
			if now.Sub(o.Placed) < 2*time.Minute {
				return nil, nil // may not be listed yet
			}
			st.Order = nil // never reached the broker: decide afresh
			return nil, nil
		}
		o.ID = os.ID
	}
	os, err := b.Order(ctx, o.ID)
	if err != nil {
		return nil, err
	}
	var evs []MCXEvent
	if os.Filled > 0 && (os.Status == Complete || os.Status == Cancelled || os.Status == Rejected) {
		evs = append(evs, applyMCXFill(st, o, os.Filled, os.Average, now))
	}
	switch os.Status {
	case Complete:
		st.Order = nil
	case Cancelled:
		st.Order = nil // cancelled outside serene: decide afresh next run
		evs = append(evs, MCXEvent{Text: fmt.Sprintf(":warning: *%s* order %s (%s) was cancelled outside serene; it will be decided afresh", st.Strategy, o.ID, o.Why)})
	case Rejected:
		st.Order = nil
		evs = append(evs, st.halt(fmt.Sprintf("Zerodha rejected the %s order for %s: %s", o.Why, o.Key, os.Message))...)
	case Open:
		if o.Mods >= cfg.MaxMods {
			return append(evs, st.halt(fmt.Sprintf("the %s order %s for %s hasn't filled after %d reprices; it is still working", o.Why, o.ID, o.Key, o.Mods))...), nil
		}
		last, ok := tgt.Last[o.Key]
		if !ok {
			return evs, nil
		}
		p := limitThrough(o.Side, last, tgt.Tick, cfg.Buffer)
		if p == o.Price {
			return evs, nil
		}
		inst, err := market.ParseKey(o.Key)
		if err != nil {
			return evs, err
		}
		if err := b.Modify(ctx, o.ID, OrderReq{Instrument: inst, Side: o.Side, Type: "LIMIT", Qty: o.Qty - os.Filled, Price: p, Tag: o.Tag}); err != nil {
			return evs, fmt.Errorf("reprice %s: %w", o.ID, err)
		}
		o.Mods++
		o.Price = p
	}
	return evs, nil
}

func applyMCXFill(st *MCXState, o *MCXOrder, filled int64, avg market.Paise, now time.Time) MCXEvent {
	f := &MCXFill{Key: o.Key, Side: o.Side, Qty: filled, Price: avg, Time: now, Close: o.Close, Why: o.Why}
	if o.Close {
		f.Entry, f.EntryTime = st.Entry, st.EntryTime
		st.Qty += int64(o.Side) * filled
		if st.Qty == 0 {
			st.Key, st.Entry, st.EntryTime = "", 0, time.Time{}
		}
	} else {
		st.Qty += int64(o.Side) * filled
		st.Key, st.Entry, st.EntryTime = o.Key, avg, now
	}
	return MCXEvent{Fill: f}
}
