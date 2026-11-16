// Package live trades nifty_hma_retest with real orders, one broker account
// at a time. Step runs once a minute, after the minute's bar closes: it reads
// the broker's view of its orders, then places, moves or cancels them to
// follow the strategy, and keeps what it knows in a State saved between runs.
//
// The orders:
//   - while a leg is armed and flat: a buy LIMIT at the slow HMA, moved each
//     minute, sized to the smaller of the capital cap and the account's funds;
//   - once filled: a sell SL (stop-limit) at the swing-low stop, raised as
//     higher swing lows confirm;
//   - at 15:15, or when the bars show the stop was hit but the SL hasn't
//     filled, a sell LIMIT priced to fill at once, repriced each minute until
//     it does.
//
// Order requests are never retried. When one fails, its tag is kept, and the
// next run looks the order up by tag before doing anything else.
package live

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"serene/internal/market"
	"serene/internal/paper"
)

// Status is an order's state, folded to what Step needs.
type Status int

const (
	Open      Status = iota // working, pending or trigger pending
	Complete                // fully filled
	Cancelled               // cancelled, possibly after a partial fill
	Rejected
)

func (s Status) String() string {
	return [...]string{"open", "complete", "cancelled", "rejected"}[s]
}

// OrderReq is a regular intraday order.
type OrderReq struct {
	Instrument     market.Instrument
	Side           market.Side
	Type           string // LIMIT or SL
	Qty            int64
	Price, Trigger market.Paise
	Tag            string
}

// OrderState is the broker's view of an order.
type OrderState struct {
	ID      string
	Tag     string
	Status  Status
	Filled  int64
	Average market.Paise
	Message string
}

// Broker is one account.
type Broker interface {
	Name() string
	// Funds is the money available for new positions.
	Funds(ctx context.Context) (market.Paise, error)
	Place(ctx context.Context, o OrderReq) (string, error)
	Modify(ctx context.Context, id string, o OrderReq) error
	Cancel(ctx context.Context, id string) error
	Order(ctx context.Context, id string) (OrderState, error)
	// ByTag finds today's order with the tag, if any.
	ByTag(ctx context.Context, tag string) (OrderState, bool, error)
	// Position is the account's net quantity in the instrument, intraday.
	Position(ctx context.Context, inst market.Instrument) (int64, error)
}

// Working is an order serene placed.
type Working struct {
	ID      string       `json:"id"` // empty when the place request failed
	Tag     string       `json:"tag"`
	Leg     string       `json:"leg"`
	Type    string       `json:"type"`
	Qty     int64        `json:"qty"`
	Price   market.Paise `json:"price"`
	Trigger market.Paise `json:"trigger,omitempty"`
	Mods    int          `json:"mods"`
	Capital market.Paise `json:"capital,omitempty"` // a buy's sizing capital
}

// Phases of the day.
const (
	Flat    = "flat"
	Long    = "long"
	Exiting = "exiting"
	Done    = "done"
)

// State is one account's day.
type State struct {
	Day       market.Date  `json:"day"`
	Phase     string       `json:"phase"`
	Seq       int          `json:"seq"`
	Buy       *Working     `json:"buy,omitempty"`
	Leg       string       `json:"leg,omitempty"`
	Qty       int64        `json:"qty,omitempty"`
	Entry     market.Paise `json:"entry,omitempty"`
	EntryTime time.Time    `json:"entry_time,omitzero"`
	Stop      market.Paise `json:"stop,omitempty"`
	LastStop  market.Paise `json:"last_stop,omitempty"`
	SL        *Working     `json:"sl,omitempty"`
	Out       *Working     `json:"out,omitempty"`
	Sold      int64        `json:"sold,omitempty"` // filled by the SL before an exit order took over
	SoldValue market.Paise `json:"sold_value,omitempty"`
	Exit      market.Paise `json:"exit,omitempty"`
	ExitTime  time.Time    `json:"exit_time,omitzero"`
	Reason    string       `json:"reason,omitempty"`
	Note      string       `json:"note,omitempty"`
	Missing   int          `json:"missing,omitempty"` // runs in a row the position wasn't held
}

