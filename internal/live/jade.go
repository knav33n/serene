package live

// Jade Lizard on NIFTY monthly options, traded with one lot on Zerodha (NRML,
// held for weeks). The rules are the backtested credit-targeted cell, no stop:
//   - entry, once per monthly expiry, on the first trading day within 45
//     calendar days of it (up to 40 if a day was missed): sell a put and a call
//     at the same |delta|, picked from 0.10-0.40 for the total credit nearest
//     the target (200), and buy a call Width (100) points above the short call;
//     skipped when the credit is more than 25% off target or below the width;
//   - exit, checked once a day at 15:20: the open P&L reaches 50% of the credit,
//     or 21 or fewer days remain.
//
// StepJade is called repeatedly within a run, with fresh quotes each time. It
// does one thing per call: decide, place one leg, or follow up the working
// order. Legs go one at a time in an order that keeps margin down: the long
// wing is bought first and sold last. Orders are LIMITs priced Buffer through
// the touch, repriced on each call until they fill; an order that won't fill
// after MaxMods reprices, a rejection, or the account holding less than serene
// expects halts the strategy, leaving positions as they are.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"serene/internal/market"
	"serene/internal/options"
)

// JadeQuote is one option's market.
type JadeQuote struct {
	Last, Bid, Ask float64
	Contracts      int64 // traded today, in lots
}

// Mark is the mid when both sides are quoted, else the last price.
func (q JadeQuote) Mark() float64 {
	if q.Bid > 0 && q.Ask > 0 && q.Ask >= q.Bid {
		return (q.Bid + q.Ask) / 2
	}
	return q.Last
}

// JadeChain is one expiry's NIFTY options, keyed by strike and call.
type JadeChain struct {
	Expiry market.Date
	Fwd    float64 // the same expiry's future
	Now    time.Time
	Quotes map[JadeStrike]JadeQuote
}

// JadeStrike names an option in a chain.
type JadeStrike struct {
	Strike int
	Call   bool
}

// JadePick is an entry: the three strikes and their marks.
type JadePick struct {
	Expiry                market.Date
	Put, Call, Wing       int
	PutPx, CallPx, WingPx float64
	Delta                 float64 // |delta| of both shorts
	PutDelta, CallDelta   float64 // as solved, signed
	Credit                float64 // per unit, at marks
}

// JadeRules are the strategy's parameters.
type JadeRules struct {
	Target       float64 // total credit per unit
	Width        int     // long call above the short call, points
	MinContracts int64
	Rate         float64
	TP           float64 // fraction of the entry credit
	ExitDTE      int
	EntryDTE     int // enter from this many days to expiry
	LastEntryDTE int // and no later than this (a missed day)
}

// DefaultJade is the rule set that was backtested.
var DefaultJade = JadeRules{Target: 200, Width: 100, MinContracts: 100, Rate: 0.065, TP: 0.5, ExitDTE: 21, EntryDTE: 45, LastEntryDTE: 40}

// DTE is calendar days from d to exp, as the backtest counts them.
func DTE(d, exp market.Date) int {
	return int(math.Round(exp.At(12, 0).Sub(d.At(12, 0)).Hours() / 24))
}

type jadeCand struct {
	strike int
	px     float64
	delta  float64
}

func (r JadeRules) pick(ch JadeChain, call bool, target float64) (jadeCand, bool) {
	years := options.YearsToExpiry(ch.Now, ch.Expiry)
	var best jadeCand
	found := false
	for k, q := range ch.Quotes {
		px := q.Mark()
		if k.Call != call || px <= 0 || q.Contracts < r.MinContracts || k.Strike%100 != 0 {
			continue
		}
		in := options.Input{Forward: ch.Fwd, Strike: float64(k.Strike), Years: years, Rate: r.Rate, Call: call}
		vol, err := options.ImpliedVol(in, px)
		if err != nil {
			continue
		}
		c := jadeCand{k.Strike, px, options.Sensitivities(in, vol).Delta}
		if !found || math.Abs(math.Abs(c.delta)-target) < math.Abs(math.Abs(best.delta)-target) {
			best, found = c, true
		}
	}
	return best, found && math.Abs(math.Abs(best.delta)-target) <= 0.10
}

