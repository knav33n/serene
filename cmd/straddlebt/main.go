// Command straddlebt backtests intraday NIFTY short straddles and strangles on
// Fyers 1m bars (scratch, uncommitted): sell 1 lot CE + PE of the nearest
// expiry at an entry time, 25% stop (per leg, per leg with the other moved to
// cost, or on the combined premium), optional profit target on margin, flat at
// the exit time. Prints a grid summary for the research window.
//
// --scan HH:MM-HH:MM switches to a proxy of Stratzy's "Lattice Short
// Straddle": scan each minute of the window, sell the ATM straddle at the first
// minute the index is within --bands points of a level (prev-day H/L/C, floor
// pivots, round 100s) and the ATM straddle IV is at or above a trailing-60-day
// percentile; per-leg 25% stop or a volatility-scaled stop; flat at --exit.
package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"

	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/market"
)

type strikeRule struct {
	name string
	off  float64 // OTM distance in points, when prem == 0
	prem float64 // target premium per side
	// itmoi: highest-OI strike at least itm points in the money (OI of the bar before entry)
	itmoi bool
	itm   float64
}

type cell struct {
	entry  string
	strike strikeRule
	sl     string // leg, legcost, combined
	tp     float64
	// lattice scan mode
	lat  bool
	lvl  string  // none, pdl, piv, round, all
	band float64 // points
	ivp  float64 // IV percentile gate, 0 = none
	volk float64 // vol-scaled stop multiplier, 0 = per-leg --sl %
	// wait/VIX grid (--waits/--vix-maxs/--sls)
	ext    bool
	wait   float64 // sell each leg only after it falls this % from its entry-minute open
	vixMax float64 // trade only if India VIX at the entry minute < this, 0 = off
	slPct  float64
	side   string // "", both, ce, pe ("" = both, old names)
}

func (c cell) name() string {
	if c.lat {
		st := "leg25"
		if c.volk > 0 {
			st = fmt.Sprintf("volk%g", c.volk)
		}
		return fmt.Sprintf("lat_%s_b%g_iv%g_%s", c.lvl, c.band, c.ivp, st)
	}
	n := fmt.Sprintf("%s_%s_%s_tp%g", strings.ReplaceAll(c.entry, ":", ""), c.strike.name, c.sl, c.tp)
	if c.ext {
		n += fmt.Sprintf("_w%g_v%g_sl%g", c.wait, c.vixMax, c.slPct)
	}
	if c.side != "" {
		n += "_" + c.side
	}
	return n
}

type result struct {
	day          market.Date
	dte0         bool
	gross        float64 // ₹, before costs and slippage
	net, netSlip float64
	margin       float64
	ceSL, peSL   bool
	tpHit        bool
	legs         []legOut
}

type legOut struct {
	inst          market.Instrument
	entry, exit   market.Paise
	entryT, exitT time.Time
	reason        string
}

