package live

// STBT short straddles on 1DTE index options, the optbt cells taken live with
// one lot on Zerodha (NRML, carried overnight):
//
//   - entry day: the trading day before the nearest expiry. At the entry time
//     the ATM CE and PE are picked off the index, and each leg arms a
//     wait-and-trade SELL: an exchange SL order whose trigger is Wait% below
//     the leg's price then;
//   - once sold, each leg has an exchange SL BUY at SL% above its fill. The
//     stop trails: for every TrailX% the premium has fallen below the fill
//     (1m lows since the fill), it moves down TrailY% of the fill;
//   - a stopped leg re-arms a SELL trigger at its last fill price (re-entry at
//     cost) while it has re-entries left;
//   - exit day (the expiry) at the exit time: triggers are cancelled and open
//     legs are bought back with LIMITs chased through the touch.
//
// Orders are DAY orders, so each morning the working triggers and stops are
// placed again; a price already through its level is traded at once with a
// marketable LIMIT, as the backtest fills a gap at the open. StepSTBT does one
// pass and is called every minute (and every few seconds while an exit is
// working). Anything unexpected (a rejection, a partial fill that is
// cancelled, a chase that won't fill) halts the strategy for a human.

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"serene/internal/kite"
	"serene/internal/market"
)

// STBTRules is one live cell.
type STBTRules struct {
	Name     string // e.g. nifty_stbt_1dte
	Index    string // NIFTY, SENSEX
	Exchange string // NFO, BFO
	Step     float64
	Entry    string // HH:MM IST on the day before expiry
	Exit     string // HH:MM IST on the expiry day
	Wait     float64
	SL       float64
	TrailX   float64
	TrailY   float64
	Reentry  int
}

// The two cells chosen 2026-11-26 (optbt grid + 2025–26 holdout).
var (
	NiftySTBT  = STBTRules{Name: "nifty_stbt_1dte", Index: "NIFTY", Exchange: "NFO", Step: 50, Entry: "13:00", Exit: "15:15", Wait: -5, SL: 40, TrailX: 20, TrailY: 10, Reentry: 1}
	SensexSTBT = STBTRules{Name: "sensex_stbt_1dte", Index: "SENSEX", Exchange: "BFO", Step: 100, Entry: "11:00", Exit: "15:15", Wait: -10, SL: 40, TrailX: 20, TrailY: 10, Reentry: 2}
)

// Leg states.
const (
	LegWait   = "wait"   // SELL trigger armed (wait-and-trade)
	LegRecost = "recost" // SELL trigger armed at the last fill (re-entry)
	LegOpen   = "open"   // short, with a stop
	LegDone   = "done"
)

// Order kinds.
const (
	KindEntry = "entry" // SELL: SL trigger or marketable LIMIT
	KindStop  = "stop"  // BUY: SL trigger or marketable LIMIT
	KindExit  = "exit"  // BUY: LIMIT chase at the exit time
)

// STBTTrade is one sale and its buy-back.
type STBTTrade struct {
	Entry, Exit         float64
	EntryTime, ExitTime time.Time
	Reason              string // sl, exit
}

// STBTLeg is one side of the straddle.
type STBTLeg struct {
	Right  string // CE, PE
	Symbol string
	Token  int64
	Strike float64
	State  string
	Ref    float64 // price when armed
	Trig   float64 // SELL trigger while waiting
	Entry  float64 // fill of the open sale
	EntryT time.Time
	Stop   float64
	Low    float64 // lowest 1m low since the fill
	ReUsed int

	Order    string // working order id
	OrderTag string
	Kind     string
	OrderDay string // DAY orders lapse at the close
	Chase    int
	Trades   []STBTTrade
}

// STBTState is one cell's live state, saved between runs.
type STBTState struct {
	Rules    string
	Active   bool
	EntryDay market.Date
	Expiry   market.Date
	Lot      int64
	Spot     float64
	Legs     []STBTLeg
	Done     []string // entry days decided (traded or skipped)
	Halted   string
	Seq      int
}

func (s *STBTState) DoneFor(d market.Date) bool {
	for _, x := range s.Done {
		if x == d.String() {
			return true
		}
	}
	return false
}

