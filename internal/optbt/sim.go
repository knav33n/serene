package optbt

import (
	"fmt"
	"math"
	"time"

	"serene/internal/cost"
	"serene/internal/market"
)

// Trade is one round trip of one contract.
type Trade struct {
	Inst          market.Instrument
	Hedge         bool // bought wing; otherwise a sold leg
	Entry, Exit   market.Paise
	EntryT, ExitT time.Time
	Reason        string // sl, sqoff, exit
}

// DayResult is one cell's outcome on one day with a trade.
type DayResult struct {
	Date      market.Date
	Ord, DTE  int
	Expiry    market.Date
	Spot      market.Paise // index at entry
	Gross     float64      // ₹ before costs and slippage
	Net       float64      // after costs
	NetSlip   float64      // after costs and slippage
	Margin    float64
	CESL      bool
	PESL      bool
	Reentries int
	MaxProfit float64 // best and worst minute-close MTM, gross ₹
	MaxLoss   float64
	Trades    []Trade // only with Env.Detail
}

// Env holds run settings and per-worker scratch. Not safe for concurrent use.
type Env struct {
	Costs    *cost.Schedule
	SlipBps  float64
	NakedPct float64
	Detail   bool
	trades   []Trade
}

type bar4 struct{ o, h, l, c market.Paise }

const (
	sWait = iota
	sOpen
	sPendCost
	sPendReexec
	sDone
)

type leg struct {
	right    market.Right
	o, h     *opt
	ser      [2]*series
	hser     [2]*series
	last     bar4
	hlast    bar4
	state    int
	trig     market.Paise
	entry    market.Paise
	stop     market.Paise
	low      market.Paise
	hentry   market.Paise
	entT     time.Time
	hentT    time.Time
	hopen    bool
	reUsed   int
	justIn   bool
	stopped  bool // any stop on this side today
	realised market.Paise
}

var costFloor = time.Date(2024, 10, 1, 10, 0, 0, 0, market.IST)

func (e *Env) charge(inst market.Instrument, side market.Side, qty int64, p market.Paise, at time.Time) market.Paise {
	if at.Before(costFloor) {
		at = costFloor
	}
	c, err := e.Costs.Charges(market.Fill{Instrument: inst, Side: side, Qty: qty, Price: p, Time: at, Kind: market.Traded})
	if err != nil {
		panic(fmt.Sprintf("optbt: charges: %v", err))
	}
	return c
}

func (e *Env) slip(p market.Paise, buy bool) market.Paise {
	s := max(market.Paise(math.Round(float64(p)*e.SlipBps/1e4)), 10)
	if buy {
		return p + s
	}
	return max(p-s, 5)
}

func seriesOf(d *Day, k optKey) *series {
	if d == nil {
		return nil
	}
	if o := d.opts[k]; o != nil {
		return o.s
	}
	return nil
}

func step(s *series, slot int, last bar4) bar4 {
	if o, h, l, c, ok := s.bar(slot); ok {
		return bar4{o, h, l, c}
	}
	return bar4{last.c, last.c, last.c, last.c}
}