func main() {
	dir := flag.String("data", os.Getenv("HOME")+"/INSIGHTS/fyers", "")
	from := flag.String("from", "2022-01-01", "")
	to := flag.String("to", "2023-12-31", "")
	costsF := flag.String("costs", "configs/costs.yaml", "")
	slipBps := flag.Float64("slip-bps", 30, "")
	slPct := flag.Float64("sl", 25, "stop %, 0 = none")
	marginPct := flag.Float64("margin-pct", 9, "margin per lot as % of spot × lot")
	exitAt := flag.String("exit", "15:15", "")
	entries := flag.String("entries", "09:20,09:30,09:45,10:00,10:30,11:00,12:00,13:00,14:00", "")
	strikesF := flag.String("strikes", "atm,otm100,otm200,otm300,prem50,prem30,prem20", "")
	slModes := flag.String("sl-modes", "leg,legcost,combined", "")
	tps := flag.String("tps", "0,1,2,3", "profit targets, % of margin")
	out := flag.String("out", "reports/straddle.csv", "")
	tradesF := flag.String("trades", "", "write per-leg trades of --trades-cell here")
	tradesCell := flag.String("trades-cell", "", "")
	dte0Only := flag.Bool("dte0", false, "expiry days only")
	mddMaxF := flag.Float64("mdd-max", 10, "max drawdown limit, % of account")
	scan := flag.String("scan", "", "lattice mode: scan window HH:MM-HH:MM")
	lvlsF := flag.String("lvls", "none,pdl,piv,round,all", "lattice level sets")
	bandsF := flag.String("bands", "10,20,30", "lattice level bands, points")
	ivsF := flag.String("ivs", "0,50,70", "lattice IV percentile gates")
	stopsF := flag.String("stops", "0,0.5,1,1.5", "lattice stops: 0 = per-leg --sl %, k = vol-scaled")
	cellsF := flag.String("cells", "", "lattice mode: only these cell names")
	waitsF := flag.String("waits", "", "grid: leg waits, % fall from the entry-minute open before selling")
	vixMaxsF := flag.String("vix-maxs", "", "grid: India VIX caps at entry, 0 = off")
	slsF := flag.String("sls", "", "grid: per-cell stop %, overrides --sl")
	vixDir := flag.String("vix-dir", "data/indices/minute/nse", "")
	sidesF := flag.String("sides", "", "grid: both,ce,pe (empty = both, old cell names)")
	mtmSL := flag.Float64("mtm-sl", 1, "sl-mode mtm: close all when MTM ≤ −this % of margin")
	noDTE0 := flag.Bool("no-dte0", false, "skip expiry days")
	flag.Parse()
	ctx := context.Background()
	f, err := market.ParseDate(*from)
	must(err)
	t, err := market.ParseDate(*to)
	must(err)
	costs, err := cost.Load(*costsF)
	must(err)
	store, err := data.Open(*dir)
	must(err)
	insts, err := store.Contracts(ctx, "NIFTY")
	must(err)
	lots := map[string]market.Instrument{}
	for _, in := range insts {
		lots[in.Key()] = in
	}
	days, err := store.Days("NIFTY", f, t)
	must(err)
	if *dte0Only || *noDTE0 {
		exps := map[market.Date]bool{}
		for _, in := range insts {
			if in.Kind == market.KindOption {
				exps[in.Expiry] = true
			}
		}
		var keep []market.Date
		for _, d := range days {
			if exps[d] == *dte0Only {
				keep = append(keep, d)
			}
		}
		days = keep
	}
	exH, exM := hm(*exitAt)

	var cells []cell
	var scanFrom, scanTo [2]int
	if *scan != "" {
		a, b, _ := strings.Cut(*scan, "-")
		scanFrom[0], scanFrom[1] = hm(a)
		scanTo[0], scanTo[1] = hm(b)
		want := map[string]bool{}
		for _, n := range split(*cellsF) {
			if n != "" {
				want[n] = true
			}
		}
		for _, lv := range split(*lvlsF) {
			bands := split(*bandsF)
			if lv == "none" {
				bands = []string{"0"}
			}
			for _, bs := range bands {
				for _, is := range split(*ivsF) {
					for _, ks := range split(*stopsF) {
						var b, iv, k float64
						fmt.Sscan(bs, &b)
						fmt.Sscan(is, &iv)
						fmt.Sscan(ks, &k)
						c := cell{entry: *scan, strike: strikeRule{name: "atm"}, sl: "leg", lat: true, lvl: lv, band: b, ivp: iv, volk: k}
						if len(want) == 0 || want[c.name()] {
							cells = append(cells, c)
						}
					}
				}
			}
		}
	}
	for _, e := range split(*entries) {
		if *scan != "" {
			break
		}
		for _, s := range split(*strikesF) {
			for _, m := range split(*slModes) {
				for _, tp := range split(*tps) {
					var v float64
					fmt.Sscan(tp, &v)
					base := cell{entry: e, strike: parseStrike(s), sl: m, tp: v, slPct: *slPct}
					add := func(c cell) {
						if *sidesF == "" {
							cells = append(cells, c)
							return
						}
						for _, sd := range split(*sidesF) {
							c.side = sd
							cells = append(cells, c)
						}
					}
					if *waitsF == "" && *vixMaxsF == "" && *slsF == "" {
						add(base)
						continue
					}
					or := func(f, d string) string {
						if f == "" {
							return d
						}
						return f
					}
					for _, ws := range split(or(*waitsF, "0")) {
						for _, vs := range split(or(*vixMaxsF, "0")) {
							for _, ss := range split(or(*slsF, fmt.Sprint(*slPct))) {
								c := base
								c.ext = true
								fmt.Sscan(ws, &c.wait)
								fmt.Sscan(vs, &c.vixMax)
								fmt.Sscan(ss, &c.slPct)
								add(c)
							}
						}
					}
				}
			}
		}
	}

	// Costs: configs/costs.yaml starts 2024-10-01; older fills are charged at
	// those rates (higher STT/exchange than at the time, so conservative).
	costFloor := time.Date(2024, 10, 1, 10, 0, 0, 0, market.IST)
	charge := func(inst market.Instrument, side market.Side, qty int64, p market.Paise, at time.Time) market.Paise {
		if at.Before(costFloor) {
			at = costFloor
		}
		c, err := costs.Charges(market.Fill{Instrument: inst, Side: side, Qty: qty, Price: p, Time: at, Kind: market.Traded})
		must(err)
		return c
	}
	slip := func(p market.Paise, buy bool) market.Paise {
		s := max(market.Paise(math.Round(float64(p)**slipBps/1e4)), 10)
		if buy {
			return p + s
		}
		return max(p-s, 5)
	}

	var vix map[time.Time]float64
	for _, c := range cells {
		if c.vixMax > 0 {
			vix = loadVIX(*vixDir)
			break
		}
	}
	results := make([][]result, len(cells))
	indexKey := market.Index(market.NSE, "NIFTY").Key()
	skipped := map[string]int{}
	var tw *csv.Writer
	if *tradesF != "" {
		fh, err := os.Create(*tradesF)
		must(err)
		defer fh.Close()
		tw = csv.NewWriter(fh)
		defer tw.Flush()
		tw.Write([]string{"day", "instrument", "entry_time", "entry", "exit_time", "exit", "reason", "qty"})
	}
	book := func(ci int, c cell, d market.Date, et time.Time, r *result) {
		// costs and slippage
		for _, l := range r.legs {
			q := l.inst.LotSize
			g := (l.entry - l.exit) * market.Paise(q)
			r.gross += g.Rupees()
			cs := charge(l.inst, market.Sell, q, l.entry, l.entryT) + charge(l.inst, market.Buy, q, l.exit, l.exitT)
			r.net += (g - cs).Rupees()
			se, sx := slip(l.entry, false), slip(l.exit, true)
			cs2 := charge(l.inst, market.Sell, q, se, l.entryT) + charge(l.inst, market.Buy, q, sx, l.exitT)
			r.netSlip += ((se-sx)*market.Paise(q) - cs2).Rupees()
		}
		if tw != nil && c.name() == *tradesCell {
			for _, l := range r.legs {
				tw.Write([]string{d.String(), l.inst.Key(), l.entryT.Format("15:04"), fmt.Sprint(l.entry.Rupees()), l.exitT.Format("15:04"),
					fmt.Sprint(l.exit.Rupees()), l.reason, fmt.Sprint(l.inst.LotSize)})
			}
		}
		r.legs = nil
		results[ci] = append(results[ci], *r)
	}
	var prevH, prevL, prevC market.Paise // previous trading day's index H/L/C
	var ivHist []float64                 // ATM straddle IV at the scan start, one per day
	for di, d := range days {
		all, err := store.DayBars(ctx, "NIFTY", d)
		must(err)
		byKey := map[string]map[time.Time]market.Bar{}
		for _, b := range all {
			if market.DateOf(b.Time) != d {
				continue
			}
			m := byKey[b.Key]
			if m == nil {
				m = map[time.Time]market.Bar{}
				byKey[b.Key] = m
			}
			m[b.Time] = b
		}
		idx := byKey[indexKey]
		// nearest expiry with option bars today
		var exp market.Date
		for k := range byKey {
			in, ok := lots[k]
			if !ok || in.Kind != market.KindOption || in.Expiry.Before(d) {
				continue
			}
			if exp.IsZero() || in.Expiry.Before(exp) {
				exp = in.Expiry
			}
		}
		exitT := d.At(exH, exM)
		if *scan != "" {
			if prevC > 0 && !exp.IsZero() {
				ivRef := latticeDay(d, exp, idx, byKey, lots, scanFrom, scanTo, exitT, prevH, prevL, prevC, ivHist, cells, *slPct, *marginPct, skipped, book)
				if ivRef > 0 {
					ivHist = append(ivHist, ivRef)
				}
			} else {
				skipped["no prev day / expiry"]++
			}
			var h, l, cl market.Paise
			var last time.Time
			for tm, b := range idx {
				if h == 0 || b.High > h {
					h = b.High
				}
				if l == 0 || b.Low < l {
					l = b.Low
				}
				if tm.After(last) {
					last, cl = tm, b.Close
				}
			}
			if cl > 0 {
				prevH, prevL, prevC = h, l, cl
			}
			if di%100 == 0 {
				fmt.Fprintf(os.Stderr, "%s %d/%d\n", d, di, len(days))
			}
			continue
		}
		for ci, c := range cells {
			eh, em := hm(c.entry)
			et := d.At(eh, em)
			ib, ok := idx[et]
			if !ok {
				skipped["no index bar"]++
				continue
			}
			if c.vixMax > 0 {
				v, ok := vix[et]
				if !ok {
					skipped["no vix"]++
					continue
				}
				if v >= c.vixMax {
					skipped["vix filter"]++
					continue
				}
			}
			spot := ib.Open
			var ce, pe market.Instrument
			if c.strike.itmoi {
				var okc, okp bool
				ce, pe, okc, okp = pickITMOI(lots, byKey, exp, et, spot, c.strike.itm)
				ok = (c.side == "ce" && okc) || (c.side == "pe" && okp) || okc && okp
				if c.side == "ce" {
					pe = market.Instrument{}
				}
				if c.side == "pe" {
					ce = market.Instrument{}
				}
			} else {
				ce, pe, ok = pickStrikes(lots, byKey, exp, et, spot, c.strike)
			}
			if !ok {
				skipped["no strike"]++
				continue
			}
			r := simulate(c, d, et, exitT, spot, ce, pe, byKey, c.slPct, *marginPct, 0, *mtmSL)
			if r == nil {
				skipped["no fill"]++
				continue
			}
			book(ci, c, d, et, r)
		}
		if di%100 == 0 {
			fmt.Fprintf(os.Stderr, "%s %d/%d\n", d, di, len(days))
		}
	}
	fmt.Fprintf(os.Stderr, "days %d, skipped %v\n", len(days), skipped)
	report(cells, results, *out, *mddMaxF)
}

