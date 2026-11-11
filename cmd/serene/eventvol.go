package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"serene/internal/analytics"
	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/market"
	"serene/internal/strategy"
)

// Scheduled events, as the India session that first trades the news. Only
// events whose dates were public in advance are listed, so a strategy could
// have known them: surprises such as the April 2025 tariffs are left out.
//
//   - rbi: MPC decision days (announced about 10:00 IST). The 2026-04-08 date
//     was checked on rbi.org.in's monetary policy page on 2026-11-09.
//   - budget: Union Budget days. Both were weekend special sessions.
//   - fomc: the India session after a US Fed decision (about 23:30-00:00 IST),
//     so the news lands at that day's open.
var eventDays = map[string][]string{
	"rbi":    {"2025-02-07", "2025-04-09", "2025-06-06", "2025-08-06", "2025-10-01", "2025-12-05", "2026-02-06", "2026-04-08"},
	"budget": {"2025-02-01", "2026-02-01"},
	"fomc": {"2025-01-30", "2025-03-20", "2025-05-08", "2025-06-19", "2025-07-31", "2025-09-18", "2025-10-30", "2025-12-11",
		"2026-01-29", "2026-03-19", "2026-04-30"},
}

// evTiming is when a position opens and closes, in trading days from the
// event day and clock times.
type evTiming struct {
	name            string
	inDay, outDay   int
	inHHMM, outHHMM int
}

var evTimings = []evTiming{
	{"T-1 15:10 > T 09:45", -1, 0, 1510, 945},    // the overnight gap only
	{"T-1 15:10 > T 15:15", -1, 0, 1510, 1515},   // gap and the event day
	{"T 09:20 > T 11:30", 0, 0, 920, 1130},       // the morning (RBI speaks at 10:00)
	{"T 09:20 > T 15:15", 0, 0, 920, 1515},       // the event day intraday
	{"T-1 15:10 > T+1 15:15", -1, 1, 1510, 1515}, // gap, the day and its follow-through
	{"T-3 09:30 > T-1 15:10", -3, -1, 930, 1510}, // the run-up, sold before the news
}

// evShape is the strikes, in steps from at the money: the call at +offset and
// the put at -offset. 0 is a straddle, positive a strangle, negative guts.
type evShape struct {
	name   string
	offset int
}

var evShapes = []evShape{{"guts 100", -2}, {"guts 50", -1}, {"straddle", 0}, {"strangle 50", 1}, {"strangle 100", 2}, {"strangle 200", 4}}

type evCell struct{ set, timing, shape int }

type evTrade struct {
	event, in, out  market.Date
	expiry          market.Date
	ce, pe          market.Instrument
	inAt, outAt     time.Time
	ceIn, peIn      market.Paise
	ceOut, peOut    market.Paise
	spotIn, spotOut market.Paise
	net             [4]float64 // rupees after charges at 0, 1, 2 and 3x slippage
	pct             float64    // return on the debit before costs
}