// Config is the live run's settings.
type Config struct {
	Strategy paper.Config
	Capital  market.Paise // the most one trade may spend
	MaxQty   int64        // the exchange's freeze quantity, or less
	// Buffer is how far below the trigger an SL's limit sits, and below the
	// last close an exit's limit, as a fraction.
	Buffer float64
	// NoEntry stops new buys; an open position is still managed.
	NoEntry bool
}

// Event is something worth posting.
type Event struct {
	Text string
	// Closed is set on the event that ends the day's trade.
	Closed *Closed
}

// Closed is a finished trade.
type Closed struct {
	Instrument          market.Instrument
	Qty                 int64
	Entry, Exit         market.Paise
	EntryTime, ExitTime time.Time
	Reason              string
	Stop, LastStop      market.Paise
}

type stepper struct {
	ctx  context.Context
	b    Broker
	st   *State
	cfg  Config
	legs map[string]paper.Leg
	now  time.Time
	ev   []Event
}

// Step advances st by one minute. legs hold the completed bars up to now.
func Step(ctx context.Context, b Broker, st *State, legs []paper.Leg, cfg Config, now time.Time) ([]Event, error) {
	s := &stepper{ctx: ctx, b: b, st: st, cfg: cfg, legs: map[string]paper.Leg{}, now: now}
	for _, l := range legs {
		s.legs[l.Instrument.Key()] = l
	}
	if st.Phase == "" {
		st.Phase, st.Day = Flat, cfg.Strategy.Day
	}
	var err error
	switch st.Phase {
	case Flat:
		err = s.flat()
	case Long:
		err = s.long()
	case Exiting:
		err = s.exiting()
	}
	return s.ev, err
}

func (s *stepper) say(format string, a ...any) {
	s.ev = append(s.ev, Event{Text: fmt.Sprintf("*%s* %s: ", paper.HMARetestName, s.b.Name()) + fmt.Sprintf(format, a...)})
}

func (s *stepper) tag() string {
	s.st.Seq++
	return fmt.Sprintf("hmr%s%s%02d", s.st.Day.String()[5:7]+s.st.Day.String()[8:10], s.b.Name()[:1], s.st.Seq)
}

func (s *stepper) open() time.Time { return market.NSESession.Open(s.cfg.Strategy.Day) }

func (s *stepper) leg(key string) (paper.Leg, error) {
	l, ok := s.legs[key]
	if !ok || len(l.Bars) == 0 {
		return paper.Leg{}, fmt.Errorf("no bars for %s", key)
	}
	return l, nil
}

func (s *stepper) lastClose(key string) (market.Paise, error) {
	l, err := s.leg(key)
	if err != nil {
		return 0, err
	}
	return l.Bars[len(l.Bars)-1].Close, nil
}

// refresh reads a working order's state, finding it by tag if its place
// request failed. ok is false if the broker has no such order.
func (s *stepper) refresh(w *Working) (OrderState, bool, error) {
	if w.ID == "" {
		o, ok, err := s.b.ByTag(s.ctx, w.Tag)
		if err != nil || !ok {
			return o, ok, err
		}
		w.ID = o.ID
		return o, true, nil
	}
	o, err := s.b.Order(s.ctx, w.ID)
	return o, err == nil, err
}

func (s *stepper) place(w *Working, o OrderReq) error {
	o.Tag = w.Tag
	id, err := s.b.Place(s.ctx, o)
	w.ID = id
	return err
}

