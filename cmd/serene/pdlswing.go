package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"serene/internal/analytics"
	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/indicator"
	"serene/internal/market"
	"serene/internal/paper"
	"serene/internal/strategy"
)

// The "holy grail of options buying" put swing from r/NSEbets: buy a put the
// moment the previous day's low breaks, after a green (<1%), narrow-range or
// inside day, with the index below its 20-day EMA. Hold up to 3 days or until
// the last pivot low is hit. Variants were fixed before the first run.

var pdlStrikeRules = []string{"atm", "itm1", "itm2", "cap400"}
var pdlExitRules = []string{"day", "3d", "3d_pivot"}

type pdlVariant struct {
	strike, exit string
	add          bool
}

func (v pdlVariant) String() string {
	s := v.strike + "/" + v.exit
	if v.add {
		s += "+add"
	}
	return s
}

func pdlVariants() []pdlVariant {
	var vs []pdlVariant
	for _, s := range pdlStrikeRules {
		for _, e := range pdlExitRules {
			vs = append(vs, pdlVariant{s, e, false})
		}
	}
	return append(vs, pdlVariant{"itm2", "3d_pivot", true})
}

type pdlDay = paper.PDLDay

func pdlSetup(days []pdlDay, ema []float64, y int) bool { return paper.PDLSetup(days, ema, y) }

// pdlPivot is the most recent daily fractal low (below the two days either
// side) confirmed by the close of y and below level, or 0 if none in 120 days.
func pdlPivot(days []pdlDay, y int, level float64) float64 {
	for j := y - 2; j >= 2 && j >= y-120; j-- {
		l := days[j].L
		if l < level && l < days[j-1].L && l < days[j-2].L && l < days[j+1].L && l < days[j+2].L {
			return l
		}
	}
	return 0
}

type pdlIdxTrade struct {
	d         market.Date
	exit, why string
	pts       float64
}

// pdlIndexTrades runs the signal on daily index bars, short the index in
// points: entry at the lower of the open and the previous low.
func pdlIndexTrades(days []pdlDay, ema []float64, from, to market.Date, hold int) []pdlIdxTrade {
	var out []pdlIdxTrade
	for y := 0; y+1 < len(days); y++ {
		D := y + 1
		if days[D].D.Before(from) || to.Before(days[D].D) || !pdlSetup(days, ema, y) {
			continue
		}
		pdl := days[y].L
		if days[D].L >= pdl {
			continue
		}
		if D+hold >= len(days) || to.Before(days[D+hold].D) {
			continue
		}
		entry := math.Min(days[D].O, pdl)
		piv := pdlPivot(days, y, entry)
		out = append(out, pdlIdxTrade{days[D].D, "day", "time", entry - days[D].C})
		out = append(out, pdlIdxTrade{days[D].D, "3d", "time", entry - days[D+hold].C})
		px, why := days[D+hold].C, "time"
		for k := D; k <= D+hold && piv > 0; k++ {
			if days[k].L <= piv {
				px, why = piv, "pivot"
				if k > D && days[k].O < piv {
					px = days[k].O
				}
				break
			}
		}
		out = append(out, pdlIdxTrade{days[D].D, "3d_pivot", why, entry - px})
	}
	return out
}

type pdlTrade struct {
	variant         string
	draw            int // -1: the rule
	day             market.Date
	key, leg, why   string
	in, out         time.Time
	entry, ext      market.Paise
	lot             int64
	net, slip, sl2x float64
}

// pdlLeg is an open option position, stepped one day at a time.
type pdlLeg struct {
	variant   string
	draw      int
	day       market.Date
	leg       string
	inst      market.Instrument
	key       string
	in        time.Time
	entry     market.Paise
	exitDay   market.Date
	pivot     float64 // exit when the index trades at or below (0: none)
	stopAbove float64 // exit when the index closes above (0: none)
	pending   bool    // exit triggered with no option bar left that day
	why       string
	// add: on addDay, buy the same strike again when the index breaks addLevel.
	addDay   market.Date
	addLevel float64
}