// pickStrikes returns the CE and PE to sell, both with a bar at et.
func pickStrikes(lots map[string]market.Instrument, byKey map[string]map[time.Time]market.Bar, exp market.Date, et time.Time,
	spot market.Paise, rule strikeRule) (ce, pe market.Instrument, ok bool) {
	atm := market.Paise(math.Round(float64(spot)/5000) * 5000)
	if exp.IsZero() {
		return ce, pe, false
	}
	get := func(strike market.Paise, r market.Right) (market.Instrument, market.Bar, bool) {
		k := market.Option(market.NFO, "NIFTY", exp, strike, r, 0).Key()
		in, ok := lots[k]
		if !ok {
			return in, market.Bar{}, false
		}
		b, ok := byKey[k][et]
		return in, b, ok
	}
	if rule.prem == 0 {
		off := market.Rupees(rule.off)
		var ok1, ok2 bool
		ce, _, ok1 = get(atm+off, market.Call)
		pe, _, ok2 = get(atm-off, market.Put)
		return ce, pe, ok1 && ok2
	}
	target := market.Rupees(rule.prem)
	best := func(r market.Right) (market.Instrument, bool) {
		var bi market.Instrument
		var bd market.Paise = -1
		for s := 0; s <= 60; s++ { // up to 3000 pts OTM in 50s
			off := market.Paise(s) * 5000
			strike := atm + off
			if r == market.Put {
				strike = atm - off
			}
			in, b, ok := get(strike, r)
			if !ok {
				continue
			}
			dd := b.Open - target
			if dd < 0 {
				dd = -dd
			}
			if bd < 0 || dd < bd {
				bi, bd = in, dd
			}
		}
		return bi, bd >= 0
	}
	var ok1, ok2 bool
	ce, ok1 = best(market.Call)
	pe, ok2 = best(market.Put)
	return ce, pe, ok1 && ok2
}

