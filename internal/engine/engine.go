// Package engine runs a strategy against a feed and a broker. The same loop
// serves research, paper and live; only the Feed and Broker differ.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"calm/internal/broker"
	"calm/internal/cost"
	"calm/internal/margin"
	"calm/internal/market"
	"calm/internal/oms"
	"calm/internal/portfolio"
	"calm/internal/strategy"
)

// Config sets up a run.
type Config struct {
	Contracts []market.Instrument
	Session   market.Session
	Costs     *cost.Schedule
	Margin    margin.Model
	Capital   market.Paise
	// RunID prefixes every order's client ID, so orders can be traced to the
	// run that placed them. Letters and digits; "bt" when empty.
	RunID string
	// Product is what every order is placed as; Carry when empty.
	Product market.Product
}

// EquityPoint is end-of-day equity.
type EquityPoint struct {
	Date   market.Date
	Equity market.Paise
}

// Verdict is what happened to an order a strategy submitted.
type Verdict string

const (
	Accepted Verdict = "accepted"
	Rejected Verdict = "rejected"
)

// Decision records why an order was accepted or refused. Reason is a stable
// code to group by; Detail says what was found.
type Decision struct {
	Time     time.Time
	Tag      string
	OrderID  int64 // zero when rejected
	ClientID string
	Verdict  Verdict
	Reason   string
	Detail   string
}

// Result is a run's output.
type Result struct {
	Capital market.Paise
	Equity  []EquityPoint
	Trades  []portfolio.Trade
	Orders  []market.Order
	Fills   []market.Fill
	Charges market.Paise
	// Decisions holds every order submitted, accepted or not, in order.
	Decisions []Decision
	Rejected  int
	// MaxDrawdown is measured on every minute, so it sees intraday losses
	// that daily closes hide.
	MaxDrawdown market.Paise
}

type subscription struct {
	key string
	tf  market.Timeframe
	r   *market.Resampler
}

// Engine is one run.
type Engine struct {
	cfg    Config
	feed   Feed
	broker broker.Broker
	strat  strategy.Strategy

	now       time.Time
	day       market.Date
	last      map[string]market.Bar
	subs      []*subscription
	subIndex  map[string]map[market.Timeframe]*subscription
	contracts map[string][]market.Instrument
	indexKey  map[string]string
	port      *portfolio.Portfolio
	nextID    int64
	res       Result
	peak      market.Paise
}

// New builds an engine.
func New(cfg Config, feed Feed, b broker.Broker, s strategy.Strategy) *Engine {
	e := &Engine{
		cfg: cfg, feed: feed, broker: b, strat: s,
		last:      map[string]market.Bar{},
		subIndex:  map[string]map[market.Timeframe]*subscription{},
		contracts: map[string][]market.Instrument{},
		indexKey:  map[string]string{},
		port:      portfolio.New(cfg.Capital),
		peak:      cfg.Capital,
	}
	for _, c := range cfg.Contracts {
		e.contracts[c.Underlying] = append(e.contracts[c.Underlying], c)
		e.indexKey[c.Underlying] = c.IndexKey()
	}
	e.res.Capital = cfg.Capital
	return e
}

// Run plays the feed to its end.
func (e *Engine) Run(ctx context.Context) (Result, error) {
	if e.cfg.RunID == "" {
		e.cfg.RunID = "bt"
	}
	if e.cfg.Product == "" {
		e.cfg.Product = market.Carry
	}
	if _, err := oms.ClientID(e.cfg.RunID, 1); err != nil {
		return e.res, fmt.Errorf("run ID: %w", err)
	}
	if err := e.strat.Init(e); err != nil {
		return e.res, fmt.Errorf("strategy init: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return e.res, err
		}
		batch, err := e.feed.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return e.res, err
		}
		if d := market.DateOf(batch.Time); d != e.day {
			if !e.day.IsZero() {
				if err := e.endDay(ctx); err != nil {
					return e.res, err
				}
			}
			e.day = d
		}
		if err := e.minute(ctx, batch); err != nil {
			return e.res, fmt.Errorf("at %s: %w", batch.Time.In(market.IST), err)
		}
	}
	if !e.day.IsZero() {
		if err := e.endDay(ctx); err != nil {
			return e.res, err
		}
	}
	e.res.Trades = e.port.Trades
	e.res.Charges = e.port.Charges
	return e.res, nil
}