func runPDLSwing(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pdlswing", flag.ContinueOnError)
	underlying := fs.String("underlying", "NIFTY", "underlying name")
	from := fs.String("from", "2024-12-27", "first entry day for option trades")
	to := fs.String("to", "2026-04-30", "last day")
	split := fs.String("split", "2025-09-01", "first day of the second half")
	idxFrom := fs.String("index-from", "2012-04-01", "first entry day for the daily index test")
	dir := fs.String("data", "data/upstox", "data directory")
	marketDir := fs.String("market", "data/market", "NSE index history")
	holdout := fs.String("holdout", "configs/holdout.yaml", "data held back from research")
	costsFile := fs.String("costs", "configs/costs.yaml", "charges schedule")
	slipBps := fs.Float64("slip-bps", 30, "slippage per side, at least 2 ticks")
	step := fs.Float64("step", 50, "strike step")
	capPrem := fs.Float64("cap", 400, "cap400: deepest ITM put at or below this premium")
	hold := fs.Int("hold", 3, "3d exits: trading days after entry")
	minDays := fs.Int("min-days", 4, "expiry must be at least this many trading days after entry")
	draws := fs.Int("draws", 200, "random-entry draws")
	seed := fs.Int64("seed", 1, "random seed")
	tradesOut := fs.String("trades", "reports/pdlswing_trades.csv", "every rule trade")
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
	ifrom, err := market.ParseDate(*idxFrom)
	if err != nil {
		return err
	}
	h, err := analytics.LoadHoldouts(*holdout)
	if err != nil {
		return err
	}
	if _, err := h.Check(*dir, t, false); err != nil {
		return err
	}
	costs, err := cost.Load(*costsFile)
	if err != nil {
		return err
	}
	idxName := map[string]string{"NIFTY": "Nifty 50", "BANKNIFTY": "Nifty Bank"}[*underlying]
	if idxName == "" {
		return fmt.Errorf("no daily index history for %s", *underlying)
	}

	// Daily index bars, for the setup, the EMA and pivots.
	ms, err := data.Open(*marketDir)
	if err != nil {
		return err
	}
	rows, err := ms.IndexDays(ctx, market.Date{Year: 2011, Month: 1, Day: 1}, t)
	ms.Close()
	if err != nil {
		return err
	}
	var days []pdlDay
	var dbars []market.Bar
	for _, r := range rows {
		if r.Name != idxName {
			continue
		}
		days = append(days, pdlDay{D: r.Date, O: r.Open.Rupees(), H: r.High.Rupees(), L: r.Low.Rupees(), C: r.Close.Rupees()})
		dbars = append(dbars, market.Bar{Close: r.Close})
	}
	ema := indicator.EMA(dbars, 20)
	dayIdx := map[market.Date]int{}
	for i, d := range days {
		dayIdx[d.D] = i
	}

	// Daily index test over the long history.
	itr := pdlIndexTrades(days, ema, ifrom, t, *hold)
	printPDLIndex(itr, *underlying, ifrom, t)

	store, err := data.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()
	insts, err := store.Contracts(ctx, *underlying)
	if err != nil {
		return err
	}
	lots := map[string]market.Instrument{}
	for _, in := range insts {
		lots[in.Key()] = in
	}
	allDays, err := store.Days(*underlying, f.AddDays(-10), t.AddDays(45))
	if err != nil {
		return err
	}
	pos := map[market.Date]int{}
	for i, d := range allDays {
		pos[d] = i
	}
	last := -1
	for i, d := range allDays {
		if !t.Before(d) {
			last = i
		}
	}
	tdays := func(a, b market.Date) int { // trading days after a up to b
		i, ok := pos[a]
		if !ok {
			return -1
		}
		j := sort.Search(len(allDays), func(k int) bool { return !allDays[k].Before(b) })
		if j == len(allDays) {
			return len(allDays) - 1 - i + int(b.At(12, 0).Sub(allDays[len(allDays)-1].At(12, 0)).Hours()/24*5/7)
		}
		if allDays[j] != b {
			j-- // b is a holiday: count up to the trading day before
		}
		return j - i
	}
	// putExp lists, per strike, the expiries of puts with bars today.
	var putExp map[market.Paise][]market.Date
	indexPuts := func(byKey map[string][]market.Bar) {
		putExp = map[market.Paise][]market.Date{}
		for key, bars := range byKey {
			in, ok := lots[key]
			if !ok || len(bars) == 0 || in.Kind != market.KindOption || in.Right != market.Put {
				continue
			}
			putExp[in.Strike] = append(putExp[in.Strike], in.Expiry)
		}
	}
	expiryFor := func(d market.Date, strike market.Paise) (market.Date, bool) {
		var best market.Date
		found := false
		for _, e := range putExp[strike] {
			if e.Before(d) || tdays(d, e) < *minDays {
				continue
			}
			if !found || e.Before(best) {
				best, found = e, true
			}
		}
		return best, found
	}

	slipBy := func(p market.Paise, buy bool, bps float64) market.Paise {
		s := max(market.Paise(math.Round(float64(p)*bps/1e4)), 10)
		if buy {
			return p + s
		}
		return max(p-s, 5)
	}
	roundTrip := func(inst market.Instrument, e, x market.Paise, tin, tout time.Time) (float64, error) {
		cb, err := costs.Charges(market.Fill{Instrument: inst, Side: market.Buy, Qty: inst.LotSize, Price: e, Time: tin, Kind: market.Traded})
		if err != nil {
			return 0, err
		}
		cs, err := costs.Charges(market.Fill{Instrument: inst, Side: market.Sell, Qty: inst.LotSize, Price: x, Time: tout, Kind: market.Traded})
		if err != nil {
			return 0, err
		}
		return ((x-e)*market.Paise(inst.LotSize) - cb - cs).Rupees(), nil
	}
	// optAt is the first bar at or after t; else the day's last bar (stale).
	optAt := func(bars []market.Bar, at time.Time) (market.Paise, time.Time, bool) {
		j := sort.Search(len(bars), func(k int) bool { return !bars[k].Time.Before(at) })
		if j < len(bars) {
			return bars[j].Open, bars[j].Time, true
		}
		if len(bars) > 0 {
			b := bars[len(bars)-1]
			return b.Close, b.End, true
		}
		return 0, time.Time{}, false
	}
	optCloseAt := func(bars []market.Bar, at time.Time) (market.Paise, bool) {
		j := sort.Search(len(bars), func(k int) bool { return bars[k].Time.After(at) })
		if j == 0 {
			return 0, false
		}
		return bars[j-1].Close, true
	}

	// Candidate entry days from the daily bars, for sizing the random draws.
	var cands []market.Date
	var eligibleDays []market.Date
	for i := pos[f]; i >= 0 && i <= last-*hold; i++ {
		d := allDays[i]
		if d.Before(f) || i == 0 {
			continue
		}
		eligibleDays = append(eligibleDays, d)
		D, ok := dayIdx[d]
		if !ok || D == 0 || days[D-1].D != allDays[i-1] {
			continue
		}
		if pdlSetup(days, ema, D-1) && days[D].L < days[D-1].L {
			cands = append(cands, d)
		}
	}
	if len(eligibleDays) == 0 {
		return fmt.Errorf("no trading days between %s and %s", f, t)
	}
	// Random draws: as many entries as candidate days, on random days and
	// minutes (signal bar 9:15 to 14:59).
	rng := rand.New(rand.NewSource(*seed))
	type rnd struct {
		draw int
		at   time.Time
	}
	randAt := map[market.Date][]rnd{}
	for k := 0; k < *draws; k++ {
		for range cands {
			d := eligibleDays[rng.Intn(len(eligibleDays))]
			m := rng.Intn(345)
			randAt[d] = append(randAt[d], rnd{k, d.At(9, 15).Add(time.Duration(m) * time.Minute)})
		}
	}

	vars := pdlVariants()
	indexKey := market.Index(market.NSE, *underlying).Key()
	var open []*pdlLeg
	var trades []pdlTrade
	skips := map[string]int{}
	ruleEntries := 0
	strikeName := func(rule string, spot float64) market.Paise {
		mult := map[string]float64{"atm": 1, "itm1": 1.01, "itm2": 1.02}[rule]
		return market.Rupees(math.Round(spot*mult / *step) * *step)
	}
	closeLeg := func(l *pdlLeg, xp market.Paise, xt time.Time, why string) error {
		net, err := roundTrip(l.inst, l.entry, xp, l.in, xt)
		if err != nil {
			return err
		}
		sl, err := roundTrip(l.inst, slipBy(l.entry, true, *slipBps), slipBy(xp, false, *slipBps), l.in, xt)
		if err != nil {
			return err
		}
		sl2, err := roundTrip(l.inst, slipBy(l.entry, true, 2**slipBps), slipBy(xp, false, 2**slipBps), l.in, xt)
		if err != nil {
			return err
		}
		trades = append(trades, pdlTrade{l.variant, l.draw, l.day, l.key, l.leg, why, l.in, xt, l.entry, xp, l.inst.LotSize, net, sl, sl2})
		return nil
	}

	var prevIdx []market.Bar
	var prevDay market.Date
	days1m, err := store.Days(*underlying, f.AddDays(-7), t)
	if err != nil {
		return err
	}
	start := time.Now()
	for di, d := range days1m {
		all, err := store.DayBars(ctx, *underlying, d)
		if err != nil {
			return err
		}
		byKey := map[string][]market.Bar{}
		for _, b := range all {
			byKey[b.Key] = append(byKey[b.Key], b)
		}
		idx := byKey[indexKey]
		yIdx, yDay := prevIdx, prevDay
		prevIdx, prevDay = idx, d
		if d.Before(f) || len(idx) < 60 {
			continue
		}
		indexPuts(byKey)
		day0 := d.At(0, 0)
		cutoff, flatAt := day0.Add(15*time.Hour), day0.Add(15*time.Hour+15*time.Minute)

		// advance moves a leg through today's bars from `from`; it returns the
		// exit time (zero if still open).
		var advance func(l *pdlLeg, from time.Time) (time.Time, error)
		advance = func(l *pdlLeg, from time.Time) (time.Time, error) {
			bars := byKey[l.key]
			if l.pending {
				if len(bars) == 0 {
					return time.Time{}, nil
				}
				return bars[0].Time, closeLeg(l, bars[0].Open, bars[0].Time, l.why+"_late")
			}
			for _, ib := range idx {
				if ib.Time.Before(from) {
					continue
				}
				var why string
				at := ib.End
				switch {
				case d == l.exitDay && !ib.Time.Before(flatAt):
					why, at = "time", flatAt
				case l.pivot > 0 && ib.Low.Rupees() <= l.pivot:
					why = "pivot"
				case l.stopAbove > 0 && ib.Close.Rupees() > l.stopAbove:
					why = "add_stop"
				}
				if why == "" {
					continue
				}
				xp, xt, ok := optAt(bars, at)
				if !ok {
					l.pending, l.why = true, why
					return time.Time{}, nil
				}
				return ib.Time, closeLeg(l, xp, xt, why)
			}
			if d == l.exitDay || l.exitDay.Before(d) {
				if len(bars) == 0 {
					l.pending, l.why = true, "time"
					return time.Time{}, nil
				}
				b := bars[len(bars)-1]
				return b.End, closeLeg(l, b.Close, b.End, "time")
			}
			return time.Time{}, nil
		}

		// Existing legs first (entries today start below).
		var still []*pdlLeg
		for _, l := range open {
			xt, err := advance(l, day0)
			if err != nil {
				return err
			}
			done := !xt.IsZero()
			if l.addDay == d && l.addLevel > 0 {
				// Add on a break of yesterday's (entry day's) low while the
				// main leg is open.
				for _, ib := range idx {
					if !ib.End.Before(cutoff) || (done && !ib.Time.Before(xt)) {
						break
					}
					if ib.Low.Rupees() >= l.addLevel {
						continue
					}
					bars := byKey[l.key]
					j := sort.Search(len(bars), func(k int) bool { return !bars[k].Time.Before(ib.End) })
					if j == len(bars) || bars[j].Time.Sub(ib.End) > 3*time.Minute {
						break
					}
					a := &pdlLeg{variant: l.variant, draw: l.draw, day: l.day, leg: "add", inst: l.inst, key: l.key,
						in: bars[j].Time, entry: bars[j].Open, exitDay: l.exitDay, pivot: l.pivot, stopAbove: l.addLevel}
					axt, err := advance(a, bars[j].Time)
					if err != nil {
						return err
					}
					if axt.IsZero() {
						still = append(still, a)
					}
					break
				}
			}
			if !done {
				still = append(still, l)
			}
		}
		open = still

		// New entries: rule signals and random draws.
		type entry struct {
			draw int
			sig  market.Bar
			y    int
		}
		var entries []entry
		D, haveD := dayIdx[d]
		okDay := haveD && D > 0 && pos[d] <= last-*hold && pos[d] > 0 && allDays[pos[d]-1] == days[D-1].D
		if okDay && pdlSetup(days, ema, D-1) {
			pdl := days[D-1].L
			if len(yIdx) > 0 && yDay == days[D-1].D {
				lo := yIdx[0].Low
				for _, b := range yIdx {
					lo = min(lo, b.Low)
				}
				pdl = lo.Rupees()
			}
			for _, ib := range idx {
				if !ib.End.Before(cutoff) {
					break
				}
				if ib.Low.Rupees() < pdl {
					entries = append(entries, entry{-1, ib, D - 1})
					break
				}
			}
		}
		if okDay {
			for _, r := range randAt[d] {
				j := sort.Search(len(idx), func(k int) bool { return !idx[k].Time.Before(r.at) })
				if j < len(idx) && idx[j].End.Before(cutoff) {
					entries = append(entries, entry{r.draw, idx[j], D - 1})
				}
			}
		} else if len(randAt[d]) > 0 {
			skips["random_day_unusable"] += len(randAt[d])
		}
		for _, e := range entries {
			spot := e.sig.Close.Rupees()
			piv := pdlPivot(days, e.y, spot)
			exitDay := allDays[pos[d]+*hold]
			if e.draw < 0 {
				ruleEntries++
			}
			for _, rule := range pdlStrikeRules {
				var strike market.Paise
				var exp market.Date
				if rule == "cap400" {
					atm := strikeName("atm", spot)
					found := false
					for s := atm; s <= atm+market.Rupees(30**step); s += market.Rupees(*step) {
						ex, ok := expiryFor(d, s)
						if !ok {
							continue
						}
						px, ok := optCloseAt(byKey[market.Option(market.NFO, *underlying, ex, s, market.Put, 0).Key()], e.sig.Time)
						if !ok {
							continue
						}
						if px.Rupees() > *capPrem {
							break
						}
						strike, exp, found = s, ex, true
					}
					if !found {
						if e.draw < 0 {
							skips["cap400_none"]++
						}
						continue
					}
				} else {
					strike = strikeName(rule, spot)
					var ok bool
					exp, ok = expiryFor(d, strike)
					if !ok {
						if e.draw < 0 {
							skips[rule+"_no_expiry"]++
						}
						continue
					}
				}
				key := market.Option(market.NFO, *underlying, exp, strike, market.Put, 0).Key()
				bars := byKey[key]
				j := sort.Search(len(bars), func(k int) bool { return !bars[k].Time.Before(e.sig.End) })
				if j == len(bars) || bars[j].Time.Sub(e.sig.End) > 3*time.Minute {
					if e.draw < 0 {
						skips[rule+"_no_fill"]++
					}
					continue
				}
				inst, ok := lots[key]
				if !ok {
					continue
				}
				for _, v := range vars {
					if v.strike != rule || (v.add && e.draw >= 0) {
						continue
					}
					l := &pdlLeg{variant: v.String(), draw: e.draw, day: d, leg: "main", inst: inst, key: key,
						in: bars[j].Time, entry: bars[j].Open, exitDay: exitDay}
					switch v.exit {
					case "day":
						l.exitDay = d
					case "3d_pivot":
						l.pivot = piv
					}
					if v.add {
						l.addDay = allDays[pos[d]+1]
						lo := idx[0].Low
						for _, b := range idx {
							lo = min(lo, b.Low)
						}
						l.addLevel = lo.Rupees() // today's full low, used tomorrow
					}
					xt, err := advance(l, bars[j].Time)
					if err != nil {
						return err
					}
					if xt.IsZero() {
						open = append(open, l)
					}
				}
			}
		}
		if (di+1)%50 == 0 {
			fmt.Fprintf(os.Stderr, "  %d/%d days, %d open legs, %s\n", di+1, len(days1m), len(open), time.Since(start).Round(time.Second))
		}
	}
	if len(open) > 0 {
		skips["still_open_at_end"] = len(open)
	}

	printPDLOptions(trades, vars, mid, *underlying, f, t, len(cands), ruleEntries, *draws, *slipBps, skips)

	if *tradesOut != "" {
		if err := os.MkdirAll(filepath.Dir(*tradesOut), 0o755); err != nil {
			return err
		}
		th, err := os.Create(*tradesOut)
		if err != nil {
			return err
		}
		defer th.Close()
		tw := csv.NewWriter(th)
		tw.Write([]string{"variant", "leg", "why", "day", "key", "entry_time", "entry", "exit_time", "exit_px", "lot", "net_rs", "net_slip_rs", "net_slip2x_rs"})
		ff := func(x float64) string { return strconv.FormatFloat(x, 'f', 2, 64) }
		for _, tr := range trades {
			if tr.draw >= 0 {
				continue
			}
			tw.Write([]string{tr.variant, tr.leg, tr.why, tr.day.String(), tr.key, tr.in.Format("2006-01-02 15:04"), tr.entry.String(),
				tr.out.Format("2006-01-02 15:04"), tr.ext.String(), strconv.FormatInt(tr.lot, 10), ff(tr.net), ff(tr.slip), ff(tr.sl2x)})
		}
		tw.Flush()
		if err := tw.Error(); err != nil {
			return err
		}
	}

	var net market.Paise
	n := 0
	for _, tr := range trades {
		if tr.draw < 0 {
			net += market.Rupees(tr.slip)
			n++
		}
	}
	err = analytics.AppendTrials(analytics.LedgerPath(*dir), []analytics.TrialRecord{{
		Time: time.Now(), Command: "research pdlswing", Strategy: "pdl_swing", Underlying: *underlying,
		Params: strategy.Params{"slip_bps": *slipBps, "step": *step, "cap": *capPrem, "hold": float64(*hold), "min_days": float64(*minDays)},
		From:   f, To: t, Count: len(vars), Net: net, Trades: n,
	}})
	if err != nil {
		return fmt.Errorf("trial ledger: %w", err)
	}
	fmt.Printf("trades: %s (recorded in the trial ledger)\n", *tradesOut)
	return nil
}

