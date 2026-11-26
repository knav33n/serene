// Command hmagridbt (scratch, uncommitted) runs the nifty_hma_crossover rules over a grid of timeframes and HMA pairs on
// any index in the Fyers store: every up-cross of the 09:15 ATM nearest-expiry CE/PE buys at the next bar's open, sized
// floor(capital/(price×lot)) lots; the next down-cross sells at the next bar's open, else the 15:15 close.
// It writes one CSV row per trade per variant.
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/market"
	"serene/internal/paper"
)

type variant struct{ tf, fast, slow int }

func main() {
	dir := flag.String("data", os.Getenv("HOME")+"/INSIGHTS/fyers", "")
	und := flag.String("underlying", "SENSEX", "")
	exch := flag.String("index-exchange", "BSE", "")
	optExch := flag.String("opt-exchange", "BFO", "")
	step := flag.Float64("step", 100, "ATM rounding")
	from := flag.String("from", "2022-01-01", "")
	to := flag.String("to", "2026-11-26", "")
	costsF := flag.String("costs", "configs/costs.yaml", "")
	capital := flag.Float64("capital", 100000, "")
	slipBps := flag.Float64("slip-bps", 30, "")
	out := flag.String("out", "grid.csv", "")
	warmDays := flag.Int("warm", 6, "")
	tfs := flag.String("tf", "3,5,10,15", "")
	pairs := flag.String("pairs", "5/13,5/21,8/21,9/21,8/34,13/34,21/34,13/55,21/55,34/55,13/89,21/89,34/89", "")
	flag.Parse()
	ctx := context.Background()
	f, _ := market.ParseDate(*from)
	t, _ := market.ParseDate(*to)
	costs, err := cost.Load(*costsF)
	must(err)
	store, err := data.Open(*dir)
	must(err)
	insts, err := store.Contracts(ctx, *und)
	must(err)
	lots := map[string]market.Instrument{}
	for _, in := range insts {
		lots[in.Key()] = in
	}
	var vs []variant
	for _, tfS := range strings.Split(*tfs, ",") {
		tf, _ := strconv.Atoi(tfS)
		for _, p := range strings.Split(*pairs, ",") {
			a, b, _ := strings.Cut(p, "/")
			fa, _ := strconv.Atoi(a)
			sl, _ := strconv.Atoi(b)
			vs = append(vs, variant{tf, fa, sl})
		}
	}
	days, err := store.Days(*und, f.AddDays(-14), t)
	must(err)
	slip := func(p market.Paise, buy bool) market.Paise {
		s := max(market.Paise(math.Round(float64(p)**slipBps/1e4)), 10)
		if buy {
			return p + s
		}
		return max(p-s, 5)
	}
	floor := time.Date(2024, 10, 1, 10, 0, 0, 0, market.IST) // costs.yaml starts here; older fills pay these rates
	net := func(inst market.Instrument, qty int64, e, x market.Paise, in, o time.Time) float64 {
		if in.Before(floor) {
			in, o = floor, floor
		}
		cb, err := costs.Charges(market.Fill{Instrument: inst, Side: market.Buy, Qty: qty, Price: e, Time: in, Kind: market.Traded})
		must(err)
		cs, err := costs.Charges(market.Fill{Instrument: inst, Side: market.Sell, Qty: qty, Price: x, Time: o, Kind: market.Traded})
		must(err)
		return ((x-e)*market.Paise(qty) - cb - cs).Rupees()
	}
	fh, err := os.Create(*out)
	must(err)
	w := csv.NewWriter(fh)
	w.Write([]string{"variant", "day", "instrument", "entry_time", "entry", "qty", "exit_time", "exit", "reason", "net", "net_slip", "lot"})
	indexKey := market.Index(market.Exchange(*exch), *und).Key()
	var warmBuf []map[string][]market.Bar
	skipped, replayed := 0, 0
	for _, d := range days {
		all, err := store.DayBars(ctx, *und, d)
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
		if d.Before(f) || len(byKey) < 3 {
			continue
		}
		idx := byKey[indexKey]
		if len(idx) == 0 || !idx[0].Time.Equal(d.At(9, 15)) {
			skipped++
			continue
		}
		strike := market.Rupees(math.Round(idx[0].Open.Rupees() / *step) * *step)
		exp, ok := nearestExpiry(lots, byKey, d, strike)
		if !ok {
			skipped++
			continue
		}
		replayed++
		for _, r := range []market.Right{market.Call, market.Put} {
			k := market.Option(market.Exchange(*optExch), *und, exp, strike, r, 0).Key()
			inst, ok := lots[k]
			if !ok || len(byKey[k]) == 0 {
				continue
			}
			var raw []market.Bar
			for _, wb := range warm {
				raw = append(raw, wb[k]...)
			}
			raw = append(raw, byKey[k]...)
			byTF := map[int][]market.Bar{}
			for _, v := range vs {
				bars, ok := byTF[v.tf]
				if !ok {
					bars, err = market.Resample(raw, market.NSESession, market.Timeframe(v.tf))
					must(err)
					byTF[v.tf] = bars
				}
				cfg := paper.DefaultHMACross(d)
				cfg.Fast, cfg.Slow, cfg.TF = v.fast, v.slow, market.Timeframe(v.tf)
				crosses := paper.HMACrosses([]paper.Leg{{Instrument: inst, Bars: bars}}, cfg)
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
				name := fmt.Sprintf("%dm_%d/%d", v.tf, v.fast, v.slow)
				var pos *market.Bar
				var qty int64
				for _, c := range crosses {
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
						write(w, name, d, inst, *pos, qty, nb.Time, nb.Open, "down-cross", net, slip)
						pos = nil
					}
				}
				if pos != nil {
					write(w, name, d, inst, *pos, qty, eod.End, eod.Close, "15:15", net, slip)
				}
			}
		}
	}
	w.Flush()
	fh.Close()
	fmt.Printf("days replayed %d, skipped %d\n", replayed, skipped)
}

func write(w *csv.Writer, name string, d market.Date, inst market.Instrument, e market.Bar, qty int64, xt time.Time, x market.Paise, reason string,
	net func(market.Instrument, int64, market.Paise, market.Paise, time.Time, time.Time) float64, slip func(market.Paise, bool) market.Paise) {
	n1 := net(inst, qty, e.Open, x, e.Time, xt)
	n2 := net(inst, qty, slip(e.Open, true), slip(x, false), e.Time, xt)
	w.Write([]string{name, d.String(), inst.Key(), e.Time.Format("15:04"), fmt.Sprint(e.Open.Rupees()), fmt.Sprint(qty), xt.Format("15:04"),
		fmt.Sprint(x.Rupees()), reason, fmt.Sprintf("%.2f", n1), fmt.Sprintf("%.2f", n2), fmt.Sprint(inst.LotSize)})
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