// pickITMOI returns, per side, the strike at least itm points in the money
// with the highest OI on the bar before et, that also traded at et.
func pickITMOI(lots map[string]market.Instrument, byKey map[string]map[time.Time]market.Bar, exp market.Date, et time.Time,
	spot market.Paise, itm float64) (ce, pe market.Instrument, okc, okp bool) {
	if exp.IsZero() {
		return
	}
	atm := market.Paise(math.Round(float64(spot)/5000) * 5000)
	prev := et.Add(-time.Minute)
	best := func(r market.Right) (market.Instrument, bool) {
		var bi market.Instrument
		var bo int64 = -1
		for s := 0; s <= 80; s++ { // up to 4000 pts ITM in 50s
			off := market.Rupees(itm) + market.Paise(s)*5000
			strike := atm - off
			if r == market.Put {
				strike = atm + off
			}
			k := market.Option(market.NFO, "NIFTY", exp, strike, r, 0).Key()
			in, ok := lots[k]
			if !ok {
				continue
			}
			b, ok := byKey[k][et]
			if !ok || b.Volume <= 0 {
				continue
			}
			pb, ok := byKey[k][prev]
			if !ok || pb.OI <= 0 {
				continue
			}
			if pb.OI > bo {
				bi, bo = in, pb.OI
			}
		}
		return bi, bo > 0
	}
	ce, okc = best(market.Call)
	pe, okp = best(market.Put)
	return
}