// STBTBroker is the Zerodha account.
type STBTBroker interface {
	Place(ctx context.Context, o kite.OrderRequest) (string, error)
	Modify(ctx context.Context, id string, o kite.OrderRequest) error
	Cancel(ctx context.Context, id string) error
	Order(ctx context.Context, id string) (kite.Order, error)
	Orders(ctx context.Context) ([]kite.Order, error)
	// Position is the account's net NRML quantity in the symbol.
	Position(ctx context.Context, symbol string) (int64, error)
}

// STBTMarket is what a pass reads.
type STBTMarket interface {
	LTP(ctx context.Context, symbols []string) (map[string]float64, error)
	// LowSince is the lowest 1m low of the option after the minute of from.
	LowSince(ctx context.Context, token int64, from time.Time) (float64, bool, error)
}

// STBTPick is an entry: the ATM legs at the entry time.
type STBTPick struct {
	Expiry market.Date
	Spot   float64
	Lot    int64
	Legs   []STBTLeg // Right, Symbol, Token, Strike, Ref
	Skip   string    // why not to enter (margin, quotes), "" to enter
	Margin float64   // Kite's basket margin for the two sales
	Funds  float64   // available then
}

// STBTEvent is something to report.
type STBTEvent struct {
	Kind string // armed, skip, sold, stop, trail, rearm, exit, closed, halt, note
	Leg  string
	Text string
}

// STBTConfig holds the pass settings.
type STBTConfig struct {
	Buffer   float64 // how far through the touch a marketable LIMIT goes, 0.03 = 3%
	SLSlack  float64 // an SL order's limit beyond its trigger, 0.05 = 5%
	Tick     float64
	MaxChase int
}

// DefaultSTBTConfig is used live.
var DefaultSTBTConfig = STBTConfig{Buffer: 0.03, SLSlack: 0.05, Tick: 0.05, MaxChase: 20}

func hm(s string) int {
	t, err := time.Parse("15:04", s)
	if err != nil {
		panic(err)
	}
	return t.Hour()*60 + t.Minute()
}

func minuteOf(t time.Time) int {
	t = t.In(market.IST)
	return t.Hour()*60 + t.Minute()
}

// NextTradingDay is the next NSE trading day after d.
func NextTradingDay(d market.Date) market.Date {
	for n := d.AddDays(1); ; n = n.AddDays(1) {
		if market.NSETradingDay(n) {
			return n
		}
	}
}

// EntryDay reports whether today is the day before exp (1 DTE).
func EntryDay(today, exp market.Date) bool {
	return market.NSETradingDay(today) && NextTradingDay(today) == exp
}

// WantsPick says whether the caller should build an entry pick this pass.
func (r STBTRules) WantsPick(st *STBTState, now time.Time, exp market.Date) bool {
	today := market.DateOf(now.In(market.IST))
	m := minuteOf(now)
	return !st.Active && st.Halted == "" && EntryDay(today, exp) && !st.DoneFor(today) && m >= hm(r.Entry) && m < hm(r.Entry)+30
}

func cents(p float64) float64                    { return math.Round(p*100) / 100 }
func (c STBTConfig) roundDown(p float64) float64 { return cents(math.Floor(p/c.Tick+1e-9) * c.Tick) }
func (c STBTConfig) roundUp(p float64) float64   { return cents(math.Ceil(p/c.Tick-1e-9) * c.Tick) }

type stbtPass struct {
	ctx  context.Context
	b    STBTBroker
	st   *STBTState
	r    STBTRules
	c    STBTConfig
	now  time.Time
	ltp  map[string]float64
	evs  []STBTEvent
	exch string
}

func (p *stbtPass) ev(kind string, l *STBTLeg, f string, a ...any) {
	name := ""
	if l != nil {
		name = fmt.Sprintf("%s %.0f %s", p.r.Index, l.Strike, l.Right)
	}
	p.evs = append(p.evs, STBTEvent{Kind: kind, Leg: name, Text: fmt.Sprintf(f, a...)})
}

func (p *stbtPass) halt(l *STBTLeg, f string, a ...any) error {
	msg := fmt.Sprintf(f, a...)
	p.st.Halted = msg
	p.ev("halt", l, "%s", msg)
	return fmt.Errorf("%s halted: %s", p.r.Name, msg)
}