// PickJade chooses the entry strikes, or says why there is none.
func (r JadeRules) PickJade(ch JadeChain) (JadePick, string) {
	if ch.Fwd <= 0 {
		return JadePick{}, "no forward"
	}
	var best JadePick
	found := false
	bestD := math.Inf(1)
	for i := 10; i <= 40; i++ {
		d := float64(i) / 100
		p, ok1 := r.pick(ch, false, d)
		c, ok2 := r.pick(ch, true, d)
		if !ok1 || !ok2 {
			continue
		}
		wq, ok := ch.Quotes[JadeStrike{c.strike + r.Width, true}]
		if !ok || wq.Mark() <= 0 || wq.Contracts < r.MinContracts {
			continue
		}
		credit := p.px + c.px - wq.Mark()
		if diff := math.Abs(credit - r.Target); diff < bestD {
			bestD, found = diff, true
			best = JadePick{Expiry: ch.Expiry, Put: p.strike, Call: c.strike, Wing: c.strike + r.Width,
				PutPx: p.px, CallPx: c.px, WingPx: wq.Mark(), Delta: d, PutDelta: p.delta, CallDelta: c.delta, Credit: credit}
		}
	}
	switch {
	case !found:
		return JadePick{}, "no liquid strikes"
	case bestD > 0.25*r.Target:
		return best, fmt.Sprintf("credit %.1f is more than 25%% off the %.0f target", best.Credit, r.Target)
	case best.Credit < float64(r.Width):
		return best, fmt.Sprintf("credit %.1f is below the %d-point call width", best.Credit, r.Width)
	}
	return best, ""
}

// JadeLeg is one leg of the position, with its fills.
type JadeLeg struct {
	Key         string       `json:"key"`
	Strike      int          `json:"strike"`
	Call        bool         `json:"call"`
	Sign        int          `json:"sign"` // -1 short, +1 long
	Qty         int64        `json:"qty"`  // units
	Entry       market.Paise `json:"entry,omitempty"`
	EntryFilled int64        `json:"entry_filled,omitempty"`
	EntryTime   time.Time    `json:"entry_time,omitzero"`
	Exit        market.Paise `json:"exit,omitempty"`
	ExitFilled  int64        `json:"exit_filled,omitempty"`
	ExitTime    time.Time    `json:"exit_time,omitzero"`
}

// JadeOrder is the order serene has working.
type JadeOrder struct {
	Leg    int          `json:"leg"`
	Exit   bool         `json:"exit"`
	ID     string       `json:"id"` // empty when the place request failed
	Tag    string       `json:"tag"`
	Side   market.Side  `json:"side"`
	Qty    int64        `json:"qty"`
	Price  market.Paise `json:"price"`
	Mods   int          `json:"mods"`
	Placed time.Time    `json:"placed"`
}

// Jade phases.
const (
	JadeFlat     = ""
	JadeEntering = "entering"
	JadeOpen     = "open"
	JadeExiting  = "exiting"
)

// JadeState is the strategy's account state, kept between runs.
type JadeState struct {
	Since    time.Time   `json:"since"`
	Seq      int         `json:"seq"`
	Phase    string      `json:"phase,omitempty"`
	Expiry   market.Date `json:"expiry,omitzero"`
	Legs     []JadeLeg   `json:"legs,omitempty"` // in entry order: wing, short call, short put
	Planned  float64     `json:"planned,omitempty"`
	Reason   string      `json:"reason,omitempty"` // why the exit
	Done     []string    `json:"done,omitempty"`   // expiries entered or skipped
	LastMark market.Date `json:"last_mark,omitzero"`
	Order    *JadeOrder  `json:"order,omitempty"`
	Halted   string      `json:"halted,omitempty"`
}

// DoneFor says whether expiry e was already entered or skipped.
func (st *JadeState) DoneFor(e market.Date) bool {
	for _, d := range st.Done {
		if d == e.String() {
			return true
		}
	}
	return false
}

