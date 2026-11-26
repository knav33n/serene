// Command hmacrossbt backtests nifty_hma_crossover (scratch, uncommitted):
// every 10m HMA 13/34 up-cross of the 09:15 ATM (round 100) nearest-expiry CE
// and PE buys at the next bar's open, sized floor(capital/(price×lot)) lots;
// the next down-cross sells at the next bar's open, else 15:15.
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/market"
	"serene/internal/paper"
)

func main() {
	dir := flag.String("data", os.Getenv("HOME")+"/INSIGHTS/fyers", "")
	from := flag.String("from", "2019-01-01", "")
	to := flag.String("to", "2026-11-23", "")
	costsF := flag.String("costs", "configs/costs.yaml", "")
	capital := flag.Float64("capital", 100000, "")
	slipBps := flag.Float64("slip-bps", 30, "")
	out := flag.String("out", "trades.csv", "")
	warmDays := flag.Int("warm", 4, "")
	firstCross := flag.Duration("first-cross", 0, "after 09:15: ignore crosses on bars closing before this")
	onePerDay := flag.Bool("one-per-day", false, "only the day's first trade, across CE and PE")
	flag.Parse()
	ctx := context.Background()
	f, _ := market.ParseDate(*from)
	t, _ := market.ParseDate(*to)
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
	days, err := store.Days("NIFTY", f.AddDays(-10), t)
	must(err)
	slip := func(p market.Paise, buy bool) market.Paise {
		s := max(market.Paise(math.Round(float64(p)**slipBps/1e4)), 10)
		if buy {
			return p + s
		}
		return max(p-s, 5)
	}
	net := func(inst market.Instrument, qty int64, e, x market.Paise, in, o time.Time) float64 {
		cb, err := costs.Charges(market.Fill{Instrument: inst, Side: market.Buy, Qty: qty, Price: e, Time: in, Kind: market.Traded})
		must(err)
		cs, err := costs.Charges(market.Fill{Instrument: inst, Side: market.Sell, Qty: qty, Price: x, Time: o, Kind: market.Traded})
		must(err)
		return ((x-e)*market.Paise(qty) - cb - cs).Rupees()
	}
	fh, err := os.Create(*out)
	must(err)
	w := csv.NewWriter(fh)
	w.Write([]string{"day", "instrument", "entry_time", "entry", "qty", "exit_time", "exit", "reason", "net", "net_slip"})
	indexKey := market.Index(market.NSE, "NIFTY").Key()
	var warmBuf []map[string][]market.Bar
	skipped, replayed := 0, 0
	for _, d := range days {
		all, err := store.DayBars(ctx, "NIFTY", d)
		must(err)
		byKey := map[string][]market.Bar{}
		for _, b := range all {
			byKey[b.Key] = append(byKey[b.Key], b)
		}
		warm := warmBuf
		warmBuf = append(warmBuf, byKey)
		if len(warmBuf) > *warmDays {
			warmBuf = warmBuf[len(warmBuf)-*warmDays:]
		}
		if d.Before(f) {
			continue
		}
		idx := byKey[indexKey]
		if len(idx) == 0 || !idx[0].Time.Equal(d.At(9, 15)) {
			skipped++
			continue
		}
		strike := market.Rupees(math.Round(idx[0].Open.Rupees()/100) * 100)
		exp, ok := nearestExpiry(lots, byKey, d, strike)
		if !ok {
			skipped++
			continue
		}
		replayed++
		var dayTrades []trade
		for _, r := range []market.Right{market.Call, market.Put} {
			k := market.Option(market.NFO, "NIFTY", exp, strike, r, 0).Key()
			inst, ok := lots[k]
			if !ok || len(byKey[k]) == 0 {
				continue
			}
			var bars []market.Bar
			for _, wb := range warm {
				bars = append(bars, wb[k]...)
			}
			bars = append(bars, byKey[k]...)
			bars, err = market.Resample(bars, market.NSESession, 10)
			must(err)
			cfg := paper.DefaultHMACross(d)
			crosses := paper.HMACrosses([]paper.Leg{{Instrument: inst, Bars: bars}}, cfg)
			// bar opening at a time, today
			openAt := map[time.Time]market.Bar{}
			var eod market.Bar
			for _, b := range bars {
				if market.DateOf(b.Time) == d {
					openAt[b.Time] = b
					if b.Time.Before(d.At(15, 15)) {
						eod = b
					}
				}
			}
			cut := d.At(15, 15)
			var pos *market.Bar
			var qty int64
			for _, c := range crosses {
				if c.End.Before(d.At(9, 15).Add(*firstCross)) {
					continue
				}
				nb, ok := openAt[c.End]
				if !ok || !nb.Time.Before(cut) {
					continue
				}
				if c.Up && pos == nil {
					l := int64(*capital / (nb.Open.Rupees() * float64(inst.LotSize)))
					if l < 1 {
						continue
					}
					b := nb
					pos, qty = &b, l*inst.LotSize
				} else if !c.Up && pos != nil {
					dayTrades = append(dayTrades, trade{inst, *pos, qty, nb.Time, nb.Open, "down-cross"})
					pos = nil
				}
			}
			if pos != nil {
				dayTrades = append(dayTrades, trade{inst, *pos, qty, eod.End, eod.Close, "15:15"})
			}
		}
		sort.SliceStable(dayTrades, func(a, b int) bool { return dayTrades[a].e.Time.Before(dayTrades[b].e.Time) })
		if *onePerDay && len(dayTrades) > 1 {
			dayTrades = dayTrades[:1]
		}
		for _, tr := range dayTrades {
			write(w, d, tr.inst, tr.e, tr.qty, tr.xt, tr.x, tr.reason, net, slip)
		}
	}
	w.Flush()
	fh.Close()
	fmt.Printf("days replayed %d, skipped %d\n", replayed, skipped)
}

type trade struct {
	inst   market.Instrument
	e      market.Bar
	qty    int64
	xt     time.Time
	x      market.Paise
	reason string
}

func write(w *csv.Writer, d market.Date, inst market.Instrument, e market.Bar, qty int64, xt time.Time, x market.Paise, reason string,
	net func(market.Instrument, int64, market.Paise, market.Paise, time.Time, time.Time) float64, slip func(market.Paise, bool) market.Paise) {
	n1 := net(inst, qty, e.Open, x, e.Time, xt)
	n2 := net(inst, qty, slip(e.Open, true), slip(x, false), e.Time, xt)
	w.Write([]string{d.String(), inst.Key(), e.Time.Format("15:04"), fmt.Sprint(e.Open.Rupees()), fmt.Sprint(qty), xt.Format("15:04"),
		fmt.Sprint(x.Rupees()), reason, fmt.Sprintf("%.2f", n1), fmt.Sprintf("%.2f", n2)})
}

func nearestExpiry(lots map[string]market.Instrument, byKey map[string][]market.Bar, d market.Date, strike market.Paise) (market.Date, bool) {
	var best market.Date
	found := false
	keys := make([]string, 0, len(lots))
	for k := range lots {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		in := lots[key]
		if in.Kind != market.KindOption || in.Strike != strike || in.Right != market.Call || in.Expiry.Before(d) || len(byKey[key]) == 0 {
			continue
		}
		if !found || in.Expiry.Before(best) {
			best, found = in.Expiry, true
		}
	}
	return best, found
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