// runEventVol buys straddles, strangles and guts on NIFTY around scheduled
// events, on expiry days, and on every other day as a control, one lot each,
// after charges and with slippage.
func runEventVol(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("eventvol", flag.ContinueOnError)
	underlying := fs.String("underlying", "NIFTY", "underlying name")
	from := fs.String("from", "2024-12-27", "first day")
	to := fs.String("to", "2026-04-30", "last day")
	split := fs.String("split", "2025-09-01", "first day of the second half")
	dir := fs.String("data", "data/upstox", "data directory")
	holdout := fs.String("holdout", "configs/holdout.yaml", "data held back from research")
	unseal := fs.Bool("unseal", false, "allow a range reaching the holdout; recorded")
	costsFile := fs.String("costs", "configs/costs.yaml", "charges schedule")
	slipBps := fs.Float64("slip-bps", 30, "slippage per side, at least 2 ticks")
	step := fs.Float64("step", 50, "strike step")
	out := fs.String("out", "reports/eventvol.csv", "every combination's results")
	tradesOut := fs.String("trades", "reports/eventvol_trades.csv", "every trade, or empty for none")
	if err := fs.Parse(args); err != nil {
		return err
	}
	f, err := market.ParseDate(*from)
	if err != nil {
		return err
	}
	t, err := market.ParseDate(*to)
	if err != nil {
		return err
	}
	mid, err := market.ParseDate(*split)
	if err != nil {
		return err
	}
	h, err := analytics.LoadHoldouts(*holdout)
	if err != nil {
		return err
	}
	if _, err := h.Check(*dir, t, *unseal); err != nil {
		return err
	}
	costs, err := cost.Load(*costsFile)
	if err != nil {
		return err
	}
	store, err := data.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()
	insts, err := store.Contracts(ctx, *underlying)
	if err != nil {
		return err
	}
	byKeyInst := map[string]market.Instrument{}
	expirySet := map[market.Date]bool{}
	for _, in := range insts {
		if in.Kind != market.KindOption {
			continue
		}
		byKeyInst[in.Key()] = in
		expirySet[in.Expiry] = true
	}
	var expiries []market.Date
	for e := range expirySet {
		expiries = append(expiries, e)
	}
	sort.Slice(expiries, func(i, j int) bool { return expiries[i].Before(expiries[j]) })

	// Trading days: stored weekdays NSE was open, plus listed special sessions.
	special := map[market.Date]bool{}
	sets := map[string]map[market.Date]bool{}
	for name, ds := range eventDays {
		sets[name] = map[market.Date]bool{}
		for _, s := range ds {
			d, err := market.ParseDate(s)
			if err != nil {
				return err
			}
			sets[name][d] = true
			if !market.NSETradingDay(d) {
				special[d] = true
			}
		}
	}
	stored, err := store.Days(*underlying, f.AddDays(-10), t)
	if err != nil {
		return err
	}
	var days []market.Date
	for _, d := range stored {
		if market.NSETradingDay(d) || special[d] {
			days = append(days, d)
		}
	}
	pos := map[market.Date]int{}
	for i, d := range days {
		pos[d] = i
	}
	inWindow := func(d market.Date) bool { return !d.Before(f) && !t.Before(d) }

	sets["macro"] = map[market.Date]bool{}
	for _, n := range []string{"rbi", "budget", "fomc"} {
		for d := range sets[n] {
			sets["macro"][d] = true
		}
	}
	sets["expiry"] = map[market.Date]bool{}
	for _, e := range expiries {
		if _, ok := pos[e]; ok && inWindow(e) {
			sets["expiry"][e] = true
		}
	}
	// Control: ordinary days, neither an event, an expiry, nor a special session.
	sets["control"] = map[market.Date]bool{}
	for _, d := range days {
		if inWindow(d) && !sets["macro"][d] && !sets["expiry"][d] && !special[d] {
			sets["control"][d] = true
		}
	}
	setNames := []string{"rbi", "budget", "fomc", "macro", "expiry", "control"}

	// Plan every trade's days. The cell's position is opened on the entry day
	// and closed on the exit day; both must be in the window.
	type plan struct {
		cell    evCell
		event   market.Date
		in, out market.Date
		trade   *evTrade
		skipped string
	}
	var plans []*plan
	for si, sn := range setNames {
		var evs []market.Date
		for d := range sets[sn] {
			evs = append(evs, d)
		}
		sort.Slice(evs, func(i, j int) bool { return evs[i].Before(evs[j]) })
		for ti, tm := range evTimings {
			for shi := range evShapes {
				for _, e := range evs {
					p, ok := pos[e]
					if !ok {
						continue
					}
					ii, oi := p+tm.inDay, p+tm.outDay
					if ii < 0 || oi >= len(days) || !inWindow(days[ii]) || !inWindow(days[oi]) {
						continue
					}
					plans = append(plans, &plan{cell: evCell{si, ti, shi}, event: e, in: days[ii], out: days[oi]})
				}
			}
		}
	}
	byIn, byOut := map[market.Date][]*plan{}, map[market.Date][]*plan{}
	need := map[market.Date]bool{}
	for _, p := range plans {
		byIn[p.in] = append(byIn[p.in], p)
		byOut[p.out] = append(byOut[p.out], p)
		need[p.in], need[p.out] = true, true
	}
	var load []market.Date
	for d := range need {
		load = append(load, d)
	}
	sort.Slice(load, func(i, j int) bool { return load[i].Before(load[j]) })

	at := func(d market.Date, hhmm int) time.Time { return d.At(hhmm/100, hhmm%100) }
	// traded is the first bar starting at or after tm that traded, within limit.
	traded := func(bars []market.Bar, tm time.Time, limit time.Duration) (market.Bar, bool) {
		i := sort.Search(len(bars), func(i int) bool { return !bars[i].Time.Before(tm) })
		for ; i < len(bars) && bars[i].Time.Sub(tm) <= limit; i++ {
			if bars[i].Volume > 0 {
				return bars[i], true
			}
		}
		return market.Bar{}, false
	}
	// lastTraded is the last bar before tm that traded.
	lastTraded := func(bars []market.Bar, tm time.Time) (market.Bar, bool) {
		i := sort.Search(len(bars), func(i int) bool { return !bars[i].Time.Before(tm) })
		for i--; i >= 0; i-- {
			if bars[i].Volume > 0 {
				return bars[i], true
			}
		}
		return market.Bar{}, false
	}
	// spotAt is the index close of the minute ending at tm, or the last before it.
	spotAt := func(idx []market.Bar, tm time.Time) (market.Paise, bool) {
		i := sort.Search(len(idx), func(i int) bool { return idx[i].End.After(tm) })
		if i == 0 {
			return 0, false
		}
		return idx[i-1].Close, true
	}
	slip := func(p market.Paise, buy bool, k float64) market.Paise {
		s := max(market.Paise(math.Round(float64(p)*k**slipBps/1e4)), market.Paise(10*k))
		if buy {
			return p + s
		}
		return max(p-s, 5)
	}
	charge := func(inst market.Instrument, p market.Paise, side market.Side, tm time.Time) (float64, error) {
		c, err := costs.Charges(market.Fill{Instrument: inst, Side: side, Qty: inst.LotSize, Price: p, Time: tm, Kind: market.Traded})
		return c.Rupees(), err
	}
	optKey := func(exp market.Date, strike market.Paise, r market.Right) string {
		return market.Option(market.NFO, *underlying, exp, strike, r, 0).Key()
	}

	indexKey := market.Index(market.NSE, *underlying).Key()
	skips := map[string]int{}
	start := time.Now()
	for li, d := range load {
		all, err := store.DayBars(ctx, *underlying, d)
		if err != nil {
			return err
		}
		bars := map[string][]market.Bar{}
		for _, b := range all {
			bars[b.Key] = append(bars[b.Key], b)
		}
		idx := bars[indexKey]
		for _, p := range byIn[d] {
			tm, sh := evTimings[p.cell.timing], evShapes[p.cell.shape]
			inAt := at(d, tm.inHHMM)
			spot, ok := spotAt(idx, inAt)
			if !ok {
				p.skipped = "no index at entry"
				continue
			}
			atm := market.Rupees(math.Round(spot.Rupees() / *step) * *step)
			off := market.Rupees(float64(sh.offset) * *step)
			ceStrike, peStrike := atm+off, atm-off
			// The nearest expiry still alive at the exit, quoted today at both strikes.
			var exp market.Date
			found := false
			for _, e := range expiries {
				if e.Before(p.out) {
					continue
				}
				if len(bars[optKey(e, ceStrike, market.Call)]) > 0 && len(bars[optKey(e, peStrike, market.Put)]) > 0 {
					exp, found = e, true
					break
				}
			}
			if !found {
				p.skipped = "no expiry quoted"
				continue
			}
			ck, pk := optKey(exp, ceStrike, market.Call), optKey(exp, peStrike, market.Put)
			cb, ok1 := traded(bars[ck], inAt, 3*time.Minute)
			pb, ok2 := traded(bars[pk], inAt, 3*time.Minute)
			if !ok1 || !ok2 {
				p.skipped = "no fill at entry"
				continue
			}
			p.trade = &evTrade{event: p.event, in: p.in, out: p.out, expiry: exp,
				ce: byKeyInst[ck], pe: byKeyInst[pk], inAt: later(cb.Time, pb.Time),
				ceIn: cb.Open, peIn: pb.Open, spotIn: spot}
		}
		for _, p := range byOut[d] {
			tr := p.trade
			if tr == nil {
				continue
			}
			tm := evTimings[p.cell.timing]
			outAt := at(d, tm.outHHMM)
			if !p.in.Before(d) && !outAt.After(tr.inAt) {
				p.skipped, p.trade = "exit before entry", nil
				continue
			}
			var legs [2]market.Bar
			for i, k := range []string{tr.ce.Key(), tr.pe.Key()} {
				b, ok := traded(bars[k], outAt, 10*time.Minute)
				if !ok {
					// A far strike that stopped trading: take its last trade.
					if b, ok = lastTraded(bars[k], outAt); ok {
						b.Open = b.Close
					}
				}
				if !ok {
					p.skipped, p.trade = "no fill at exit", nil
					break
				}
				legs[i] = b
			}
			if p.trade == nil {
				continue
			}
			tr.ceOut, tr.peOut = legs[0].Open, legs[1].Open
			tr.outAt = later(legs[0].Time, legs[1].Time)
			tr.spotOut, _ = spotAt(idx, outAt)
			for k := 0; k < 4; k++ {
				kk := float64(k)
				var n float64
				for _, leg := range []struct {
					inst    market.Instrument
					in, out market.Paise
				}{{tr.ce, tr.ceIn, tr.ceOut}, {tr.pe, tr.peIn, tr.peOut}} {
					in, out := leg.in, leg.out
					if k > 0 {
						in, out = slip(in, true, kk), slip(out, false, kk)
					}
					cb, err := charge(leg.inst, in, market.Buy, tr.inAt)
					if err != nil {
						return err
					}
					cs, err := charge(leg.inst, out, market.Sell, tr.outAt)
					if err != nil {
						return err
					}
					n += (out-in).Rupees()*float64(leg.inst.LotSize) - cb - cs
				}
				tr.net[k] = n
			}
			debit := (tr.ceIn + tr.peIn).Rupees()
			tr.pct = ((tr.ceOut+tr.peOut).Rupees()/debit - 1) * 100
		}
		if (li+1)%50 == 0 {
			fmt.Fprintf(os.Stderr, "  %d/%d days, %s\n", li+1, len(load), time.Since(start).Round(time.Second))
		}
	}
	results := map[evCell][]*evTrade{}
	for _, p := range plans {
		if p.trade != nil && !p.trade.outAt.IsZero() {
			results[p.cell] = append(results[p.cell], p.trade)
		} else if p.skipped != "" {
			skips[p.skipped]++
		} else {
			skips["not closed"]++
		}
	}

	type sum struct {
		n                    int
		win, avgPct, avgMove float64
		net                  [4]float64
		pf, dd, h1, h2       float64
		avgDebit, movePerBE  float64
		n1, n2               int
	}
	summarize := func(ts []*evTrade) sum {
		var s sum
		var w, l, peak, cum float64
		for _, tr := range ts {
			s.n++
			for k := range s.net {
				s.net[k] += tr.net[k]
			}
			if tr.net[1] > 0 {
				s.win++
				w += tr.net[1]
			} else {
				l -= tr.net[1]
			}
			if tr.in.Before(mid) {
				s.h1 += tr.net[1]
				s.n1++
			} else {
				s.h2 += tr.net[1]
				s.n2++
			}
			cum += tr.net[1]
			peak = max(peak, cum)
			s.dd = max(s.dd, peak-cum)
			s.avgPct += tr.pct
			debit := (tr.ceIn + tr.peIn).Rupees()
			move := math.Abs((tr.spotOut - tr.spotIn).Rupees())
			s.avgDebit += debit
			s.avgMove += move
			s.movePerBE += move / debit
		}
		s.pf = math.Inf(1)
		if l > 0 {
			s.pf = w / l
		}
		if s.n > 0 {
			fn := float64(s.n)
			s.win = s.win / fn * 100
			s.avgPct /= fn
			s.avgDebit /= fn
			s.avgMove /= fn
			s.movePerBE /= fn
		}
		return s
	}

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	fh, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer fh.Close()
	w := csv.NewWriter(fh)
	w.Write([]string{"events", "timing", "shape", "trades", "win_pct", "avg_pct", "avg_debit_pts", "avg_move_pts", "move_over_debit",
		"net_rs", "net_slip_rs", "net_slip2_rs", "net_slip3_rs", "pf_slip", "max_dd_slip_rs", "h1_trades", "h1_net_slip_rs", "h2_trades", "h2_net_slip_rs"})
	ff := func(x float64, p int) string { return strconv.FormatFloat(x, 'f', p, 64) }

	fmt.Printf("%s %s to %s. Long one lot of each leg, after charges; slip = %.0f bps a side (2 ticks minimum). Halves split at %s.\n",
		*underlying, f, t, *slipBps, mid)
	fmt.Printf("Events in window: ")
	for _, sn := range setNames {
		fmt.Printf("%s %d  ", sn, len(sets[sn]))
	}
	fmt.Printf("\nmove/debit = |index move| over the premium paid, both in points; above 1 the move beat a straddle's breakeven.\n")
	both, cells := 0, 0
	for si, sn := range setNames {
		fmt.Printf("\n== %s ==\n", sn)
		fmt.Printf("%-24s %-12s | %4s %5s %7s %6s %6s %5s | %9s %9s %9s %5s %8s | %9s %9s\n",
			"timing", "shape", "n", "win%", "avg%", "debit", "move", "m/d", "net 0x", "net 1x", "net 2x", "PF1x", "maxDD", "h1 1x", "h2 1x")
		for ti, tm := range evTimings {
			for shi, sh := range evShapes {
				ts := results[evCell{si, ti, shi}]
				if len(ts) == 0 {
					continue
				}
				s := summarize(ts)
				cells++
				if s.h1 > 0 && s.h2 > 0 && s.n1 > 0 && s.n2 > 0 {
					both++
				}
				fmt.Printf("%-24s %-12s | %4d %5.1f %7.1f %6.0f %6.0f %5.2f | %9.0f %9.0f %9.0f %5.2f %8.0f | %9.0f %9.0f\n",
					tm.name, sh.name, s.n, s.win, s.avgPct, s.avgDebit, s.avgMove, s.movePerBE, s.net[0], s.net[1], s.net[2], s.pf, s.dd, s.h1, s.h2)
				w.Write([]string{sn, tm.name, sh.name, strconv.Itoa(s.n), ff(s.win, 1), ff(s.avgPct, 2), ff(s.avgDebit, 1), ff(s.avgMove, 1), ff(s.movePerBE, 3),
					ff(s.net[0], 0), ff(s.net[1], 0), ff(s.net[2], 0), ff(s.net[3], 0), ff(s.pf, 3), ff(s.dd, 0),
					strconv.Itoa(s.n1), ff(s.h1, 0), strconv.Itoa(s.n2), ff(s.h2, 0)})
			}
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	fmt.Printf("\n%d of %d combinations made money after slippage in both halves. Skipped: %v\n", both, cells, skips)

	if *tradesOut != "" {
		th, err := os.Create(*tradesOut)
		if err != nil {
			return err
		}
		defer th.Close()
		tw := csv.NewWriter(th)
		tw.Write([]string{"events", "timing", "shape", "event", "entry", "exit", "expiry", "ce", "pe", "spot_in", "spot_out",
			"ce_in", "pe_in", "ce_out", "pe_out", "pct", "net_rs", "net_slip_rs", "net_slip2_rs"})
		for si, sn := range setNames {
			for ti, tm := range evTimings {
				for shi, sh := range evShapes {
					for _, tr := range results[evCell{si, ti, shi}] {
						tw.Write([]string{sn, tm.name, sh.name, tr.event.String(), tr.inAt.In(market.IST).Format("2006-01-02 15:04"), tr.outAt.In(market.IST).Format("2006-01-02 15:04"),
							tr.expiry.String(), tr.ce.Strike.String(), tr.pe.Strike.String(), tr.spotIn.String(), tr.spotOut.String(),
							tr.ceIn.String(), tr.peIn.String(), tr.ceOut.String(), tr.peOut.String(), ff(tr.pct, 2), ff(tr.net[0], 2), ff(tr.net[1], 2), ff(tr.net[2], 2)})
					}
				}
			}
		}
		tw.Flush()
		if err := tw.Error(); err != nil {
			return err
		}
	}

	var net market.Paise
	n := 0
	for _, ts := range results {
		for _, tr := range ts {
			net += market.Rupees(tr.net[1])
			n++
		}
	}
	err = analytics.AppendTrials(analytics.LedgerPath(*dir), []analytics.TrialRecord{{
		Time: time.Now(), Command: "research eventvol", Strategy: "event_long_vol", Underlying: *underlying,
		Params: strategy.Params{"slip_bps": *slipBps, "step": *step}, From: f, To: t, Count: cells, Unsealed: *unseal, Net: net, Trades: n,
	}})
	if err != nil {
		return fmt.Errorf("trial ledger: %w", err)
	}
	fmt.Printf("all %d combinations: %s (recorded in the trial ledger)\n", cells, *out)
	return nil
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