// Run simulates cell c on day d; next is the following trading day, needed
// for STBT. It reports false when the cell does not trade that day.
func (e *Env) Run(c *Cell, d, next *Day) (DayResult, bool) {
	if c.DTE >= 0 && d.DTE != c.DTE {
		return DayResult{}, false
	}
	exp := d.nearest(c.STBT)
	if exp.IsZero() || (c.STBT && next == nil) || !d.Index.has[c.Entry] {
		return DayResult{}, false
	}
	if c.skipMask != 0 {
		ev := d.Events
		if c.STBT {
			ev |= next.Events
		}
		if ev&c.skipMask != 0 {
			return DayResult{}, false
		}
	}
	es := c.Entry
	var legs [2]leg
	n := 0
	for _, r := range [2]market.Right{market.Call, market.Put} {
		if (c.Side == "ce" && r != market.Call) || (c.Side == "pe" && r != market.Put) {
			continue
		}
		o := d.pick(c.Strike, c.Round, r, exp, es)
		if o == nil {
			return DayResult{}, false
		}
		l := &legs[n]
		*l = leg{right: r}
		e.setLeg(l, c, o, d, next)
		if c.Hedge.Kind != "" {
			h := d.hedge(c.Hedge, o.inst.Strike, r, exp, es)
			if h == nil {
				return DayResult{}, false
			}
			e.setHedge(l, c, h, d, next)
		}
		n++
	}
	lot := legs[0].o.inst.LotSize
	spot := d.Index.o[es]
	res := DayResult{Date: d.Date, Ord: d.Ord, DTE: d.DTE, Expiry: exp, Spot: spot}
	nakedPU := market.Paise(float64(spot) * e.NakedPct / 100)
	for i := range n {
		pu := nakedPU
		if l := &legs[i]; l.h != nil {
			risk := l.h.inst.Strike - l.o.inst.Strike
			if risk < 0 {
				risk = -risk
			}
			pu = min(pu, risk)
		}
		// SPAN charges a short straddle about one side's margin
		res.Margin = max(res.Margin, (pu * market.Paise(lot)).Rupees())
	}
	e.trades = e.trades[:0]
	type span struct {
		day      *Day
		p        int
		from, to int
	}
	spans := []span{{d, 0, es, c.Exit}}
	if c.STBT {
		spans = []span{{d, 0, es, max(d.Close, es+1)}, {next, 1, 0, c.Exit}}
	}
	startT := d.Date.At(9, 15).Add(time.Duration(es) * time.Minute)
	for i := range n {
		l := &legs[i]
		l.last = bar4{l.o.s.o[es], l.o.s.o[es], l.o.s.o[es], l.o.s.c[es]}
		if l.h != nil {
			l.hlast = bar4{l.h.s.o[es], l.h.s.o[es], l.h.s.o[es], l.h.s.c[es]}
		}
		if c.Wait != 0 {
			l.state = sWait
			l.trig = market.Paise(math.Round(float64(l.o.s.o[es]) * (1 + c.Wait/100)))
			continue
		}
		e.enter(l, c, l.o.s.o[es], startT)
		if l.h != nil {
			e.buyHedge(l, l.h.s.o[es], startT)
		}
	}
	var lastAt time.Time
	done := false
	for _, sp := range spans {
		if done {
			break
		}
		base := sp.day.Date.At(9, 15)
		for s := sp.from; s < sp.to && !done; s++ {
			tm := base.Add(time.Duration(s) * time.Minute)
			at := tm.Add(time.Minute)
			lastAt = at
			for i := range n {
				l := &legs[i]
				l.last = step(l.ser[sp.p], s, l.last)
				if l.h != nil {
					l.hlast = step(l.hser[sp.p], s, l.hlast)
				}
				l.justIn = false
			}
			// re-execute: a fresh strike by the same rule, sold at this bar's open
			for i := range n {
				l := &legs[i]
				if l.state != sPendReexec {
					continue
				}
				o := sp.day.pick(c.Strike, c.Round, l.right, exp, s)
				if o == nil {
					continue
				}
				var h *opt
				if c.Hedge.Kind != "" {
					if h = sp.day.hedge(c.Hedge, o.inst.Strike, l.right, exp, s); h == nil {
						continue
					}
				}
				var nx *Day
				if sp.p == 0 {
					nx = next
				}
				e.setLegAt(l, o, sp.p, sp.day, nx)
				l.last = bar4{o.s.o[s], o.s.h[s], o.s.l[s], o.s.c[s]}
				l.reUsed++
				e.enter(l, c, l.last.o, tm)
				if h != nil {
					e.setHedgeAt(l, h, sp.p, sp.day, nx)
					l.hlast = bar4{h.s.o[s], h.s.h[s], h.s.l[s], h.s.c[s]}
					e.buyHedge(l, l.hlast.o, tm)
				}
			}
			// wait-and-trade and re-entry at cost: filled inside the bar,
			// so their stop is checked from the next bar
			for i := range n {
				l := &legs[i]
				switch l.state {
				case sWait:
					if c.Wait < 0 && l.last.l > 0 && l.last.l <= l.trig {
						e.enter(l, c, min(l.trig, l.last.o), tm)
					} else if c.Wait > 0 && l.last.h >= l.trig {
						e.enter(l, c, max(l.trig, l.last.o), tm)
					} else {
						continue
					}
				case sPendCost:
					if l.last.l > 0 && l.last.l <= l.trig {
						l.reUsed++
						e.enter(l, c, min(l.trig, l.last.o), tm)
					} else {
						continue
					}
				default:
					continue
				}
				l.justIn = true
				if l.h != nil {
					e.buyHedge(l, l.hlast.c, tm)
				}
			}
			if c.SL > 0 {
				stopped := false
				for i := range n {
					l := &legs[i]
					if l.state == sOpen && !l.justIn && l.last.h >= l.stop {
						e.close(l, c, max(l.stop, l.last.o), at, "sl")
						l.stopped = true
						stopped = true
					}
				}
				if stopped {
					for i := range n {
						l := &legs[i]
						switch c.SLMode {
						case "cost":
							if l.state == sOpen {
								l.stop = min(l.stop, l.entry)
							}
						case "all":
							if l.state == sOpen {
								e.close(l, c, l.last.c, at, "sqoff")
							} else if l.state == sWait {
								l.state = sDone
							}
						}
					}
				}
				if c.TrailX > 0 {
					for i := range n {
						l := &legs[i]
						if l.state != sOpen || l.justIn {
							continue
						}
						l.low = min(l.low, l.last.l)
						k := math.Floor(float64(l.entry-l.low) / float64(l.entry) * 100 / c.TrailX)
						if k >= 1 {
							ts := market.Paise(math.Round(float64(l.entry) * (1 + (c.SL-k*c.TrailY)/100)))
							l.stop = min(l.stop, ts)
						}
					}
				}
			}
			var mtm market.Paise
			all := true
			for i := range n {
				l := &legs[i]
				mtm += l.realised
				if l.state == sOpen {
					mtm += l.entry - l.last.c
				}
				if l.hopen {
					mtm += l.hlast.c - l.hentry
				}
				if l.state != sDone {
					all = false
				}
			}
			r := mtm.Rupees() * float64(lot)
			res.MaxProfit, res.MaxLoss = max(res.MaxProfit, r), min(res.MaxLoss, r)
			done = all
		}
	}
	exitT := lastAt
	if !done {
		last := spans[len(spans)-1]
		exitT = last.day.Date.At(9, 15).Add(time.Duration(last.to) * time.Minute)
	}
	for i := range n {
		l := &legs[i]
		if l.state == sOpen {
			e.close(l, c, l.last.c, exitT, "exit")
		}
		if l.stopped {
			if l.right == market.Call {
				res.CESL = true
			} else {
				res.PESL = true
			}
		}
		res.Reentries += l.reUsed
	}
	if len(e.trades) == 0 {
		return DayResult{}, false
	}
	for _, t := range e.trades {
		q := t.Inst.LotSize
		sellP, buyP, sellT, buyT := t.Entry, t.Exit, t.EntryT, t.ExitT
		if t.Hedge {
			sellP, buyP, sellT, buyT = t.Exit, t.Entry, t.ExitT, t.EntryT
		}
		g := (sellP - buyP) * market.Paise(q)
		cs := e.charge(t.Inst, market.Sell, q, sellP, sellT) + e.charge(t.Inst, market.Buy, q, buyP, buyT)
		res.Gross += g.Rupees()
		res.Net += (g - cs).Rupees()
		ss, sb := e.slip(sellP, false), e.slip(buyP, true)
		cs2 := e.charge(t.Inst, market.Sell, q, ss, sellT) + e.charge(t.Inst, market.Buy, q, sb, buyT)
		res.NetSlip += ((ss-sb)*market.Paise(q) - cs2).Rupees()
	}
	if e.Detail {
		res.Trades = append([]Trade(nil), e.trades...)
	}
	return res, true
}

