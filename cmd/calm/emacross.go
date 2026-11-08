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

	"calm/internal/analytics"
	"calm/internal/cost"
	"calm/internal/data"
	"calm/internal/indicator"
	"calm/internal/market"
	"calm/internal/strategy"
)

// emaCell is one combination in the EMA crossover grid.
type emaCell struct {
	dte        int // trading days to expiry: 0 or 1
	index      bool
	tf         market.Timeframe
	fast, slow int
}

func (c emaCell) chart() string {
	if c.index {
		return "index"
	}
	return "option"
}

// emaTrade is one round trip in one lot.
type emaTrade struct {
	day        market.Date
	key        string
	right      market.Right
	in, out    time.Time
	entry, ext market.Paise
	net, slip  float64 // rupees after charges; slip also after slippage
	pct        float64 // return on premium before costs
}

// emaPairs are the crossovers tried, fast/slow.
var emaPairs = [][2]int{{3, 8}, {5, 13}, {5, 20}, {8, 21}, {9, 21}, {12, 26}, {13, 34}, {20, 50}}

// runEMACross replays EMA crossovers on ATM NIFTY options on expiry day and
// the day before. Crossovers read on the index chart buy the ATM call on an
// up-cross and the ATM put on a down-cross, reversing on the next cross.
// Crossovers read on an option's own chart buy that option on an up-cross and
// sell it on the down-cross. Signals act at the next minute's open; every
// position closes at 15:15.
func runEMACross(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("emacross", flag.ContinueOnError)
	underlying := fs.String("underlying", "NIFTY", "underlying name")
	from := fs.String("from", "2024-12-27", "first day")
	to := fs.String("to", "2026-04-30", "last day")
	split := fs.String("split", "2025-09-01", "first day of the second half")
	dir := fs.String("data", "data/upstox", "data directory")
	holdout := fs.String("holdout", "configs/holdout.yaml", "data held back from research")
	costsFile := fs.String("costs", "configs/costs.yaml", "charges schedule")
	slipBps := fs.Float64("slip-bps", 30, "slippage per side, at least 2 ticks")
	step := fs.Float64("step", 50, "strike step for ATM")
	lastEntry := fs.String("last-entry", "15:00", "no entries from this time")
	out := fs.String("out", "reports/emacross.csv", "every combination's results")
	tradesOut := fs.String("trades", "", "also write every trade to this CSV")
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
	le, err := time.Parse("15:04", *lastEntry)
	if err != nil {
		return fmt.Errorf("--last-entry: %w", err)
	}
	entryBy := time.Duration(le.Hour())*time.Hour + time.Duration(le.Minute())*time.Minute
	flat := 15*time.Hour + 15*time.Minute
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
	// All stored days, so the day after the window's last day is known.
	allDays, err := store.Days(*underlying, f.AddDays(-10), t.AddDays(15))
	if err != nil {
		return err
	}
	pos := map[market.Date]int{}
	for i, d := range allDays {
		pos[d] = i
	}

	tfs := []market.Timeframe{1, 3, 5, 15}
	var cells []emaCell
	for _, dte := range []int{0, 1} {
		for _, index := range []bool{true, false} {
			for _, tf := range tfs {
				for _, p := range emaPairs {
					cells = append(cells, emaCell{dte, index, tf, p[0], p[1]})
				}
			}
		}
	}
	results := make([][]emaTrade, len(cells))
	cellIndex := map[emaCell]int{}
	for i, c := range cells {
		cellIndex[c] = i
	}

	slip := func(p market.Paise, buy bool) market.Paise {
		s := max(market.Paise(math.Round(float64(p)**slipBps/1e4)), 10) // 2 ticks of 5 paise
		if buy {
			return p + s
		}
		return max(p-s, 5)
	}
	trade := func(d market.Date, key string, in, out market.Bar) (emaTrade, error) {
		inst := lots[key]
		tr := emaTrade{day: d, key: key, right: inst.Right, in: in.Time, out: out.Time, entry: in.Open, ext: out.Open}
		if out.Time.Equal(in.Time) {
			tr.ext = in.Close
		}
		charge := func(p market.Paise, side market.Side, tm time.Time) (market.Paise, error) {
			return costs.Charges(market.Fill{Instrument: inst, Side: side, Qty: inst.LotSize, Price: p, Time: tm, Kind: market.Traded})
		}
		net := func(e, x market.Paise) (float64, error) {
			cb, err := charge(e, market.Buy, in.Time)
			if err != nil {
				return 0, err
			}
			cs, err := charge(x, market.Sell, out.Time)
			if err != nil {
				return 0, err
			}
			return ((x-e)*market.Paise(inst.LotSize) - cb - cs).Rupees(), nil
		}
		var err error
		if tr.net, err = net(tr.entry, tr.ext); err != nil {
			return tr, err
		}
		if tr.slip, err = net(slip(tr.entry, true), slip(tr.ext, false)); err != nil {
			return tr, err
		}
		tr.pct = (tr.ext.Rupees()/tr.entry.Rupees() - 1) * 100
		return tr, nil
	}
	// fillAt is the first 1-minute bar starting at or after tm, within 3 minutes.
	fillAt := func(bars []market.Bar, tm time.Time) (market.Bar, bool) {
		i := sort.Search(len(bars), func(i int) bool { return !bars[i].Time.Before(tm) })
		if i == len(bars) || bars[i].Time.Sub(tm) > 3*time.Minute {
			return market.Bar{}, false
		}
		return bars[i], true
	}
	// exitAt is the first bar at or after tm, else the day's last bar.
	exitAt := func(bars []market.Bar, tm time.Time) market.Bar {
		i := sort.Search(len(bars), func(i int) bool { return !bars[i].Time.Before(tm) })
		if i == len(bars) {
			return bars[len(bars)-1]
		}
		return bars[i]
	}
	type cross struct {
		at time.Time // the signal bar's end
		up bool
	}
	crosses := func(series []market.Bar, from int, fast, slow int) []cross {
		ef, es := indicator.EMA(series, fast), indicator.EMA(series, slow)
		var out []cross
		for i := max(from, 1); i < len(series); i++ {
			if math.IsNaN(es[i-1]) {
				continue
			}
			was, is := ef[i-1] > es[i-1], ef[i] > es[i]
			if was != is {
				out = append(out, cross{series[i].End, is})
			}
		}
		return out
	}

	indexKey := market.Index(market.NSE, *underlying).Key()
	var prev map[string][]market.Bar
	var prevDay market.Date
	counted := [2]int{}
	skipped := 0
	start := time.Now()
	days, err := store.Days(*underlying, f, t)
	if err != nil {
		return err
	}
	for di, d := range days {
		all, err := store.DayBars(ctx, *underlying, d)
		if err != nil {
			return err
		}
		byKey := map[string][]market.Bar{}
		for _, b := range all {
			byKey[b.Key] = append(byKey[b.Key], b)
		}
		warm := prev
		if pos[d]-1 < 0 || allDays[pos[d]-1] != prevDay {
			warm = nil
		}
		prev, prevDay = byKey, d
		idx := byKey[indexKey]
		if len(idx) < 60 {
			continue
		}
		atm := func(px market.Paise) market.Paise {
			return market.Rupees(math.Round(px.Rupees() / *step) * *step)
		}
		exp, ok := nearestExpiry(lots, byKey, d, atm(idx[0].Open), *underlying)
		if !ok {
			continue
		}
		ei, ok := pos[exp]
		if !ok {
			continue
		}
		dte := ei - pos[d]
		if dte > 1 {
			continue
		}
		counted[dte]++
		day0 := d.At(0, 0)
		cutoff, flatAt := day0.Add(entryBy), day0.Add(flat)
		key := func(strike market.Paise, r market.Right) string {
			return market.Option(market.NFO, *underlying, exp, strike, r, 0).Key()
		}
		for _, tf := range tfs {
			series := append(resample(warm[indexKey], tf), resample(idx, tf)...)
			nWarm := len(series) - len(resample(idx, tf))
			openStrike := atm(idx[0].Open)
			optSeries := map[market.Right][]market.Bar{}
			optWarm := map[market.Right]int{}
			for _, r := range []market.Right{market.Call, market.Put} {
				k := key(openStrike, r)
				today := resample(byKey[k], tf)
				optSeries[r] = append(resample(warm[k], tf), today...)
				optWarm[r] = len(optSeries[r]) - len(today)
			}
			for _, p := range emaPairs {
				// Index chart: stop and reverse.
				c := cellIndex[emaCell{dte, true, tf, p[0], p[1]}]
				var open *struct {
					key string
					bar market.Bar
				}
				closeAt := func(tm time.Time) error {
					if open == nil {
						return nil
					}
					tr, err := trade(d, open.key, open.bar, exitAt(byKey[open.key], tm))
					if err != nil {
						return err
					}
					results[c] = append(results[c], tr)
					open = nil
					return nil
				}
				for _, x := range crosses(series, nWarm, p[0], p[1]) {
					if !x.at.Before(flatAt) {
						break
					}
					if err := closeAt(x.at); err != nil {
						return err
					}
					if !x.at.Before(cutoff) {
						continue
					}
					r := market.Put
					if x.up {
						r = market.Call
					}
					ix := exitAt(idx, x.at.Add(-time.Minute)) // the minute that closed the signal bar
					k := key(atm(ix.Close), r)
					b, ok := fillAt(byKey[k], x.at)
					if !ok {
						skipped++
						continue
					}
					open = &struct {
						key string
						bar market.Bar
					}{k, b}
				}
				if err := closeAt(flatAt); err != nil {
					return err
				}

				// Option chart: long only, each right on its own.
				c = cellIndex[emaCell{dte, false, tf, p[0], p[1]}]
				for _, r := range []market.Right{market.Call, market.Put} {
					s := optSeries[r]
					k := key(openStrike, r)
					if len(s)-optWarm[r] < 10 {
						continue
					}
					var in *market.Bar
					for _, x := range crosses(s, optWarm[r], p[0], p[1]) {
						if !x.at.Before(flatAt) {
							break
						}
						if !x.up && in != nil {
							tr, err := trade(d, k, *in, exitAt(byKey[k], x.at))
							if err != nil {
								return err
							}
							results[c] = append(results[c], tr)
							in = nil
						}
						if x.up && in == nil && x.at.Before(cutoff) {
							if b, ok := fillAt(byKey[k], x.at); ok {
								in = &b
							} else {
								skipped++
							}
						}
					}
					if in != nil {
						tr, err := trade(d, k, *in, exitAt(byKey[k], flatAt))
						if err != nil {
							return err
						}
						results[c] = append(results[c], tr)
					}
				}
			}
		}
		if (di+1)%50 == 0 {
			fmt.Fprintf(os.Stderr, "  %d/%d days, %s\n", di+1, len(days), time.Since(start).Round(time.Second))
		}
	}

	type sum struct {
		n               int
		win, net, slip  float64
		pf, pfSlip, avg float64
		dd              float64
	}
	summarize := func(ts []emaTrade) sum {
		var s sum
		var w, l, ws, ls, peak, cum float64
		for _, tr := range ts {
			s.n++
			s.net += tr.net
			s.slip += tr.slip
			s.avg += tr.pct
			if tr.net > 0 {
				s.win++
				w += tr.net
			} else {
				l -= tr.net
			}
			if tr.slip > 0 {
				ws += tr.slip
			} else {
				ls -= tr.slip
			}
			cum += tr.slip
			peak = max(peak, cum)
			s.dd = max(s.dd, peak-cum)
		}
		s.pf, s.pfSlip = math.Inf(1), math.Inf(1)
		if l > 0 {
			s.pf = w / l
		}
		if ls > 0 {
			s.pfSlip = ws / ls
		}
		if s.n > 0 {
			s.win = s.win / float64(s.n) * 100
			s.avg /= float64(s.n)
		}
		return s
	}
	type row struct {
		c         emaCell
		all, a, b sum
	}
	var rows []row
	for ci, c := range cells {
		var a, b []emaTrade
		for _, tr := range results[ci] {
			if tr.day.Before(mid) {
				a = append(a, tr)
			} else {
				b = append(b, tr)
			}
		}
		rows = append(rows, row{c, summarize(results[ci]), summarize(a), summarize(b)})
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
	w.Write([]string{"dte", "chart", "timeframe", "fast", "slow", "trades", "win_pct", "avg_pct", "net_rs", "pf", "net_slip_rs", "pf_slip", "max_dd_slip_rs",
		"h1_trades", "h1_net_slip_rs", "h1_pf_slip", "h2_trades", "h2_net_slip_rs", "h2_pf_slip"})
	ff := func(x float64, p int) string { return strconv.FormatFloat(x, 'f', p, 64) }
	for _, r := range rows {
		w.Write([]string{strconv.Itoa(r.c.dte), r.c.chart(), strconv.Itoa(int(r.c.tf)), strconv.Itoa(r.c.fast), strconv.Itoa(r.c.slow),
			strconv.Itoa(r.all.n), ff(r.all.win, 1), ff(r.all.avg, 2), ff(r.all.net, 0), ff(r.all.pf, 3), ff(r.all.slip, 0), ff(r.all.pfSlip, 3), ff(r.all.dd, 0),
			strconv.Itoa(r.a.n), ff(r.a.slip, 0), ff(r.a.pfSlip, 3), strconv.Itoa(r.b.n), ff(r.b.slip, 0), ff(r.b.pfSlip, 3)})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}

	fmt.Printf("%s %s to %s: %d expiry days (0DTE), %d days before expiry (1DTE). %d signals had no option bar to fill.\n",
		*underlying, f, t, counted[0], counted[1], skipped)
	fmt.Printf("One lot per trade, after charges. 'slip' adds %.0f bps a side (2 ticks minimum). Halves split at %s.\n\n", *slipBps, mid)
	fmt.Printf("%-4s %-6s %3s %-6s | %6s %5s %6s %8s %5s %8s %5s | %8s %5s | %8s %5s\n",
		"dte", "chart", "tf", "ema", "trades", "win%", "avg%", "net", "PF", "slip net", "PF", "h1 slip", "PF", "h2 slip", "PF")
	for _, r := range rows {
		fmt.Printf("%-4d %-6s %3d %-6s | %6d %5.1f %6.2f %8.0f %5.2f %8.0f %5.2f | %8.0f %5.2f | %8.0f %5.2f\n",
			r.c.dte, r.c.chart(), int(r.c.tf), fmt.Sprintf("%d/%d", r.c.fast, r.c.slow),
			r.all.n, r.all.win, r.all.avg, r.all.net, r.all.pf, r.all.slip, r.all.pfSlip, r.a.slip, r.a.pfSlip, r.b.slip, r.b.pfSlip)
	}
	both := 0
	for _, r := range rows {
		if r.a.slip > 0 && r.b.slip > 0 {
			both++
		}
	}
	fmt.Printf("\n%d of %d combinations made money after slippage in both halves.\n", both, len(rows))

	if *tradesOut != "" {
		th, err := os.Create(*tradesOut)
		if err != nil {
			return err
		}
		defer th.Close()
		tw := csv.NewWriter(th)
		tw.Write([]string{"dte", "chart", "timeframe", "fast", "slow", "day", "key", "entry_time", "entry", "exit_time", "exit", "net_rs", "net_slip_rs"})
		for ci, c := range cells {
			for _, tr := range results[ci] {
				tw.Write([]string{strconv.Itoa(c.dte), c.chart(), strconv.Itoa(int(c.tf)), strconv.Itoa(c.fast), strconv.Itoa(c.slow),
					tr.day.String(), tr.key, tr.in.Format("15:04"), tr.entry.String(), tr.out.Format("15:04"), tr.ext.String(), ff(tr.net, 2), ff(tr.slip, 2)})
			}
		}
		tw.Flush()
		if err := tw.Error(); err != nil {
			return err
		}
	}

	var net market.Paise
	n := 0
	for _, r := range rows {
		net += market.Rupees(r.all.slip)
		n += r.all.n
	}
	err = analytics.AppendTrials(analytics.LedgerPath(*dir), []analytics.TrialRecord{{
		Time: time.Now(), Command: "research emacross", Strategy: "ema_cross", Underlying: *underlying,
		Params: strategy.Params{"slip_bps": *slipBps, "step": *step}, From: f, To: t, Count: len(cells), Net: net, Trades: n,
	}})
	if err != nil {
		return fmt.Errorf("trial ledger: %w", err)
	}
	fmt.Printf("all %d combinations: %s (recorded in the trial ledger)\n", len(cells), *out)
	return nil
}