func simulate(c cell, d market.Date, et, exitT time.Time, spot market.Paise, ce, pe market.Instrument,
	byKey map[string]map[time.Time]market.Bar, slPct, marginPct float64, stopAdd market.Paise, mtmSL float64) *result {
	type leg struct {
		inst  market.Instrument
		bars  map[time.Time]market.Bar
		entry market.Paise
		stop  market.Paise
		open  bool
		in    bool // sold
		trig  market.Paise
		entT  time.Time
		last  market.Bar
		out   legOut
	}
	var legs []*leg
	if ce.Key() != "" && ce.LotSize > 0 {
		legs = append(legs, &leg{inst: ce})
	}
	if pe.Key() != "" && pe.LotSize > 0 {
		legs = append(legs, &leg{inst: pe})
	}
	if len(legs) == 0 {
		return nil
	}
	setStop := func(l *leg) {
		l.stop = market.Paise(math.Round(float64(l.entry) * (1 + slPct/100)))
		if stopAdd > 0 {
			l.stop = l.entry + stopAdd
		}
		if slPct == 0 {
			l.stop = math.MaxInt64 / 4
		}
	}
	for _, l := range legs {
		l.bars = byKey[l.inst.Key()]
		b := l.bars[et]
		l.last = b
		if c.wait > 0 {
			l.trig = market.Paise(math.Round(float64(b.Open) * (1 - c.wait/100)))
			continue
		}
		l.entry, l.open, l.in, l.entT = b.Open, true, true, et
		setStop(l)
	}
	lot := legs[0].inst.LotSize
	r := &result{day: d, dte0: legs[0].inst.Expiry == d}
	r.margin = marginPct / 100 * spot.Rupees() * float64(lot)
	var credit market.Paise
	for _, l := range legs {
		credit += l.entry
	}
	combStop := market.Paise(math.Round(float64(credit) * (1 + slPct/100)))
	tpRs := c.tp / 100 * r.margin
	var realised market.Paise // per unit
	closeLeg := func(l *leg, px market.Paise, at time.Time, why string) {
		l.open = false
		l.out = legOut{l.inst, l.entry, px, l.entT, at, why}
		realised += l.entry - px
	}
	for tm := et; tm.Before(exitT); tm = tm.Add(time.Minute) {
		for _, l := range legs {
			if b, ok := l.bars[tm]; ok {
				l.last = b
			} else {
				// no trade this minute: flat bar at the last close
				l.last = market.Bar{Time: tm, End: tm.Add(time.Minute), Open: l.last.Close, High: l.last.Close, Low: l.last.Close, Close: l.last.Close}
			}
		}
		at := tm.Add(time.Minute)
		// legs waiting for their decay trigger; a fill bar's stop is checked from the next minute
		filled := map[*leg]bool{}
		for _, l := range legs {
			if c.wait > 0 && !l.in && l.last.Low <= l.trig && l.last.Low > 0 {
				l.entry = min(l.trig, l.last.Open)
				l.in, l.open, l.entT = true, true, tm
				setStop(l)
				filled[l] = true
			}
		}
		if slPct > 0 {
			switch c.sl {
			case "leg", "legcost":
				stopped := false
				for _, l := range legs {
					if l.open && !filled[l] && l.last.High >= l.stop {
						closeLeg(l, max(l.stop, l.last.Open), at, "sl")
						stopped = true
					}
				}
				if stopped && c.sl == "legcost" {
					for _, l := range legs {
						if l.open {
							l.stop = l.entry
						}
					}
				}
			case "mtm":
				// close all when MTM ≤ −mtmSL% of margin: one leg on its 1m high,
				// filled at the stop level; two legs on the sum of 1m closes
				lim := -mtmSL / 100 * r.margin
				open := 0
				for _, l := range legs {
					if l.open {
						open++
					}
				}
				if open == 1 {
					for _, l := range legs {
						if !l.open {
							continue
						}
						// rupee MTM at price p: (realised + entry − p) × lot
						lvl := l.entry + realised - market.Rupees(lim/float64(lot))
						if l.last.High >= lvl {
							closeLeg(l, max(lvl, l.last.Open), at, "sl")
						}
					}
				} else if open > 1 {
					mtm := realised
					for _, l := range legs {
						mtm += l.entry - l.last.Close
					}
					if mtm.Rupees()*float64(lot) <= lim {
						for _, l := range legs {
							closeLeg(l, l.last.Close, at, "sl")
						}
					}
				}
			case "combined", "combclose":
				if len(legs) < 2 {
					break
				}
				a, b := legs[0].last, legs[1].last
				if legs[0].open && legs[1].open {
					// combined: intrabar estimate of the sum's high (conservative);
					// combclose: sum of 1m closes, filled at that sum
					hiSum := max(a.High+b.Low, a.Low+b.High, a.Open+b.Open, a.Close+b.Close)
					lvl := max(combStop, a.Open+b.Open)
					if c.sl == "combclose" {
						hiSum, lvl = a.Close+b.Close, a.Close+b.Close
					}
					if hiSum >= combStop {
						// split the stop level by the legs' shares at the close
						pa := market.Paise(math.Round(float64(lvl) * float64(a.Close) / float64(a.Close+b.Close)))
						closeLeg(legs[0], pa, at, "sl")
						closeLeg(legs[1], lvl-pa, at, "sl")
					}
				}
			}
		}
		if c.tp > 0 {
			mtm := realised
			anyOpen := false
			for _, l := range legs {
				if l.open {
					mtm += l.entry - l.last.Close
					anyOpen = true
				}
			}
			if anyOpen && mtm.Rupees()*float64(lot) >= tpRs {
				for _, l := range legs {
					if l.open {
						closeLeg(l, l.last.Close, at, "tp")
					}
				}
				r.tpHit = true
			}
		}
		done := true
		for _, l := range legs {
			if l.open || !l.in {
				done = false
			}
		}
		if done {
			break
		}
	}
	for _, l := range legs {
		if l.open {
			closeLeg(l, l.last.Close, exitT, "eod")
		}
	}
	for _, l := range legs {
		if l.out.reason == "sl" {
			if l.inst.Right == market.Call {
				r.ceSL = true
			} else {
				r.peSL = true
			}
		}
	}
	for _, l := range legs {
		if l.in {
			r.legs = append(r.legs, l.out)
		}
	}
	if len(r.legs) == 0 {
		return nil
	}
	return r
}

type stats struct {
	n                                int
	win, net, netSlip, gross, avg, t float64
	pf, worst, rom                   float64
	ceSL, peSL, bothSL, tp           float64
	avgWin, avgLoss, expR            float64         // expectancy = avg; expR = avg / avgLoss
	mdd, mddPct                      float64         // ₹, and % of account value (start = avg 1-lot margin)
	mddDays, recDays                 int             // peak→trough, trough→back to peak (-1 = not recovered)
	roi                              map[int]float64 // net after slip by year, % of capital (avg 1-lot margin)
	roiMin, roiCV                    float64         // worst year; sd ÷ mean across years
}

