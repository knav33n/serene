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

// pdExits are the exits tried, fixed before the first run.
var pdExits = []string{"eod", "level", "30/60", "idx200", "opt200",
	"sl_pdlvl", "sl_idx50", "sl_idx100", "sl_opt20", "sl_opt30", "sl_optpdh"}

type pdTrade struct {
	day        market.Date
	key        string
	right      market.Right
	dte        int
	exit       string
	why        string // trail, stop, time or level
	in, out    time.Time
	entry, ext market.Paise
	net, slip  float64
}

// runPDBreak tests a previous-day breakout on NIFTY options. The strike is the
// one nearest the previous day's NIFTY low plus 1%, nearest expiry. The call
// is bought when NIFTY closes a minute above its previous day high and the call
// closes a minute above its own previous day high; the put when NIFTY closes
// below its previous day low and the put closes above its own previous day
// high. One trade per right per day, filled at the next minute's open.
func runPDBreak(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pdbreak", flag.ContinueOnError)
	underlying := fs.String("underlying", "NIFTY", "underlying name")
	from := fs.String("from", "2024-12-27", "first day")
	to := fs.String("to", "2026-04-30", "last day")
	split := fs.String("split", "2025-09-01", "first day of the second half")
	dir := fs.String("data", "data/upstox", "data directory")
	holdout := fs.String("holdout", "configs/holdout.yaml", "data held back from research")
	costsFile := fs.String("costs", "configs/costs.yaml", "charges schedule")
	slipBps := fs.Float64("slip-bps", 30, "slippage per side, at least 2 ticks")
	step := fs.Float64("step", 50, "strike step")
	offset := fs.Float64("offset-pct", 1, "strike = previous day low plus this percent")
	window := fs.Int("window", 0, "minutes: the index and option breaks must be at most this far apart (0: any)")
	trigPts := fs.Float64("trail-after", 200, "idx200/opt200: points in favour, on NIFTY or the premium, before trailing")
	trailPts := fs.Float64("trail", 50, "idx200/opt200: trailing distance in points below the best close")
	tradesOut := fs.String("trades", "reports/pdbreak_trades.csv", "every trade")
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
	allDays, err := store.Days(*underlying, f.AddDays(-10), t.AddDays(15))
	if err != nil {
		return err
	}
	pos := map[market.Date]int{}
	for i, d := range allDays {
		pos[d] = i
	}

	slip := func(p market.Paise, buy bool) market.Paise {
		s := max(market.Paise(math.Round(float64(p)**slipBps/1e4)), 10)
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
	hiLo := func(bars []market.Bar) (hi, lo market.Paise) {
		for i, b := range bars {
			if i == 0 || b.High > hi {
				hi = b.High
			}
			if i == 0 || b.Low < lo {
				lo = b.Low
			}
		}
		return
	}

	indexKey := market.Index(market.NSE, *underlying).Key()
	var trades []pdTrade
	var prev map[string][]market.Bar
	var prevDay market.Date
	days, err := store.Days(*underlying, f.AddDays(-7), t)
	if err != nil {
		return err
	}
	eligible, noPrevBars, noFill := 0, 0, 0
	signals := map[market.Right]int{}
	start := time.Now()
	for di, d := range days {
		all, err := store.DayBars(ctx, *underlying, d)
		if err != nil {
			return err
		}
		byKey := map[string][]market.Bar{}
		for _, b := range all {
			byKey[b.Key] = append(byKey[b.Key], b)
		}
		yday, ydayKey := prev, prevDay
		prev, prevDay = byKey, d
		if d.Before(f) || yday == nil || pos[d]-1 < 0 || allDays[pos[d]-1] != ydayKey {
			continue
		}
		idx, yidx := byKey[indexKey], yday[indexKey]
		if len(idx) < 60 || len(yidx) < 60 {
			continue
		}
		pdh, pdl := hiLo(yidx)
		strike := market.Rupees(math.Round(pdl.Rupees()*(1+*offset/100) / *step) * *step)
		exp, ok := nearestExpiry(lots, byKey, d, strike, *underlying)
		if !ok {
			continue
		}
		eligible++
		dte := pos[exp] - pos[d]
		day0 := d.At(0, 0)
		cutoff, flatAt := day0.Add(15*time.Hour), day0.Add(15*time.Hour+15*time.Minute)
		idxAt := map[time.Time]market.Bar{}
		for _, b := range idx {
			idxAt[b.Time] = b
		}
		for _, r := range []market.Right{market.Call, market.Put} {
			key := market.Option(market.NFO, *underlying, exp, strike, r, 0).Key()
			bars, ybars := byKey[key], yday[key]
			if len(bars) == 0 {
				continue
			}
			if len(ybars) == 0 {
				noPrevBars++
				continue
			}
			oph, _ := hiLo(ybars)
			beyond := func(c market.Paise) bool {
				if r == market.Put {
					return c < pdl
				}
				return c > pdh
			}
			// The latest index close crossing the level, at or before each minute;
			// the first bar counts as a crossing if it is already beyond.
			idxCross := map[time.Time]time.Time{}
			var lastIdx time.Time
			was := false
			for _, ib := range idx {
				is := beyond(ib.Close)
				if is && !was {
					lastIdx = ib.Time
				}
				was = is
				idxCross[ib.Time] = lastIdx
			}
			sig := -1
			var lastOpt time.Time
			optWas := false
			for i, b := range bars {
				optIs := b.Close > oph
				if optIs && !optWas {
					lastOpt = b.Time
				}
				optWas = optIs
				if !b.End.Before(cutoff) {
					break
				}
				ib, ok := idxAt[b.Time]
				if !ok {
					continue
				}
				if !beyond(ib.Close) || !optIs {
					continue
				}
				if *window > 0 {
					gap := lastOpt.Sub(idxCross[b.Time])
					if gap < 0 {
						gap = -gap
					}
					if gap > time.Duration(*window)*time.Minute {
						continue
					}
				}
				sig = i
				break
			}
			if sig < 0 {
				continue
			}
			signals[r]++
			if sig+1 >= len(bars) || bars[sig+1].Time.Sub(bars[sig].End) > 3*time.Minute {
				noFill++
				continue
			}
			inBar := bars[sig+1]
			entry := inBar.Open
			inst := lots[key]
			ref := idxAt[inBar.Time].Open
			if ref == 0 {
				ref = idx[0].Open
			}
			trig, trail := market.Rupees(*trigPts), market.Rupees(*trailPts)
			for _, ex := range pdExits {
				var xp market.Paise
				var xt time.Time
				done := false
				var best market.Paise // best favourable close-to-entry move so far
				armed := false
				why := "time"
				for j := sig + 1; j < len(bars) && !done; j++ {
					b := bars[j]
					if !b.Time.Before(flatAt) {
						xp, xt, done = b.Open, b.Time, true
						break
					}
					switch ex {
					case "level":
						if j > sig+1 && b.Close < oph && j+1 < len(bars) {
							xp, xt, done, why = bars[j+1].Open, bars[j+1].Time, true, "stop"
						}
					case "30/60":
						stop, tgt := entry*70/100, entry*160/100
						if b.Low <= stop {
							xp, xt, done, why = min(stop, b.Open), b.Time, true, "stop"
						} else if b.High >= tgt {
							xp, xt, done, why = max(tgt, b.Open), b.Time, true, "target"
						}
					case "sl_pdlvl", "sl_idx50", "sl_idx100", "sl_opt20", "sl_opt30", "sl_optpdh":
						// The idx200 trail, plus a stop on closes until it arms.
						ib, ok := idxAt[b.Time]
						if !ok {
							continue
						}
						move := ib.Close - ref
						if r == market.Put {
							move = ref - ib.Close
						}
						best = max(best, move)
						if best >= trig {
							armed = true
						}
						if j+1 >= len(bars) {
							continue
						}
						if armed && move <= best-trail {
							xp, xt, done, why = bars[j+1].Open, bars[j+1].Time, true, "trail"
							continue
						}
						if armed {
							continue
						}
						var hit bool
						switch ex {
						case "sl_pdlvl":
							hit = (r == market.Call && ib.Close < pdh) || (r == market.Put && ib.Close > pdl)
						case "sl_idx50":
							hit = move <= -market.Rupees(50)
						case "sl_idx100":
							hit = move <= -market.Rupees(100)
						case "sl_opt20":
							hit = b.Close <= entry*80/100
						case "sl_opt30":
							hit = b.Close <= entry*70/100
						case "sl_optpdh":
							hit = j > sig+1 && b.Close < oph
						}
						if hit {
							xp, xt, done, why = bars[j+1].Open, bars[j+1].Time, true, "stop"
						}
					case "idx200", "opt200":
						// No stop until the move reaches trail-after; then exit on
						// a close more than trail below the best close.
						var move market.Paise
						if ex == "opt200" {
							move = b.Close - entry
						} else {
							ib, ok := idxAt[b.Time]
							if !ok {
								continue
							}
							move = ib.Close - ref
							if r == market.Put {
								move = ref - ib.Close
							}
						}
						best = max(best, move)
						if best >= trig {
							armed = true
						}
						if armed && move <= best-trail && j+1 < len(bars) {
							xp, xt, done, why = bars[j+1].Open, bars[j+1].Time, true, "trail"
						}
					}
				}
				if !done {
					last := bars[len(bars)-1]
					xp, xt = last.Close, last.End
				}
				net, err := roundTrip(inst, entry, xp, inBar.Time, xt)
				if err != nil {
					return err
				}
				sl, err := roundTrip(inst, slip(entry, true), slip(xp, false), inBar.Time, xt)
				if err != nil {
					return err
				}
				trades = append(trades, pdTrade{d, key, r, dte, ex, why, inBar.Time, xt, entry, xp, net, sl})
			}
		}
		if (di+1)%50 == 0 {
			fmt.Fprintf(os.Stderr, "  %d/%d days, %s\n", di+1, len(days), time.Since(start).Round(time.Second))
		}
	}

	type sum struct {
		n            int
		win, net, sl float64
		pf, dd       float64
	}
	summarize := func(ts []pdTrade) sum {
		var s sum
		var w, l, peak, cum float64
		for _, tr := range ts {
			s.n++
			s.net += tr.net
			s.sl += tr.slip
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
	sort.SliceStable(trades, func(i, j int) bool { return trades[i].in.Before(trades[j].in) })
	fmt.Printf("%s %s to %s: %d days with a strike, %d call and %d put signals; %d strikes had no bars the day before, %d signals had no next bar.\n",
		*underlying, f, t, eligible, signals[market.Call], signals[market.Put], noPrevBars, noFill)
	fmt.Printf("Strike nearest previous-day low +%.1f%% (step %.0f), nearest expiry, one lot, charges + %.0f bps a side. Halves split at %s.\n\n", *offset, *step, *slipBps, mid)
	fmt.Printf("%-6s %-5s %-6s | %6s %5s %9s %9s %5s %9s | %5s %9s %5s | %5s %9s %5s\n",
		"exit", "side", "dte", "trades", "win%", "gross net", "slip net", "PF", "max DD", "h1 n", "h1 slip", "PF", "h2 n", "h2 slip", "PF")
	type group struct {
		side, dte string
		keep      func(pdTrade) bool
	}
	groups := []group{
		{"both", "all", func(pdTrade) bool { return true }},
		{"CE", "all", func(tr pdTrade) bool { return tr.right == market.Call }},
		{"PE", "all", func(tr pdTrade) bool { return tr.right == market.Put }},
		{"both", "0", func(tr pdTrade) bool { return tr.dte == 0 }},
		{"both", "1", func(tr pdTrade) bool { return tr.dte == 1 }},
		{"both", "2+", func(tr pdTrade) bool { return tr.dte >= 2 }},
		{"both", "@9:16", func(tr pdTrade) bool { return tr.in.Hour() == 9 && tr.in.Minute() <= 16 }},
		{"both", ">9:16", func(tr pdTrade) bool { return !(tr.in.Hour() == 9 && tr.in.Minute() <= 16) }},
	}
	for _, ex := range pdExits {
		for _, g := range groups {
			var all, a, b []pdTrade
			for _, tr := range trades {
				if tr.exit != ex || !g.keep(tr) {
					continue
				}
				all = append(all, tr)
				if tr.day.Before(mid) {
					a = append(a, tr)
				} else {
					b = append(b, tr)
				}
			}
			s, sa, sb := summarize(all), summarize(a), summarize(b)
			fmt.Printf("%-6s %-5s %-6s | %6d %5.1f %9.0f %9.0f %5.2f %9.0f | %5d %9.0f %5.2f | %5d %9.0f %5.2f\n",
				ex, g.side, g.dte, s.n, s.win, s.net, s.sl, s.pf, s.dd, sa.n, sa.sl, sa.pf, sb.n, sb.sl, sb.pf)
		}
		fmt.Println()
	}

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
		tw.Write([]string{"exit", "why", "day", "key", "dte", "entry_time", "entry", "exit_time", "exit_px", "net_rs", "net_slip_rs"})
		ff := func(x float64) string { return strconv.FormatFloat(x, 'f', 2, 64) }
		for _, tr := range trades {
			tw.Write([]string{tr.exit, tr.why, tr.day.String(), tr.key, strconv.Itoa(tr.dte), tr.in.Format("15:04"), tr.entry.String(),
				tr.out.Format("15:04"), tr.ext.String(), ff(tr.net), ff(tr.slip)})
		}
		tw.Flush()
		if err := tw.Error(); err != nil {
			return err
		}
	}

	var net market.Paise
	for _, tr := range trades {
		net += market.Rupees(tr.slip)
	}
	err = analytics.AppendTrials(analytics.LedgerPath(*dir), []analytics.TrialRecord{{
		Time: time.Now(), Command: "research pdbreak", Strategy: "pd_break", Underlying: *underlying,
		Params: strategy.Params{"slip_bps": *slipBps, "step": *step, "offset_pct": *offset, "window": float64(*window), "trail_after": *trigPts, "trail": *trailPts}, From: f, To: t, Count: len(pdExits), Net: net, Trades: len(trades),
	}})
	if err != nil {
		return fmt.Errorf("trial ledger: %w", err)
	}
	fmt.Printf("trades: %s (recorded in the trial ledger)\n", *tradesOut)
	return nil
}