// place sends an order, recording its tag first: a request that fails may
// still have reached Kite, so the next pass looks the tag up instead of
// sending again.
func (p *stbtPass) place(l *STBTLeg, kind string, o kite.OrderRequest) error {
	p.st.Seq++
	tag := fmt.Sprintf("stbt%s%d", strings.ToLower(p.r.Index[:1]), p.st.Seq)
	o.Exchange, o.Symbol, o.Product, o.Qty, o.Tag = p.exch, l.Symbol, "NRML", p.st.Lot, tag
	l.OrderTag, l.Kind, l.OrderDay, l.Order, l.Chase = tag, kind, market.DateOf(p.now.In(market.IST)).String(), "", 0
	id, err := p.b.Place(p.ctx, o)
	if err != nil {
		return fmt.Errorf("place %s %s %s: %w", l.Symbol, o.Side, o.Type, err)
	}
	l.Order = id
	return nil
}

func (p *stbtPass) clearOrder(l *STBTLeg) {
	l.Order, l.OrderTag, l.Kind, l.OrderDay, l.Chase = "", "", "", "", 0
}

// StepSTBT runs one pass. pick is non-nil only when WantsPick was true.
func StepSTBT(ctx context.Context, b STBTBroker, m STBTMarket, st *STBTState, r STBTRules, c STBTConfig, now time.Time, pick *STBTPick) ([]STBTEvent, error) {
	p := &stbtPass{ctx: ctx, b: b, st: st, r: r, c: c, now: now, exch: r.Exchange}
	if st.Halted != "" {
		return nil, nil
	}
	today := market.DateOf(now.In(market.IST))
	if !st.Active {
		if pick == nil {
			return nil, nil
		}
		st.Done = append(st.Done, today.String())
		if pick.Skip != "" {
			p.ev("skip", nil, "%s", pick.Skip)
			return p.evs, nil
		}
		st.Active, st.EntryDay, st.Expiry, st.Lot, st.Spot, st.Legs = true, today, pick.Expiry, pick.Lot, pick.Spot, nil
		for _, l := range pick.Legs {
			l.State = LegWait
			l.Trig = c.roundDown(l.Ref * (1 + r.Wait/100))
			l.Trades = nil
			st.Legs = append(st.Legs, l)
		}
		var parts []string
		for _, l := range st.Legs {
			parts = append(parts, fmt.Sprintf("%.0f %s at ₹%.2f → sell at ₹%.2f", l.Strike, l.Right, l.Ref, l.Trig))
		}
		p.ev("armed", nil, "%s %.2f, expiry %s, lot %d: %s", r.Index, pick.Spot, pick.Expiry, pick.Lot, strings.Join(parts, "; "))
	}
	if !st.Active {
		return p.evs, nil
	}
	var syms []string
	for _, l := range st.Legs {
		syms = append(syms, l.Symbol)
	}
	var err error
	if p.ltp, err = m.LTP(ctx, syms); err != nil {
		return p.evs, err
	}
	for i := range st.Legs {
		if err := p.follow(&st.Legs[i]); err != nil {
			return p.evs, err
		}
	}
	exitNow := today == st.Expiry && minuteOf(now) >= hm(r.Exit) || st.Expiry.Before(today)
	for i := range st.Legs {
		l := &st.Legs[i]
		if exitNow {
			if err := p.exit(l); err != nil {
				return p.evs, err
			}
			continue
		}
		if err := p.arm(l); err != nil {
			return p.evs, err
		}
		if err := p.trail(m, l); err != nil {
			return p.evs, err
		}
	}
	done := true
	for _, l := range st.Legs {
		if l.State != LegDone || l.Order != "" {
			done = false
		}
	}
	if done {
		p.ev("closed", nil, "")
		st.Active = false
	}
	return p.evs, nil
}