func summarise(rs []result) stats {
	var s stats
	s.n = len(rs)
	if s.n == 0 {
		return s
	}
	var sum, sq, gw, gl, eq, marg float64
	var nl float64
	peak, peakI := 0.0, -1
	var ddPeakI, ddTroughI int
	ddPeakV := 0.0
	eqs := make([]float64, len(rs))
	for i, r := range rs {
		x := r.netSlip
		sum += x
		sq += x * x
		s.net += r.net
		s.gross += r.gross
		marg += r.margin
		if x > 0 {
			s.win++
			gw += x
		} else {
			gl -= x
			nl++
		}
		eq += x
		eqs[i] = eq
		if eq > peak {
			peak, peakI = eq, i
		}
		if peak-eq > s.mdd {
			s.mdd, ddPeakI, ddTroughI, ddPeakV = peak-eq, peakI, i, peak
		}
		s.worst = min(s.worst, x)
		s.rom += x / r.margin * 100
		if r.ceSL {
			s.ceSL++
		}
		if r.peSL {
			s.peSL++
		}
		if r.ceSL && r.peSL {
			s.bothSL++
		}
		if r.tpHit {
			s.tp++
		}
	}
	n := float64(s.n)
	s.netSlip = sum
	s.avg = sum / n
	if s.n > 1 {
		sd := math.Sqrt((sq - sum*sum/n) / (n - 1))
		if sd > 0 {
			s.t = s.avg / sd * math.Sqrt(n)
		}
	}
	if gl > 0 {
		s.pf = gw / gl
	}
	if s.win > 0 {
		s.avgWin = gw / s.win
	}
	if nl > 0 {
		s.avgLoss = gl / nl
		s.expR = s.avg / s.avgLoss
	}
	if s.mdd > 0 {
		s.mddDays = ddTroughI - ddPeakI // peak index -1 means the start
		s.recDays = -1
		for j := ddTroughI + 1; j < len(eqs); j++ {
			if eqs[j] >= ddPeakV {
				s.recDays = j - ddTroughI
				break
			}
		}
		// Investopedia: (peak − trough) / peak of account value, the
		// account starting at the average 1-lot margin
		capital, pk := marg/n, 0.0
		for _, e := range eqs {
			pk = max(pk, e)
			s.mddPct = max(s.mddPct, (pk-e)/(capital+pk)*100)
		}
	}
	capital := marg / n
	s.roi = map[int]float64{}
	for _, r := range rs {
		s.roi[r.day.Year] += r.netSlip / capital * 100
	}
	var ys, ysq float64
	first := true
	for _, v := range s.roi {
		ys += v
		ysq += v * v
		if first || v < s.roiMin {
			s.roiMin, first = v, false
		}
	}
	if k := float64(len(s.roi)); k > 1 && ys != 0 {
		mu := ys / k
		s.roiCV = math.Sqrt(max(0, (ysq-k*mu*mu)/(k-1))) / math.Abs(mu)
	}
	s.win = s.win / n * 100
	s.rom /= n
	s.ceSL, s.peSL, s.bothSL, s.tp = s.ceSL/n*100, s.peSL/n*100, s.bothSL/n*100, s.tp/n*100
	return s
}

func recRatio(s stats) string {
	if s.recDays < 0 {
		return "not_recovered"
	}
	if s.mddDays == 0 {
		return ""
	}
	return f2(float64(s.recDays) / float64(s.mddDays))
}