// Credit is the per-unit credit the entry fills took in.
func (st *JadeState) Credit() float64 {
	var c float64
	for _, l := range st.Legs {
		if l.Qty > 0 {
			c -= float64(l.Sign) * l.Entry.Rupees() * float64(l.EntryFilled) / float64(l.Qty)
		}
	}
	return c
}

// OpenPnL is the per-unit P&L if the open legs were closed at marks.
func (st *JadeState) OpenPnL(marks map[string]float64) (float64, bool) {
	v := st.Credit()
	for _, l := range st.Legs {
		m, ok := marks[l.Key]
		if !ok || m <= 0 {
			return 0, false
		}
		v += float64(l.Sign) * m
	}
	return v, true
}

// JadeSnap is what a step sees of the market.
type JadeSnap struct {
	Now time.Time
	// Decide lets the daily decision run: it is past 15:20 and quotes are live.
	Decide bool
	// Entry is today's entry, when today is an entry day and strikes were found.
	Entry *JadePick
	// MarginOK says the account can carry Entry.
	MarginOK bool
	// Quotes are the legs' markets, by instrument key.
	Quotes map[string]JadeQuote
	Lot    int64
	Tick   market.Paise
}

// JadeConfig is the live settings.
type JadeConfig struct {
	Rules   JadeRules
	Buffer  float64 // how far through the touch a LIMIT is priced, as a fraction
	MaxMods int
	NoEntry bool // a STOP file: exits still go
}

// JadeEvent is something to report.
type JadeEvent struct {
	Kind string // entered, exited, mark, fill, halt, note
	Text string
}

// JadeKey is the instrument key of a NIFTY option.
func JadeKey(exp market.Date, strike int, call bool) string {
	r := market.Put
	if call {
		r = market.Call
	}
	return market.Option(market.NFO, "NIFTY", exp, market.Rupees(float64(strike)), r, 0).Key()
}

func (st *JadeState) halt(why string) []JadeEvent {
	st.Halted = why
	return []JadeEvent{{Kind: "halt", Text: fmt.Sprintf(":octagonal_sign: *jade_lizard halted:* %s. Nothing more is placed until data/live/jade.json is fixed by hand.", why)}}
}

// Busy says a step still has work to do now: an order or legs to place.
func (st *JadeState) Busy() bool {
	return st.Halted == "" && (st.Order != nil || st.Phase == JadeEntering || st.Phase == JadeExiting)
}