func (e *Env) setLeg(l *leg, c *Cell, o *opt, d, next *Day) {
	var nx *Day
	if c.STBT {
		nx = next
	}
	e.setLegAt(l, o, 0, d, nx)
}

// setLegAt points a leg at contract o, found on day (phase p); next is the
// following day when the position may be carried into it.
func (e *Env) setLegAt(l *leg, o *opt, p int, day, next *Day) {
	l.o = o
	l.ser = [2]*series{}
	l.ser[p] = o.s
	if p == 0 && next != nil {
		l.ser[1] = seriesOf(next, o.key)
	}
}

func (e *Env) setHedge(l *leg, c *Cell, h *opt, d, next *Day) {
	var nx *Day
	if c.STBT {
		nx = next
	}
	e.setHedgeAt(l, h, 0, d, nx)
}

func (e *Env) setHedgeAt(l *leg, h *opt, p int, day, next *Day) {
	l.h = h
	l.hser = [2]*series{}
	l.hser[p] = h.s
	if p == 0 && next != nil {
		l.hser[1] = seriesOf(next, h.key)
	}
}

func (e *Env) enter(l *leg, c *Cell, px market.Paise, at time.Time) {
	l.state = sOpen
	l.entry, l.low, l.entT = px, px, at
	l.stop = math.MaxInt64 / 4
	if c.SL > 0 {
		l.stop = market.Paise(math.Round(float64(px) * (1 + c.SL/100)))
	}
}

func (e *Env) buyHedge(l *leg, px market.Paise, at time.Time) {
	l.hopen, l.hentry, l.hentT = true, px, at
}

// close buys back a sold leg (and sells its wing), then arms a re-entry if
// the cell has one left.
func (e *Env) close(l *leg, c *Cell, px market.Paise, at time.Time, why string) {
	e.trades = append(e.trades, Trade{Inst: l.o.inst, Entry: l.entry, Exit: px, EntryT: l.entT, ExitT: at, Reason: why})
	l.realised += l.entry - px
	if l.hopen {
		e.trades = append(e.trades, Trade{Inst: l.h.inst, Hedge: true, Entry: l.hentry, Exit: l.hlast.c, EntryT: l.hentT, ExitT: at, Reason: why})
		l.realised += l.hlast.c - l.hentry
		l.hopen = false
	}
	l.state = sDone
	if why == "exit" || l.reUsed >= c.Reentry {
		return
	}
	if c.ReentryMode == "reexec" {
		l.state = sPendReexec
	} else {
		l.state, l.trig = sPendCost, l.entry
	}
}