func report(cells []cell, results [][]result, out string, mddMax float64) {
	fh, err := os.Create(out)
	must(err)
	defer fh.Close()
	w := csv.NewWriter(fh)
	defer w.Flush()
	yset := map[int]bool{}
	for _, rs := range results {
		for _, r := range rs {
			yset[r.day.Year] = true
		}
	}
	var years []int
	for y := range yset {
		years = append(years, y)
	}
	sort.Ints(years)
	hdr := []string{"cell", "entry", "strike", "sl", "tp", "split", "n", "mdd", "mdd_pct", "mdd_days", "rec_days", "rec_over_mdd",
		"expectancy", "exp_r", "win_pct"}
	for _, y := range years {
		hdr = append(hdr, fmt.Sprintf("roi_%d", y))
	}
	hdr = append(hdr, "roi_min_yr", "roi_cv", "avg_win", "avg_loss", "net_slip", "net", "gross", "t", "pf", "worst", "rom_pct",
		"ce_sl_pct", "pe_sl_pct", "both_sl_pct", "tp_pct")
	w.Write(hdr)
	type row struct {
		c cell
		s stats
		i int
	}
	var rows []row
	for i, c := range cells {
		rs := results[i]
		var e0, ne []result
		for _, r := range rs {
			if r.dte0 {
				e0 = append(e0, r)
			} else {
				ne = append(ne, r)
			}
		}
		for _, sp := range []struct {
			n  string
			rs []result
		}{{"all", rs}, {"dte0", e0}, {"other", ne}} {
			s := summarise(sp.rs)
			if sp.n == "all" {
				rows = append(rows, row{c, s, i})
			}
			row := []string{c.name(), c.entry, c.strike.name, c.sl, fmt.Sprint(c.tp), sp.n, fmt.Sprint(s.n), f0(s.mdd), f1(s.mddPct),
				fmt.Sprint(s.mddDays), fmt.Sprint(s.recDays), recRatio(s), f0(s.avg), f2(s.expR), f1(s.win)}
			for _, y := range years {
				row = append(row, f1(s.roi[y]))
			}
			row = append(row, f1(s.roiMin), f2(s.roiCV), f0(s.avgWin), f0(s.avgLoss),
				f0(s.netSlip), f0(s.net), f0(s.gross), f2(s.t), f2(s.pf), f0(s.worst), f2(s.rom), f1(s.ceSL), f1(s.peSL), f1(s.bothSL), f1(s.tp))
			w.Write(row)
		}
	}
	sort.Slice(rows, func(a, b int) bool {
		pa, pb := rows[a].s.mddPct < mddMax, rows[b].s.mddPct < mddMax
		if pa != pb {
			return pa
		}
		return rows[a].s.avg > rows[b].s.avg
	})
	ok := 0
	for _, r := range rows {
		if r.s.mddPct < mddMax && r.s.avg > 0 {
			ok++
		}
	}
	fmt.Printf("%d cells with MDD < %g%% and positive expectancy\n", ok, mddMax)
	pos := 0
	for _, r := range rows {
		if r.s.netSlip > 0 {
			pos++
		}
	}
	fmt.Printf("%d cells, %d positive after slippage\n", len(rows), pos)
	fmt.Printf("%-30s %5s %8s %6s %5s %5s %13s %7s %6s %6s %7s %5s %10s %6s\n", "cell", "n", "mdd", "mdd%", "mddD", "recD", "rec/mdd", "expect", "expR", "win%", "roiMin", "roiCV", "net_slip", "t")
	for i, r := range rows {
		if i >= 15 {
			break
		}
		s := r.s
		fmt.Printf("%-30s %5d %8.0f %6.1f %5d %5d %13s %7.0f %6.2f %6.1f %7.1f %5.2f %10.0f %6.2f\n", r.c.name(), s.n, s.mdd, s.mddPct, s.mddDays, s.recDays, recRatio(s), s.avg, s.expR, s.win, s.roiMin, s.roiCV, s.netSlip, s.t)
	}
	fmt.Println("\nROI after slippage by year (% of 1-lot margin), top 10:")
	ys := years
	fmt.Printf("%-34s", "cell")
	for _, y := range ys {
		fmt.Printf(" %9d", y)
	}
	fmt.Println()
	for i, r := range rows {
		if i >= 10 {
			break
		}
		fmt.Printf("%-34s", r.c.name())
		for _, y := range ys {
			fmt.Printf(" %9.1f", r.s.roi[y])
		}
		fmt.Println()
	}
}

func parseStrike(s string) strikeRule {
	var v float64
	switch {
	case s == "atm":
		return strikeRule{name: s}
	case strings.HasPrefix(s, "otm"):
		fmt.Sscan(s[3:], &v)
		return strikeRule{name: s, off: v}
	case strings.HasPrefix(s, "itmoi"):
		fmt.Sscan(s[5:], &v)
		return strikeRule{name: s, itmoi: true, itm: v}
	case strings.HasPrefix(s, "prem"):
		fmt.Sscan(s[4:], &v)
		return strikeRule{name: s, prem: v}
	}
	must(fmt.Errorf("bad strike rule %q", s))
	return strikeRule{}
}

func hm(s string) (int, int) {
	tt, err := time.Parse("15:04", s)
	must(err)
	return tt.Hour(), tt.Minute()
}

func split(s string) []string { return strings.Split(s, ",") }
func f0(x float64) string     { return fmt.Sprintf("%.0f", x) }
func f1(x float64) string     { return fmt.Sprintf("%.1f", x) }
func f2(x float64) string     { return fmt.Sprintf("%.2f", x) }