// follow checks the leg's working order and acts on a fill.
func (p *stbtPass) follow(l *STBTLeg) error {
	today := market.DateOf(p.now.In(market.IST)).String()
	if l.OrderTag != "" && l.OrderDay != today {
		// yesterday's DAY order has lapsed and today's book doesn't list it:
		// the account must hold what serene thinks before anything is re-armed
		held, err := p.b.Position(p.ctx, l.Symbol)
		if err != nil {
			return err
		}
		want := int64(0)
		if l.State == LegOpen {
			want = -p.st.Lot
		}
		if held != want {
			return p.halt(l, "Zerodha holds %d of %s (NRML), serene expects %d: yesterday's %s order may have filled after the last check", held, l.Symbol, want, l.Kind)
		}
		p.clearOrder(l)
		return nil
	}
	if l.Order == "" && l.OrderTag != "" {
		// the place call failed: did it reach Kite?
		os, err := p.b.Orders(p.ctx)
		if err != nil {
			return err
		}
		for _, o := range os {
			if o.Tag == l.OrderTag {
				l.Order = o.ID
			}
		}
		if l.Order == "" {
			p.clearOrder(l)
			return nil
		}
	}
	if l.Order == "" {
		return nil
	}
	o, err := p.b.Order(p.ctx, l.Order)
	if err != nil {
		return err
	}
	switch strings.ToUpper(o.Status) {
	case "COMPLETE":
		return p.filled(l, o)
	case "REJECTED":
		return p.halt(l, "Zerodha rejected the %s order %s: %s", l.Kind, o.ID, o.StatusMessage)
	case "CANCELLED":
		if o.Filled > 0 {
			return p.halt(l, "the %s order %s was cancelled after %d of %d filled", l.Kind, o.ID, o.Filled, o.Qty)
		}
		if l.Kind == KindExit {
			p.clearOrder(l) // resent by exit
			return nil
		}
		return p.halt(l, "the %s order %s was cancelled outside serene (%s)", l.Kind, o.ID, o.StatusMessage)
	case "OPEN":
		// a triggered SL or a LIMIT still working: chase it through the touch
	default:
		return nil // trigger pending, or a request Kite is still handling
	}
	l.Chase++
	if l.Chase > p.c.MaxChase {
		return p.halt(l, "the %s order %s has not filled after %d tries", l.Kind, o.ID, l.Chase-1)
	}
	ltp := p.ltp[l.Symbol]
	if ltp <= 0 {
		return nil
	}
	req := kite.OrderRequest{Type: "LIMIT", Qty: p.st.Lot}
	if o.Side == "SELL" {
		req.Price = p.c.roundDown(ltp * (1 - p.c.Buffer))
	} else {
		req.Price = p.c.roundUp(ltp * (1 + p.c.Buffer))
	}
	if req.Price == o.Price && o.Type == "LIMIT" {
		return nil
	}
	if err := p.b.Modify(p.ctx, l.Order, req); err != nil {
		return fmt.Errorf("modify %s: %w", o.ID, err)
	}
	return nil
}

func (p *stbtPass) filled(l *STBTLeg, o kite.Order) error {
	kind := l.Kind
	p.clearOrder(l)
	switch kind {
	case KindEntry:
		re := l.State == LegRecost
		if re {
			l.ReUsed++
		}
		l.State, l.Entry, l.EntryT, l.Low = LegOpen, o.Average, p.now, o.Average
		l.Stop = p.c.roundUp(o.Average * (1 + p.r.SL/100))
		what := "sold"
		if re {
			what = fmt.Sprintf("sold again (re-entry %d of %d)", l.ReUsed, p.r.Reentry)
		}
		p.ev("sold", l, "%s at ₹%.2f, stop ₹%.2f", what, o.Average, l.Stop)
	case KindStop, KindExit:
		reason := "sl"
		if kind == KindExit {
			reason = "exit"
		}
		l.Trades = append(l.Trades, STBTTrade{Entry: l.Entry, Exit: o.Average, EntryTime: l.EntryT, ExitTime: p.now, Reason: reason})
		pnl := (l.Entry - o.Average) * float64(p.st.Lot)
		if kind == KindExit {
			l.State = LegDone
			p.ev("exit", l, "bought back at ₹%.2f (sold ₹%.2f): ₹%.0f", o.Average, l.Entry, pnl)
			return nil
		}
		if l.ReUsed < p.r.Reentry {
			l.State, l.Trig = LegRecost, p.c.roundDown(l.Entry)
			p.ev("stop", l, "stopped at ₹%.2f (sold ₹%.2f): ₹%.0f · re-entry armed: sell again at ₹%.2f", o.Average, l.Entry, pnl, l.Trig)
		} else {
			l.State = LegDone
			p.ev("stop", l, "stopped at ₹%.2f (sold ₹%.2f): ₹%.0f · no re-entries left", o.Average, l.Entry, pnl)
		}
	}
	return nil
}