// minute handles one minute. The order is what prevents lookahead: orders
// placed earlier fill at this minute's open, and only then does the minute's
// close become visible to the strategy.
func (e *Engine) minute(ctx context.Context, batch Batch) error {
	e.now = batch.Time
	byKey := make(map[string]market.Bar, len(batch.Bars))
	for _, b := range batch.Bars {
		byKey[b.Key] = b
	}
	fills, err := e.broker.OnMinute(ctx, batch.Time, byKey)
	if err != nil {
		return err
	}
	if err := e.book(fills); err != nil {
		return err
	}

	e.now = batch.Time.Add(time.Minute)
	for _, b := range batch.Bars {
		e.last[b.Key] = b
	}
	var done []market.Bar
	for _, b := range batch.Bars {
		for _, sub := range e.subIndex[b.Key] {
			got, err := sub.r.Add(b)
			if err != nil {
				return err
			}
			done = append(done, got...)
		}
	}
	for _, sub := range e.subs {
		if b, ok := sub.r.FlushUntil(e.now); ok {
			done = append(done, b)
		}
	}
	e.trackDrawdown()
	return e.deliver(done)
}

func (e *Engine) deliver(done []market.Bar) error {
	sort.Slice(done, func(i, j int) bool {
		if !done[i].End.Equal(done[j].End) {
			return done[i].End.Before(done[j].End)
		}
		if done[i].Key != done[j].Key {
			return done[i].Key < done[j].Key
		}
		return done[i].Timeframe < done[j].Timeframe
	})
	for _, b := range done {
		if err := e.strat.OnBar(e, b); err != nil {
			return fmt.Errorf("strategy OnBar: %w", err)
		}
	}
	return nil
}

func (e *Engine) book(fills []market.Fill) error {
	for i := range fills {
		c, err := e.cfg.Costs.Charges(fills[i])
		if err != nil {
			return err
		}
		fills[i].Charges = c
		e.port.Apply(fills[i])
		e.res.Fills = append(e.res.Fills, fills[i])
		if err := e.strat.OnFill(e, fills[i]); err != nil {
			return fmt.Errorf("strategy OnFill: %w", err)
		}
	}
	return nil
}

func (e *Engine) mark(key string) (market.Paise, bool) {
	b, ok := e.last[key]
	return b.Close, ok
}

func (e *Engine) trackDrawdown() {
	eq := e.port.Equity(e.mark)
	e.peak = max(e.peak, eq)
	e.res.MaxDrawdown = max(e.res.MaxDrawdown, e.peak-eq)
}

// endDay flushes the session, cancels open orders and settles expiries.
func (e *Engine) endDay(ctx context.Context) error {
	closeAt := e.cfg.Session.Close(e.day)
	e.now = closeAt
	var done []market.Bar
	for _, sub := range e.subs {
		if b, ok := sub.r.FlushUntil(closeAt); ok {
			done = append(done, b)
		}
	}
	if err := e.deliver(done); err != nil {
		return err
	}
	if err := e.broker.EndOfDay(ctx); err != nil {
		return err
	}
	if err := e.settle(closeAt); err != nil {
		return fmt.Errorf("settle %s: %w", e.day, err)
	}
	e.trackDrawdown()
	e.res.Equity = append(e.res.Equity, EquityPoint{e.day, e.port.Equity(e.mark)})
	return nil
}

// settle cash-settles contracts expiring today at intrinsic value. A missing
// or stale index close is an error, because any guess would be a made-up P&L.
func (e *Engine) settle(closeAt time.Time) error {
	for _, p := range e.port.Positions() {
		inst := p.Instrument
		if inst.Expiry.Before(e.day) {
			return fmt.Errorf("%s is open past its expiry: data is missing for that day", inst.Key())
		}
		if inst.Expiry != e.day {
			continue
		}
		idx, ok := e.last[inst.IndexKey()]
		// Since the closing auction, the index ends about 10 minutes before options.
		if !ok || closeAt.Sub(idx.End) > 15*time.Minute {
			return fmt.Errorf("no index close to settle %s", inst.Key())
		}
		side := market.Sell
		qty := p.Qty
		if p.Qty < 0 {
			side, qty = market.Buy, -p.Qty
		}
		f := market.Fill{Tag: p.Tag, Instrument: inst, Side: side, Qty: qty,
			Price: inst.Intrinsic(idx.Close), Time: closeAt, Kind: market.Settled}
		if err := e.book([]market.Fill{f}); err != nil {
			return err
		}
	}
	return nil
}

// The methods below implement strategy.Context.

// Now returns the current time.
func (e *Engine) Now() time.Time { return e.now }

// Subscribe adds a bar subscription. Repeat calls are harmless.
func (e *Engine) Subscribe(key string, tf market.Timeframe) error {
	if e.subIndex[key][tf] != nil {
		return nil
	}
	r, err := market.NewResampler(e.cfg.Session, tf)
	if err != nil {
		return err
	}
	sub := &subscription{key: key, tf: tf, r: r}
	e.subs = append(e.subs, sub)
	if e.subIndex[key] == nil {
		e.subIndex[key] = map[market.Timeframe]*subscription{}
	}
	e.subIndex[key][tf] = sub
	return nil
}