func printPDLIndex(tr []pdlIdxTrade, u string, from, to market.Date) {
	fmt.Printf("Daily %s index test, %s to %s: points per trade as a short from min(open, previous low); no costs.\n", u, from, to)
	years := map[int]bool{}
	for _, x := range tr {
		years[x.d.Year] = true
	}
	var ys []int
	for y := range years {
		ys = append(ys, y)
	}
	sort.Ints(ys)
	fmt.Printf("%-6s", "year")
	for _, e := range pdlExitRules {
		fmt.Printf(" | %-8s %4s %5s %8s", e, "n", "win%", "points")
	}
	fmt.Println()
	row := func(label string, keep func(pdlIdxTrade) bool) {
		fmt.Printf("%-6s", label)
		for _, e := range pdlExitRules {
			n, w, p := 0, 0, 0.0
			for _, x := range tr {
				if x.exit == e && keep(x) {
					n++
					p += x.pts
					if x.pts > 0 {
						w++
					}
				}
			}
			wp := 0.0
			if n > 0 {
				wp = float64(w) / float64(n) * 100
			}
			fmt.Printf(" | %-8s %4d %5.1f %8.0f", "", n, wp, p)
		}
		fmt.Println()
	}
	for _, y := range ys {
		row(strconv.Itoa(y), func(x pdlIdxTrade) bool { return x.d.Year == y })
	}
	row("<2019", func(x pdlIdxTrade) bool { return x.d.Year < 2019 })
	row(">=2019", func(x pdlIdxTrade) bool { return x.d.Year >= 2019 })
	row("all", func(pdlIdxTrade) bool { return true })
	// Concentration: the share of 3d_pivot points from the best 10 trades.
	var ps []float64
	tot := 0.0
	for _, x := range tr {
		if x.exit == "3d_pivot" {
			ps = append(ps, x.pts)
			tot += x.pts
		}
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(ps)))
	top := 0.0
	for i := 0; i < 10 && i < len(ps); i++ {
		top += ps[i]
	}
	fmt.Printf("3d_pivot: best 10 trades %.0f points of %.0f total.\n\n", top, tot)
}