// arm places the order a leg needs and doesn't have.
func (p *stbtPass) arm(l *STBTLeg) error {
	if l.Order != "" || l.OrderTag != "" || l.State == LegDone {
		return nil
	}
	ltp := p.ltp[l.Symbol]
	if ltp <= 0 {
		return nil // no quote yet; next pass
	}
	switch l.State {
	case LegWait, LegRecost:
		if ltp <= l.Trig {
			return p.place(l, KindEntry, kite.OrderRequest{Side: "SELL", Type: "LIMIT", Price: p.c.roundDown(ltp * (1 - p.c.Buffer))})
		}
		return p.place(l, KindEntry, kite.OrderRequest{Side: "SELL", Type: "SL", Trigger: l.Trig, Price: p.c.roundDown(l.Trig * (1 - p.c.SLSlack))})
	case LegOpen:
		if ltp >= l.Stop {
			return p.place(l, KindStop, kite.OrderRequest{Side: "BUY", Type: "LIMIT", Price: p.c.roundUp(ltp * (1 + p.c.Buffer))})
		}
		return p.place(l, KindStop, kite.OrderRequest{Side: "BUY", Type: "SL", Trigger: l.Stop, Price: p.c.roundUp(l.Stop * (1 + p.c.SLSlack))})
	}
	return nil
}

// trail lowers an open leg's stop from the 1m lows since its fill.
func (p *stbtPass) trail(m STBTMarket, l *STBTLeg) error {
	if l.State != LegOpen || p.r.TrailX <= 0 || l.Kind != KindStop || l.Order == "" {
		return nil
	}
	low, ok, err := m.LowSince(p.ctx, l.Token, l.EntryT)
	if err != nil || !ok {
		return err
	}
	l.Low = min(l.Low, low)
	k := math.Floor((l.Entry - l.Low) / l.Entry * 100 / p.r.TrailX)
	if k < 1 {
		return nil
	}
	stop := p.c.roundUp(l.Entry * (1 + (p.r.SL-k*p.r.TrailY)/100))
	if stop >= l.Stop {
		return nil
	}
	o, err := p.b.Order(p.ctx, l.Order)
	if err != nil {
		return err
	}
	if strings.ToUpper(o.Status) != "TRIGGER PENDING" {
		return nil // triggered or done: follow handles it
	}
	if err := p.b.Modify(p.ctx, l.Order, kite.OrderRequest{Type: "SL", Qty: p.st.Lot, Trigger: stop, Price: p.c.roundUp(stop * (1 + p.c.SLSlack))}); err != nil {
		return fmt.Errorf("trail %s: %w", l.Symbol, err)
	}
	p.ev("trail", l, "low ₹%.2f (%.0f%% under the ₹%.2f sale): stop ₹%.2f → ₹%.2f", l.Low, (l.Entry-l.Low)/l.Entry*100, l.Entry, l.Stop, stop)
	l.Stop = stop
	return nil
}

// exit cancels triggers and buys back open legs at the exit time.
func (p *stbtPass) exit(l *STBTLeg) error {
	if l.State == LegDone || l.Kind == KindExit {
		return nil // finished, or the buy-back is working (follow chases it)
	}
	if l.Order == "" && l.OrderTag != "" {
		return nil // an unconfirmed place: follow resolves it first
	}
	if l.Order != "" {
		cerr := p.b.Cancel(p.ctx, l.Order)
		o, err := p.b.Order(p.ctx, l.Order)
		if err != nil {
			return err
		}
		switch strings.ToUpper(o.Status) {
		case "COMPLETE": // filled as it was cancelled
			if err := p.filled(l, o); err != nil {
				return err
			}
		case "CANCELLED":
			if o.Filled > 0 {
				return p.halt(l, "the %s order %s was cancelled at the exit after %d of %d filled", l.Kind, o.ID, o.Filled, o.Qty)
			}
			p.clearOrder(l)
		default:
			return fmt.Errorf("cancel %s at the exit: still %s (%v)", o.ID, o.Status, cerr)
		}
	}
	if l.State != LegOpen {
		l.State = LegDone
		return nil
	}
	ltp := p.ltp[l.Symbol]
	if ltp <= 0 {
		return fmt.Errorf("no price for %s at the exit", l.Symbol)
	}
	return p.place(l, KindExit, kite.OrderRequest{Side: "BUY", Type: "LIMIT", Price: p.c.roundUp(ltp * (1 + p.c.Buffer))})
}

// Busy says a pass should be repeated within seconds: an exit or a
// triggered order is being chased, or a place is unconfirmed.
func (s *STBTState) Busy() bool {
	for _, l := range s.Legs {
		if l.Kind == KindExit || l.Chase > 0 || (l.Order == "" && l.OrderTag != "") {
			return true
		}
	}
	return false
}