// StepJade does one thing.
func StepJade(ctx context.Context, b Broker, st *JadeState, snap JadeSnap, cfg JadeConfig) ([]JadeEvent, error) {
	if st.Halted != "" {
		return nil, nil
	}
	if st.Order != nil {
		return stepJadeOrder(ctx, b, st, snap, cfg)
	}
	today := market.DateOf(snap.Now.In(market.IST))
	switch st.Phase {
	case JadeFlat:
		if !snap.Decide || snap.Entry == nil || cfg.NoEntry || st.DoneFor(snap.Entry.Expiry) {
			return nil, nil
		}
		e := snap.Entry
		st.Done = append(st.Done, e.Expiry.String())
		if !snap.MarginOK {
			return []JadeEvent{{Kind: "note", Text: fmt.Sprintf(":warning: *jade_lizard* skipped the %s expiry: not enough margin for -P%d -C%d +C%d", e.Expiry, e.Put, e.Call, e.Wing)}}, nil
		}
		st.Phase, st.Expiry, st.Planned, st.Reason, st.LastMark = JadeEntering, e.Expiry, e.Credit, "", today
		st.Legs = []JadeLeg{
			{Key: JadeKey(e.Expiry, e.Wing, true), Strike: e.Wing, Call: true, Sign: +1, Qty: snap.Lot},
			{Key: JadeKey(e.Expiry, e.Call, true), Strike: e.Call, Call: true, Sign: -1, Qty: snap.Lot},
			{Key: JadeKey(e.Expiry, e.Put, false), Strike: e.Put, Call: false, Sign: -1, Qty: snap.Lot},
		}
		return []JadeEvent{{Kind: "note", Text: fmt.Sprintf(":lizard: *jade_lizard* entering %s expiry: -P%d -C%d +C%d at |Δ| %.2f, credit ≈ %.1f at marks",
			e.Expiry, e.Put, e.Call, e.Wing, e.Delta, e.Credit)}}, nil
	case JadeEntering:
		for i := range st.Legs {
			if l := &st.Legs[i]; l.EntryFilled < l.Qty {
				return nil, placeJade(ctx, b, st, snap, cfg, i, false)
			}
		}
		st.Phase = JadeOpen
		return []JadeEvent{{Kind: "entered"}}, nil
	case JadeOpen:
		if !snap.Decide || !st.LastMark.Before(today) {
			return nil, nil
		}
		marks := map[string]float64{}
		for _, l := range st.Legs {
			marks[l.Key] = snap.Quotes[l.Key].Mark()
		}
		pnl, ok := st.OpenPnL(marks)
		if !ok {
			return nil, fmt.Errorf("no mark for a leg")
		}
		st.LastMark = today
		dte := DTE(today, st.Expiry)
		switch {
		case pnl >= cfg.Rules.TP*st.Credit():
			st.Phase, st.Reason = JadeExiting, "tp"
		case dte <= cfg.Rules.ExitDTE:
			st.Phase, st.Reason = JadeExiting, "time"
		default:
			return []JadeEvent{{Kind: "mark", Text: fmt.Sprintf("%.2f %d", pnl, dte)}}, nil
		}
		return []JadeEvent{{Kind: "mark", Text: fmt.Sprintf("%.2f %d", pnl, dte)}}, nil
	case JadeExiting:
		// Shorts first, the long wing last.
		for i := len(st.Legs) - 1; i >= 0; i-- {
			l := &st.Legs[i]
			if l.ExitFilled >= l.EntryFilled {
				continue
			}
			inst, err := market.ParseKey(l.Key)
			if err != nil {
				return nil, err
			}
			pos, err := b.Position(ctx, inst)
			if err != nil {
				return nil, err
			}
			want := int64(l.Sign) * (l.EntryFilled - l.ExitFilled)
			if (want > 0 && pos < want) || (want < 0 && pos > want) {
				return st.halt(fmt.Sprintf("Zerodha shows %d of %s, serene expected %d", pos, l.Key, want)), nil
			}
			return nil, placeJade(ctx, b, st, snap, cfg, i, true)
		}
		st.Phase = JadeFlat
		return []JadeEvent{{Kind: "exited"}}, nil
	}
	return nil, fmt.Errorf("unknown phase %q", st.Phase)
}

// jadeLimit is a LIMIT for side priced Buffer through the touch, on the tick.
func jadeLimit(side market.Side, q JadeQuote, tick market.Paise, buf float64) (market.Paise, bool) {
	if tick <= 0 {
		tick = market.Rupees(0.05)
	}
	ref := q.Ask
	if side == market.Sell {
		ref = q.Bid
	}
	if ref <= 0 {
		ref = q.Last
		buf *= 2
	}
	if ref <= 0 {
		return 0, false
	}
	p := float64(market.Rupees(ref)) * (1 + float64(side)*buf)
	t := float64(tick)
	if side == market.Buy {
		return market.Paise(math.Ceil(p/t) * t), true
	}
	return max(tick, market.Paise(math.Floor(p/t)*t)), true
}

func placeJade(ctx context.Context, b Broker, st *JadeState, snap JadeSnap, cfg JadeConfig, i int, exit bool) error {
	l := st.Legs[i]
	side := market.Side(l.Sign)
	qty := l.Qty - l.EntryFilled
	if exit {
		side = -side
		qty = l.EntryFilled - l.ExitFilled
	}
	price, ok := jadeLimit(side, snap.Quotes[l.Key], snap.Tick, cfg.Buffer)
	if !ok {
		return fmt.Errorf("no quote for %s", l.Key)
	}
	inst, err := market.ParseKey(l.Key)
	if err != nil {
		return err
	}
	st.Seq++
	o := &JadeOrder{Leg: i, Exit: exit, Tag: fmt.Sprintf("sjl%s%d", snap.Now.In(market.IST).Format("0102"), st.Seq),
		Side: side, Qty: qty, Price: price, Placed: snap.Now}
	st.Order = o // kept before the request: if it fails, its tag is looked up next step
	id, err := b.Place(ctx, OrderReq{Instrument: inst, Side: side, Type: "LIMIT", Qty: qty, Price: price, Tag: o.Tag})
	if err != nil {
		return fmt.Errorf("place %s %s: %w", side, l.Key, err)
	}
	o.ID = id
	return nil
}