func printPDLOptions(trades []pdlTrade, vars []pdlVariant, mid market.Date, u string, f, t market.Date, cands, ruleEntries, draws int, bps float64, skips map[string]int) {
	fmt.Printf("%s put options %s to %s: %d candidate days from daily bars, %d rule entries; %d random draws of %d entries each.\n",
		u, f, t, cands, ruleEntries, draws, cands)
	fmt.Printf("One lot a leg, charges + %.0f bps a side (2x: %.0f). Halves split at %s. Skips: %v\n\n", bps, 2*bps, mid, skips)
	type sum struct {
		n               int
		win, sl, sl2, g float64
		pf, dd          float64
	}
	summarize := func(ts []pdlTrade) sum {
		var s sum
		var w, l, peak, cum float64
		for _, tr := range ts {
			s.n++
			s.g += tr.net
			s.sl += tr.slip
			s.sl2 += tr.sl2x
			if tr.slip > 0 {
				s.win++
				w += tr.slip
			} else {
				l -= tr.slip
			}
			cum += tr.slip
			peak = max(peak, cum)
			s.dd = max(s.dd, peak-cum)
		}
		s.pf = math.Inf(1)
		if l > 0 {
			s.pf = w / l
		}
		if s.n > 0 {
			s.win = s.win / float64(s.n) * 100
		}
		return s
	}
	sort.SliceStable(trades, func(i, j int) bool { return trades[i].out.Before(trades[j].out) })
	fmt.Printf("%-20s | %5s %5s %9s %9s %9s %5s %8s | %4s %9s | %8s %4s | %5s %8s %5s | %5s %8s %5s\n",
		"variant", "legs", "win%", "gross", "net", "net 2x", "PF", "max DD", "conc", "capital", "top5 day", "rnd%", "h1 n", "h1 net", "PF", "h2 n", "h2 net", "PF")
	for _, v := range vars {
		name := v.String()
		var rule, a, b []pdlTrade
		rnd := map[int]float64{}
		for _, tr := range trades {
			if tr.variant != name {
				continue
			}
			if tr.draw >= 0 {
				rnd[tr.draw] += tr.slip
				continue
			}
			rule = append(rule, tr)
			if tr.day.Before(mid) {
				a = append(a, tr)
			} else {
				b = append(b, tr)
			}
		}
		s, sa, sb := summarize(rule), summarize(a), summarize(b)
		// Peak concurrent legs and premium outlay.
		type ev struct {
			t   time.Time
			n   int
			cap float64
		}
		var evs []ev
		for _, tr := range rule {
			c := tr.entry.Rupees() * float64(tr.lot)
			evs = append(evs, ev{tr.in, 1, c}, ev{tr.out, -1, -c})
		}
		sort.Slice(evs, func(i, j int) bool {
			if evs[i].t.Equal(evs[j].t) {
				return evs[i].n < evs[j].n
			}
			return evs[i].t.Before(evs[j].t)
		})
		conc, peakConc, capNow, peakCap := 0, 0, 0.0, 0.0
		for _, e := range evs {
			conc += e.n
			capNow += e.cap
			peakConc = max(peakConc, conc)
			peakCap = max(peakCap, capNow)
		}
		byDay := map[market.Date]float64{}
		for _, tr := range rule {
			byDay[tr.day] += tr.slip
		}
		var dps []float64
		for _, x := range byDay {
			dps = append(dps, x)
		}
		sort.Sort(sort.Reverse(sort.Float64Slice(dps)))
		top5 := 0.0
		for i := 0; i < 5 && i < len(dps); i++ {
			top5 += dps[i]
		}
		pct := "-"
		if !v.add && len(rnd) > 0 {
			below := 0
			for k := 0; k < draws; k++ {
				if rnd[k] < s.sl {
					below++
				}
			}
			pct = strconv.Itoa(below * 100 / draws)
		}
		fmt.Printf("%-20s | %5d %5.1f %9.0f %9.0f %9.0f %5.2f %8.0f | %4d %9.0f | %8.0f %4s | %5d %8.0f %5.2f | %5d %8.0f %5.2f\n",
			name, s.n, s.win, s.g, s.sl, s.sl2, s.pf, s.dd, peakConc, peakCap, top5, pct, sa.n, sa.sl, sa.pf, sb.n, sb.sl, sb.pf)
		if !v.add && len(rnd) > 0 {
			var xs []float64
			for k := 0; k < draws; k++ {
				xs = append(xs, rnd[k])
			}
			fmt.Printf("%-20s   random: median %.0f, 5th %.0f, 95th %.0f\n", "", percentile(xs, 50), percentile(xs, 5), percentile(xs, 95))
		}
	}
	fmt.Println()
	fmt.Println("Exit reasons (rule legs):")
	for _, v := range vars {
		why := map[string]int{}
		for _, tr := range trades {
			if tr.variant == v.String() && tr.draw < 0 {
				why[tr.leg+":"+tr.why]++
			}
		}
		fmt.Printf("  %-20s %v\n", v.String(), why)
	}
	fmt.Println()
}