func (e *Engine) quotedToday(key string) (market.Bar, bool) {
	b, ok := e.last[key]
	return b, ok && market.DateOf(b.Time) == market.DateOf(e.now)
}

// Spot returns the index's latest close today.
func (e *Engine) Spot(underlying string) (market.Paise, bool) {
	b, ok := e.quotedToday(e.indexKey[underlying])
	return b.Close, ok
}

// Expiries lists expiries with a quote today, nearest first.
func (e *Engine) Expiries(underlying string) []market.Date {
	seen := map[market.Date]bool{}
	var out []market.Date
	for _, c := range e.contracts[underlying] {
		if seen[c.Expiry] {
			continue
		}
		if _, ok := e.quotedToday(c.Key()); ok {
			seen[c.Expiry] = true
			out = append(out, c.Expiry)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// Chain returns contracts of an expiry that have traded today, by strike.
// Requiring a quote today stands in for a listing date: a contract that has
// not traded yet cannot be selected.
func (e *Engine) Chain(underlying string, expiry market.Date) []strategy.Quote {
	var out []strategy.Quote
	for _, c := range e.contracts[underlying] {
		if c.Expiry != expiry {
			continue
		}
		if b, ok := e.quotedToday(c.Key()); ok {
			out = append(out, strategy.Quote{Instrument: c, Last: b})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Instrument.Strike != out[j].Instrument.Strike {
			return out[i].Instrument.Strike < out[j].Instrument.Strike
		}
		return out[i].Instrument.Right < out[j].Instrument.Right
	})
	return out
}

// Last returns an instrument's latest closed 1-minute bar.
func (e *Engine) Last(key string) (market.Bar, bool) {
	b, ok := e.last[key]
	return b, ok
}

// Positions returns open positions.
func (e *Engine) Positions() []portfolio.Position { return e.port.Positions() }

// Pending counts open orders.
func (e *Engine) Pending() int { return e.broker.Pending() }

// Submit checks margin and sends the order to the broker.
func (e *Engine) Submit(tag string, legs ...market.Leg) error {
	if len(legs) == 0 {
		return fmt.Errorf("order %q has no legs", tag)
	}
	held := map[string]*margin.Holding{}
	for _, p := range e.port.Positions() {
		held[p.Instrument.Key()] = &margin.Holding{Instrument: p.Instrument, Qty: p.Qty}
	}
	var premium market.Paise
	for _, l := range legs {
		key := l.Instrument.Key()
		bar, ok := e.last[key]
		if !ok {
			return e.reject(tag, "no_price", "no price for "+key)
		}
		if held[key] == nil {
			held[key] = &margin.Holding{Instrument: l.Instrument}
		}
		held[key].Qty += int64(l.Side) * l.Qty()
		premium -= market.Paise(int64(l.Side)*l.Qty()) * bar.Close
	}
	hs := make([]margin.Holding, 0, len(held))
	for _, h := range held {
		hs = append(hs, *h)
	}
	req, err := e.cfg.Margin.Required(hs, e.Spot)
	if err != nil {
		return err
	}
	if e.port.Cash+premium-req < 0 {
		return e.reject(tag, "margin", fmt.Sprintf("margin %s exceeds available cash", req))
	}
	kind, err := orderKind(legs)
	if err != nil {
		return err
	}
	e.nextID++
	id, err := oms.ClientID(e.cfg.RunID, e.nextID)
	if err != nil {
		return err
	}
	o := market.Order{ID: e.nextID, ClientID: id, Tag: tag, Kind: kind, Product: e.cfg.Product,
		Legs: legs, Placed: e.now}
	if err := e.broker.Submit(context.Background(), o); err != nil {
		return err
	}
	e.res.Orders = append(e.res.Orders, o)
	e.res.Decisions = append(e.res.Decisions, Decision{Time: e.now, Tag: tag, OrderID: o.ID, ClientID: id, Verdict: Accepted})
	return nil
}

// orderKind is a limit order when every leg has a limit, and a market order
// when none does. A mix is refused, since its legs would behave differently.
func orderKind(legs []market.Leg) (market.OrderKind, error) {
	limits := 0
	for _, l := range legs {
		if l.Limit < 0 {
			return "", fmt.Errorf("%s: negative limit %s", l.Instrument.Key(), l.Limit)
		}
		if l.Limit > 0 {
			limits++
		}
	}
	switch limits {
	case 0:
		return market.MarketOrder, nil
	case len(legs):
		return market.LimitOrder, nil
	}
	return "", errors.New("an order's legs must all have limits or none")
}

// reject records a refused order and returns the error strategies see.
func (e *Engine) reject(tag, reason, detail string) error {
	e.res.Rejected++
	e.res.Decisions = append(e.res.Decisions, Decision{Time: e.now, Tag: tag, Verdict: Rejected, Reason: reason, Detail: detail})
	return fmt.Errorf("%w: %s", strategy.ErrRejected, detail)
}