// loadVIX reads India VIX 1m opens keyed by bar start (IST wall clock).
func loadVIX(dir string) map[time.Time]float64 {
	db, err := sql.Open("duckdb", "")
	must(err)
	defer db.Close()
	rows, err := db.Query(fmt.Sprintf(`select strftime(ts, '%%Y-%%m-%%d %%H:%%M'), open from '%s/key=India%%20VIX/bars.parquet'`, dir))
	must(err)
	defer rows.Close()
	m := map[time.Time]float64{}
	for rows.Next() {
		var ts string
		var o float64
		must(rows.Scan(&ts, &o))
		tt, err := time.ParseInLocation("2006-01-02 15:04", ts, market.IST)
		must(err)
		m[tt] = o
	}
	must(rows.Err())
	fmt.Fprintf(os.Stderr, "vix minutes %d\n", len(m))
	return m
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// latticeDay runs every lattice cell on one day and returns the ATM straddle
// IV at the scan start (0 if missing), for the trailing percentile history.
func latticeDay(d, exp market.Date, idx map[time.Time]market.Bar, byKey map[string]map[time.Time]market.Bar,
	lots map[string]market.Instrument, from, to [2]int, exitT time.Time, pH, pL, pC market.Paise, ivHist []float64,
	cells []cell, slPct, marginPct float64, skipped map[string]int, book func(int, cell, market.Date, time.Time, *result)) float64 {
	H, L, C := pH.Rupees(), pL.Rupees(), pC.Rupees()
	P := (H + L + C) / 3
	levels := map[string][]float64{
		"pdl": {H, L, C},
		"piv": {P, 2*P - L, 2*P - H, P + (H - L), P - (H - L)},
	}
	levels["all"] = append(append([]float64{}, levels["pdl"]...), levels["piv"]...)
	expT := exp.At(15, 30)
	type minute struct {
		tm       time.Time
		spot, iv float64
		ce, pe   market.Instrument
	}
	var ms []minute
	start, end := d.At(from[0], from[1]), d.At(to[0], to[1])
	for tm := start; !tm.After(end); tm = tm.Add(time.Minute) {
		ib, ok := idx[tm]
		if !ok {
			continue
		}
		ce, pe, ok := pickStrikes(lots, byKey, exp, tm, ib.Open, strikeRule{name: "atm"})
		if !ok {
			continue
		}
		cb, pb := byKey[ce.Key()][tm], byKey[pe.Key()][tm]
		spot := ib.Open.Rupees()
		T := expT.Sub(tm).Minutes() / 525600
		iv := 0.0
		if T > 0 {
			// ATM straddle ≈ 0.8·σ·S·√T
			iv = (cb.Open + pb.Open).Rupees() / (0.8 * spot * math.Sqrt(T))
		}
		ms = append(ms, minute{tm, spot, iv, ce, pe})
	}
	ivRef := 0.0
	if len(ms) > 0 && ms[0].tm.Equal(start) {
		ivRef = ms[0].iv
	}
	hist := ivHist
	if len(hist) > 60 {
		hist = hist[len(hist)-60:]
	}
	thr := func(p float64) (float64, bool) {
		if len(hist) < 20 {
			return 0, false
		}
		h := append([]float64{}, hist...)
		sort.Float64s(h)
		return h[int(math.Min(float64(len(h)-1), math.Floor(p/100*float64(len(h)))))], true
	}
	near := func(lvl string, band, spot float64) bool {
		switch lvl {
		case "none":
			return true
		case "round", "all":
			if math.Abs(spot-math.Round(spot/100)*100) <= band {
				return true
			}
			if lvl == "round" {
				return false
			}
		}
		for _, x := range levels[lvl] {
			if math.Abs(spot-x) <= band {
				return true
			}
		}
		return false
	}
	for ci, c := range cells {
		th := 0.0
		if c.ivp > 0 {
			var ok bool
			if th, ok = thr(c.ivp); !ok {
				skipped["iv history < 20"]++
				continue
			}
		}
		var hit *minute
		for i := range ms {
			m := &ms[i]
			if m.iv <= 0 || (c.ivp > 0 && m.iv < th) || !near(c.lvl, c.band, m.spot) {
				continue
			}
			hit = m
			break
		}
		if hit == nil {
			skipped["no scan hit"]++
			continue
		}
		var stopAdd market.Paise
		if c.volk > 0 {
			// expected index move to the exit from the last 30 1m returns,
			// × 0.5 ATM delta × k, as a premium cushion on each leg
			var rs []float64
			for tm := hit.tm.Add(-30 * time.Minute); tm.Before(hit.tm); tm = tm.Add(time.Minute) {
				a, ok1 := idx[tm.Add(-time.Minute)]
				b, ok2 := idx[tm]
				if ok1 && ok2 && a.Close > 0 {
					rs = append(rs, math.Log(float64(b.Close)/float64(a.Close)))
				}
			}
			if len(rs) < 10 {
				skipped["vol history"]++
				continue
			}
			var mu, sq float64
			for _, x := range rs {
				mu += x
			}
			mu /= float64(len(rs))
			for _, x := range rs {
				sq += (x - mu) * (x - mu)
			}
			sd := math.Sqrt(sq / float64(len(rs)-1))
			move := sd * math.Sqrt(exitT.Sub(hit.tm).Minutes()) * hit.spot
			stopAdd = market.Rupees(c.volk * 0.5 * move)
			if stopAdd <= 0 {
				stopAdd = 1
			}
		}
		r := simulate(c, d, hit.tm, exitT, market.Rupees(hit.spot), hit.ce, hit.pe, byKey, slPct, marginPct, stopAdd, 0)
		if r == nil {
			skipped["no bars"]++
			continue
		}
		book(ci, c, d, hit.tm, r)
	}
	return ivRef
}