// cancel cancels w and returns its final state. A failed cancel is fine if the
// order has meanwhile filled or ended.
func (s *stepper) cancel(w *Working) (OrderState, error) {
	cerr := s.b.Cancel(s.ctx, w.ID)
	var o OrderState
	var err error
	// The exchange confirms a cancel a moment later.
	for try := 0; try < 8; try++ {
		if try > 0 {
			select {
			case <-s.ctx.Done():
				return o, s.ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		if o, err = s.b.Order(s.ctx, w.ID); err != nil || o.Status != Open {
			break
		}
	}
	if err != nil {
		return o, errors.Join(cerr, err)
	}
	if o.Status == Open {
		return o, fmt.Errorf("%s order %s still open after cancel: %w", s.b.Name(), w.ID, cerr)
	}
	return o, nil
}

func floorTick(p, tick market.Paise) market.Paise { return p / tick * tick }

func (s *stepper) below(p market.Paise) market.Paise {
	tick := s.cfg.Strategy.Tick
	q := floorTick(market.Paise(float64(p)*(1-s.cfg.Buffer)), tick)
	if q >= p {
		q = p - tick
	}
	return max(q, tick)
}

// lots is how many lots of inst at price capital buys, within the freeze cap.
func (s *stepper) lots(capital, price market.Paise, inst market.Instrument) int64 {
	if price <= 0 {
		return 0
	}
	// 1% kept back for charges.
	n := int64(float64(capital)*0.99) / (int64(price) * inst.LotSize)
	if s.cfg.MaxQty > 0 {
		n = min(n, s.cfg.MaxQty/inst.LotSize)
	}
	return max(n, 0)
}

func (s *stepper) flat() error {
	st := s.st
	if st.Buy != nil {
		o, ok, err := s.refresh(st.Buy)
		if err != nil {
			return err
		}
		if !ok {
			s.say("the buy order %s never reached the broker; trying again", st.Buy.Tag)
			st.Buy = nil
		} else if done, err := s.buyState(o); done || err != nil {
			return err
		}
	}
	flatAt := s.open().Add(s.cfg.Strategy.LastEntry)
	if !s.now.Before(flatAt) || s.cfg.NoEntry {
		if st.Buy != nil {
			o, err := s.cancel(st.Buy)
			if err != nil {
				return err
			}
			if done, err := s.buyState(o); done || err != nil {
				return err
			}
			st.Buy = nil
		}
		if !s.now.Before(flatAt) {
			st.Phase, st.Note = Done, "no fill by "+flatAt.Format("15:04")
		}
		return nil
	}
	var legs []paper.Leg
	for _, l := range s.legs {
		legs = append(legs, l)
	}
	arms := paper.Arms(legs, s.cfg.Strategy)
	var want *paper.Arm
	for i := range arms {
		a := &arms[i]
		if st.Buy != nil && a.Instrument.Key() == st.Buy.Leg {
			want = a // stay on the leg already working
			break
		}
		// Otherwise the leg whose limit is nearest its price.
		if want == nil || float64(a.Close-a.Limit)/float64(a.Close) < float64(want.Close-want.Limit)/float64(want.Close) {
			want = a
		}
	}
	if st.Buy != nil && (want == nil || want.Instrument.Key() != st.Buy.Leg) {
		o, err := s.cancel(st.Buy)
		if err != nil {
			return err
		}
		if done, err := s.buyState(o); done || err != nil {
			return err
		}
		st.Buy = nil
	}
	if want == nil {
		return nil
	}
	req := OrderReq{Instrument: want.Instrument, Side: market.Buy, Type: "LIMIT", Price: want.Limit}
	if st.Buy != nil {
		w := st.Buy
		// Resized to the capital it was placed with, up or down.
		qty := s.lots(w.Capital, want.Limit, want.Instrument) * want.Instrument.LotSize
		if qty == w.Qty && want.Limit == w.Price {
			return nil
		}
		if qty == 0 {
			o, err := s.cancel(w)
			if err != nil {
				return err
			}
			if done, err := s.buyState(o); done || err != nil {
				return err
			}
			st.Buy = nil
			return nil
		}
		// Kite allows 25 modifications of an order: replace it well before.
		if w.Mods < 20 {
			req.Qty = qty
			if err := s.b.Modify(s.ctx, w.ID, req); err != nil {
				// It may have filled meanwhile; the next run reads it.
				return fmt.Errorf("modify buy %s: %w", w.ID, err)
			}
			w.Qty, w.Price = qty, want.Limit
			w.Mods++
			return nil
		}
		o, err := s.cancel(w)
		if err != nil {
			return err
		}
		if done, err := s.buyState(o); done || err != nil {
			return err
		}
		st.Buy = nil
	}
	funds, err := s.b.Funds(s.ctx)
	if err != nil {
		return err
	}
	capital := min(s.cfg.Capital, funds)
	lots := s.lots(capital, want.Limit, want.Instrument)
	if lots == 0 {
		if st.Note != "funds" {
			s.say("₹%s available, not enough for one lot of %s at ₹%s", funds, name(want.Instrument), want.Limit)
			st.Note = "funds"
		}
		return nil
	}
	w := &Working{Tag: s.tag(), Leg: want.Instrument.Key(), Type: "LIMIT", Qty: lots * want.Instrument.LotSize,
		Price: want.Limit, Capital: capital}
	req.Qty = w.Qty
	st.Buy = w
	if err := s.place(w, req); err != nil {
		return fmt.Errorf("place buy %s: %w", w.Tag, err)
	}
	s.say("buy limit %s · %d qty @ ₹%s (₹%s of ₹%s available) · SL would be ₹%s",
		name(want.Instrument), w.Qty, w.Price, capital, funds, want.Stop)
	return nil
}

// buyState acts on the buy order's state. done is true once the day's
// position is open or the day is over.
func (s *stepper) buyState(o OrderState) (bool, error) {
	st := s.st
	switch {
	case o.Filled > 0 && o.Status == Open:
		// A partial fill: keep what filled.
		c, err := s.cancel(st.Buy)
		if err != nil {
			return true, err
		}
		return true, s.filled(c)
	case o.Filled > 0:
		return true, s.filled(o)
	case o.Status == Rejected:
		s.say(":warning: buy %s rejected: %s. No more entries today.", st.Buy.Tag, o.Message)
		st.Phase, st.Note, st.Buy = Done, "buy rejected: "+o.Message, nil
		return true, nil
	case o.Status == Cancelled:
		st.Buy = nil // by serene, or by hand: the strategy may place another
	}
	return false, nil
}

func (s *stepper) filled(o OrderState) error {
	st := s.st
	l, err := s.leg(st.Buy.Leg)
	if err != nil {
		return err
	}
	last := l.Bars[len(l.Bars)-1]
	st.Phase, st.Leg, st.Qty, st.Entry, st.EntryTime = Long, st.Buy.Leg, o.Filled, o.Average, last.Time
	st.Buy = nil
	stop, ok := paper.StopFor(l, s.cfg.Strategy, last.End, o.Average)
	s.ev = append(s.ev, Event{Text: fmt.Sprintf(":large_green_circle: *%s* %s: BOUGHT %s · %d qty @ ₹%s · %s",
		paper.HMARetestName, s.b.Name(), name(l.Instrument), st.Qty, st.Entry, s.now.Format("15:04"))})
	if !ok {
		s.say(":warning: no swing low below the fill: selling now")
		return s.exitNow("no stop below the fill")
	}
	st.Stop, st.LastStop = stop, stop
	return s.placeSL()
}

func (s *stepper) placeSL() error {
	st := s.st
	l, err := s.leg(st.Leg)
	if err != nil {
		return err
	}
	qty := st.Qty - st.Sold
	held, err := s.b.Position(s.ctx, l.Instrument)
	if err != nil {
		return err
	}
	if held > 0 && held < qty {
		qty = held
	}
	w := &Working{Tag: s.tag(), Leg: st.Leg, Type: "SL", Qty: qty, Trigger: st.LastStop, Price: s.below(st.LastStop)}
	st.SL = w
	err = s.place(w, OrderReq{Instrument: l.Instrument, Side: market.Sell, Type: "SL", Qty: w.Qty, Price: w.Price, Trigger: w.Trigger})
	if err != nil {
		return fmt.Errorf("place SL %s: %w", w.Tag, err)
	}
	s.say("SL ₹%s (limit ₹%s) for %d qty", w.Trigger, w.Price, w.Qty)
	return nil
}

func (s *stepper) long() error {
	st := s.st
	l, err := s.leg(st.Leg)
	if err != nil {
		return err
	}
	if st.SL != nil {
		o, ok, err := s.refresh(st.SL)
		if err != nil {
			return err
		}
		if ok && o.Status == Complete {
			reason := "stop"
			if st.SL.Trigger > st.Stop {
				reason = "trailed stop"
			}
			return s.closed(o.Average, o.Filled, reason)
		}
	}
	if gone, err := s.gone(l.Instrument); gone || err != nil {
		return err
	}
	if st.SL != nil {
		o, ok, err := s.refresh(st.SL)
		if err != nil {
			return err
		}
		switch {
		case !ok:
			st.SL = nil
		case o.Status == Complete:
			reason := "stop"
			if st.SL.Trigger > st.Stop {
				reason = "trailed stop"
			}
			return s.closed(o.Average, o.Filled, reason)
		case o.Status == Rejected || o.Status == Cancelled:
			st.Sold += o.Filled
			st.SoldValue += o.Average * market.Paise(o.Filled)
			s.say(":warning: SL %s %s (%s): selling now", st.SL.Tag, o.Status, o.Message)
			st.SL = nil
			return s.exitNow("SL " + o.Status.String())
		}
	}
	if st.SL == nil {
		return s.placeSL()
	}
	flat := s.open().Add(s.cfg.Strategy.Exit)
	if !s.now.Before(flat) {
		return s.exitNow(flat.Format("15:04"))
	}
	// From the initial stop, as the replay does: started at a raised one, it
	// would test bars from before the raise against it.
	t := paper.Trade{Instrument: l.Instrument, EntryTime: st.EntryTime, Entry: st.Entry, Stop: st.Stop,
		LastStop: st.Stop, Open: true}
	paper.Manage(l, s.cfg.Strategy, &t)
	if !t.Open && t.Reason != "15:15" {
		// The bars went through the stop, and the SL didn't fill.
		return s.exitNow(t.Reason + ", SL unfilled")
	}
	if t.LastStop > st.SL.Trigger {
		st.LastStop = t.LastStop
		w := st.SL
		req := OrderReq{Instrument: l.Instrument, Side: market.Sell, Type: "SL", Qty: w.Qty, Trigger: t.LastStop, Price: s.below(t.LastStop)}
		if w.Mods >= 20 {
			o, err := s.cancel(w)
			if err != nil {
				return err
			}
			if o.Filled > 0 {
				return nil // the next run sees the position change
			}
			return s.placeSL()
		}
		if err := s.b.Modify(s.ctx, w.ID, req); err != nil {
			return fmt.Errorf("raise SL %s: %w", w.ID, err)
		}
		w.Trigger, w.Price = req.Trigger, req.Price
		w.Mods++
		s.say("SL raised to ₹%s", w.Trigger)
	}
	return nil
}

// gone checks the account still holds the position, so no sell of serene's
// can go short. Missing twice running, sold by hand or squared off by the
// broker, it ends the day; missing once, it may be the broker's positions
// lagging a fill, so stop is true and nothing more is done this run.
func (s *stepper) gone(inst market.Instrument) (stop bool, err error) {
	held, err := s.b.Position(s.ctx, inst)
	st := s.st
	if err != nil || held > 0 {
		st.Missing = 0
		return err != nil, err
	}
	st.Missing++
	if st.Missing < 2 {
		return true, nil
	}
	for _, w := range []*Working{st.SL, st.Out} {
		if w == nil || w.ID == "" {
			continue
		}
		o, err := s.cancel(w)
		if err != nil {
			return true, err
		}
		if o.Status == Complete {
			reason := st.Reason
			if w == st.SL {
				reason = "stop"
				if w.Trigger > st.Stop {
					reason = "trailed stop"
				}
			}
			return true, s.closed(o.Average, o.Filled, reason)
		}
	}
	s.say(":warning: the account no longer holds %s (sold by hand or squared off by the broker); done for the day, P&L not tracked", name(inst))
	st.Phase, st.SL, st.Out, st.Note = Done, nil, nil, "position closed outside serene"
	st.ExitTime = s.now
	return true, nil
}

// exitNow cancels the SL, if any, and sells what is left at a limit priced to
// fill at once.
func (s *stepper) exitNow(reason string) error {
	st := s.st
	if st.SL != nil && st.SL.ID != "" {
		o, err := s.cancel(st.SL)
		if err != nil {
			return err
		}
		if o.Status == Complete {
			return s.closed(o.Average, o.Filled, "stop")
		}
		st.Sold += o.Filled
		st.SoldValue += o.Average * market.Paise(o.Filled)
	}
	st.SL = nil
	st.Phase, st.Reason = Exiting, reason
	return s.sellRest()
}

func (s *stepper) sellRest() error {
	st := s.st
	l, err := s.leg(st.Leg)
	if err != nil {
		return err
	}
	held, err := s.b.Position(s.ctx, l.Instrument)
	if err != nil {
		return err
	}
	qty := min(st.Qty-st.Sold, held)
	if qty <= 0 {
		_, err := s.gone(l.Instrument) // ends the day if it stays so
		return err
	}
	c := l.Bars[len(l.Bars)-1].Close
	w := &Working{Tag: s.tag(), Leg: st.Leg, Type: "LIMIT", Qty: qty, Price: s.below(c)}
	st.Out = w
	if err := s.place(w, OrderReq{Instrument: l.Instrument, Side: market.Sell, Type: "LIMIT", Qty: w.Qty, Price: w.Price}); err != nil {
		return fmt.Errorf("place exit %s: %w", w.Tag, err)
	}
	return nil
}

func (s *stepper) exiting() error {
	st := s.st
	if st.Out == nil {
		return s.sellRest()
	}
	o, ok, err := s.refresh(st.Out)
	if err != nil {
		return err
	}
	switch {
	case !ok:
		st.Out = nil
		return s.sellRest()
	case o.Status == Complete:
		return s.closed(o.Average, o.Filled, st.Reason)
	case o.Status == Rejected || o.Status == Cancelled:
		st.Sold += o.Filled
		st.SoldValue += o.Average * market.Paise(o.Filled)
		s.say(":warning: exit %s %s (%s): trying again", st.Out.Tag, o.Status, o.Message)
		st.Out = nil
		return s.sellRest()
	}
	// Still open: reprice to the latest close.
	c, err := s.lastClose(st.Leg)
	if err != nil {
		return err
	}
	l, _ := s.leg(st.Leg)
	w := st.Out
	if p := s.below(c); p != w.Price {
		req := OrderReq{Instrument: l.Instrument, Side: market.Sell, Type: "LIMIT", Qty: w.Qty, Price: p}
		if err := s.b.Modify(s.ctx, w.ID, req); err != nil {
			return fmt.Errorf("reprice exit %s: %w", w.ID, err)
		}
		w.Price = p
		w.Mods++
	}
	return nil
}

// closed ends the day with the last sell's fill: filled qty at avg, plus what
// earlier sells filled.
func (s *stepper) closed(avg market.Paise, filled int64, reason string) error {
	st := s.st
	l, err := s.leg(st.Leg)
	if err != nil {
		return err
	}
	value := st.SoldValue + avg*market.Paise(filled)
	sold := st.Sold + filled
	if sold > 0 {
		st.Exit = value / market.Paise(sold)
	}
	st.ExitTime, st.Reason, st.Phase, st.SL, st.Out = s.now, reason, Done, nil, nil
	if sold != st.Qty {
		s.say(":warning: sold %d of %d: check the account", sold, st.Qty)
	}
	gross := (st.Exit - st.Entry) * market.Paise(st.Qty)
	s.ev = append(s.ev, Event{
		Text: fmt.Sprintf(":red_circle: *%s* %s: SOLD %s · %d qty @ ₹%s · %s · gross ₹%s · %s",
			paper.HMARetestName, s.b.Name(), name(l.Instrument), sold, st.Exit, reason, gross, s.now.Format("15:04")),
		Closed: &Closed{Instrument: l.Instrument, Qty: st.Qty, Entry: st.Entry, Exit: st.Exit, EntryTime: st.EntryTime,
			ExitTime: st.ExitTime, Reason: reason, Stop: st.Stop, LastStop: st.LastStop},
	})
	return nil
}

func name(in market.Instrument) string {
	return strings.TrimSpace(fmt.Sprintf("NIFTY %s %s", in.Strike, in.Right))
}