func stepJadeOrder(ctx context.Context, b Broker, st *JadeState, snap JadeSnap, cfg JadeConfig) ([]JadeEvent, error) {
	o := st.Order
	l := &st.Legs[o.Leg]
	if o.ID == "" {
		os, found, err := b.ByTag(ctx, o.Tag)
		if err != nil {
			return nil, err
		}
		if !found {
			if snap.Now.Sub(o.Placed) < 2*time.Minute {
				return nil, nil
			}
			st.Order = nil // never reached the broker: placed afresh
			return nil, nil
		}
		o.ID = os.ID
	}
	os, err := b.Order(ctx, o.ID)
	if err != nil {
		return nil, err
	}
	var evs []JadeEvent
	done := os.Status == Complete || os.Status == Cancelled || os.Status == Rejected
	if done && os.Filled > 0 {
		applyJadeFill(l, o, os.Filled, os.Average, snap.Now)
		evs = append(evs, JadeEvent{Kind: "fill", Text: fmt.Sprintf("%s %d %s @ ₹%s", o.Side, os.Filled, l.Key, os.Average)})
	}
	switch os.Status {
	case Complete:
		st.Order = nil
	case Cancelled:
		st.Order = nil // the rest is placed afresh
		evs = append(evs, JadeEvent{Kind: "note", Text: fmt.Sprintf(":warning: *jade_lizard* order %s for %s was cancelled outside serene; the rest is placed again", o.ID, l.Key)})
	case Rejected:
		st.Order = nil
		evs = append(evs, st.halt(fmt.Sprintf("Zerodha rejected the %s order for %s: %s", o.Side, l.Key, os.Message))...)
	case Open:
		if o.Mods >= cfg.MaxMods {
			if err := b.Cancel(ctx, o.ID); err != nil {
				return evs, fmt.Errorf("cancel %s: %w", o.ID, err)
			}
			return append(evs, st.halt(fmt.Sprintf("the %s order for %s didn't fill after %d reprices and was cancelled", o.Side, l.Key, o.Mods))...), nil
		}
		p, ok := jadeLimit(o.Side, snap.Quotes[l.Key], snap.Tick, cfg.Buffer)
		if !ok || p == o.Price {
			return evs, nil
		}
		inst, err := market.ParseKey(l.Key)
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

func applyJadeFill(l *JadeLeg, o *JadeOrder, filled int64, avg market.Paise, now time.Time) {
	if o.Exit {
		l.Exit = weighted(l.Exit, l.ExitFilled, avg, filled)
		l.ExitFilled += filled
		l.ExitTime = now
		return
	}
	l.Entry = weighted(l.Entry, l.EntryFilled, avg, filled)
	l.EntryFilled += filled
	l.EntryTime = now
}

func weighted(a market.Paise, na int64, b market.Paise, nb int64) market.Paise {
	if na+nb == 0 {
		return 0
	}
	return market.Paise(math.Round((float64(a)*float64(na) + float64(b)*float64(nb)) / float64(na+nb)))
}

// JadeMonthlies is each month's last expiry among exps, in order.
func JadeMonthlies(exps []market.Date) []market.Date {
	last := map[[2]int]market.Date{}
	for _, e := range exps {
		k := [2]int{e.Year, int(e.Month)}
		if cur, ok := last[k]; !ok || cur.Before(e) {
			last[k] = e
		}
	}
	var out []market.Date
	for _, e := range last {
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Before(out[b]) })
	return out
}

// EntryExpiry is the monthly expiry today may enter, if any: within EntryDTE
// days but not yet past LastEntryDTE.
func (r JadeRules) EntryExpiry(today market.Date, monthlies []market.Date) (market.Date, bool) {
	for _, e := range monthlies {
		if d := DTE(today, e); d <= r.EntryDTE && d >= r.LastEntryDTE {
			return e, true
		}
	}
	return market.Date{}, false
}
