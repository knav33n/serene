package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"

	"serene/internal/data"
	"serene/internal/market"
	"serene/internal/nse"
	"serene/internal/regime"
)

// runDayFeatures writes one row per trading day of what was known before or
// early in the session: the opening gap against the official previous close,
// the market regime and India VIX at the previous close, and the index's move
// from the open by 10:00. Research filters join it to trade files by date.
func runDayFeatures(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dayfeatures", flag.ContinueOnError)
	underlying := fs.String("underlying", "NIFTY", "underlying name")
	from := fs.String("from", "2024-12-27", "first day")
	to := fs.String("to", "2026-04-30", "last day")
	dir := fs.String("data", "data/upstox", "intraday data directory")
	marketDir := fs.String("market", "data/market", "NSE index history")
	out := fs.String("out", "reports/dayfeatures.csv", "output CSV")
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
	cat, err := nse.LoadCatalog("configs/indices.yaml")
	if err != nil {
		return err
	}
	ms, err := data.Open(*marketDir)
	if err != nil {
		return err
	}
	defer ms.Close()
	rows, err := loadIndexRows(ctx, ms)
	if err != nil {
		return err
	}
	states := map[market.Date]string{}
	for _, d := range regime.Timeline(regime.NewPanel(rows, marketMeta(cat))) {
		states[d.Date] = string(d.State)
	}
	nifty, vix := map[market.Date]float64{}, map[market.Date]float64{}
	for _, r := range rows {
		switch r.Name {
		case "Nifty 50":
			nifty[r.Date] = r.Close.Rupees()
		case "India VIX":
			vix[r.Date] = r.Close.Rupees()
		}
	}
	// latestBefore finds the value on the last date before d.
	latestBefore := func(m map[market.Date]float64, d market.Date) (float64, bool) {
		for i := 1; i <= 10; i++ {
			if v, ok := m[d.AddDays(-i)]; ok {
				return v, true
			}
		}
		return math.NaN(), false
	}
	stateBefore := func(d market.Date) string {
		for i := 1; i <= 10; i++ {
			if s, ok := states[d.AddDays(-i)]; ok {
				return s
			}
		}
		return ""
	}

	store, err := data.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()
	days, err := store.Days(*underlying, f, t)
	if err != nil {
		return err
	}
	fh, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer fh.Close()
	w := csv.NewWriter(fh)
	w.Write([]string{"day", "open", "prev_close", "gap_pct", "regime", "vix", "close_0959", "move10_pct"})
	indexKey := market.Index(market.NSE, *underlying).Key()
	ff := func(x float64) string { return strconv.FormatFloat(x, 'f', 3, 64) }
	for _, d := range days {
		all, err := store.DayBars(ctx, *underlying, d)
		if err != nil {
			return err
		}
		var open, at10 float64
		ten := d.At(10, 0)
		for _, b := range all {
			if b.Key != indexKey {
				continue
			}
			if open == 0 {
				open = b.Open.Rupees()
			}
			if b.End.After(ten) {
				break
			}
			at10 = b.Close.Rupees()
		}
		if open == 0 {
			continue
		}
		pc, _ := latestBefore(nifty, d)
		v, _ := latestBefore(vix, d)
		w.Write([]string{d.String(), ff(open), ff(pc), ff((open/pc - 1) * 100), stateBefore(d), ff(v), ff(at10), ff((at10/open - 1) * 100)})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	fmt.Printf("%d days: %s\n", len(days), *out)
	return nil
}
