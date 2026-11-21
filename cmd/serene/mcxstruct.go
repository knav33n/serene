package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/market"
	"serene/internal/mcx"
)

// runMCXStruct reruns the frozen goldm_hma_1h and silverm_hma_1h rules with a
// Dow structure gate: long only while the last swing high and low are HH and
// HL, short only on LH and LL. The swings are a wide zigzag on the 1h bars,
// so they read the broader trend, not every small wiggle. Research only: the
// frozen strategies are untouched.
func runMCXStruct(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcxstruct", flag.ContinueOnError)
	nSw := fs.Int("swings", 2, "swing highs and lows to read; 2 is the last HH/HL pair, more uses --rule")
	dir := fs.String("data", "data/mcx", "data directory")
	costsFile := fs.String("costs", "configs/costs_mcx.yaml", "charges schedule")
	if err := fs.Parse(args); err != nil {
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
	split := market.Date{Year: 2026, Month: time.September, Day: 1}
	pcts := []float64{0.75, 1, 1.5, 2, 3, 4}
	if *nSw > 2 {
		pcts = []float64{0.5, 0.75, 1, 1.5, 2} // five swings a side need smaller ones to exist at all
	}
	modes := []mcx.StructMode{mcx.StructTurn, mcx.StructLate, mcx.StructStrict}
	for _, u := range []struct {
		name   string
		lo, hi market.Date
		cfg    func(mcx.Spec, *cost.Schedule, int64, market.Date, market.Date) mcx.EMAConfig
	}{
		{gemName, gemResearchLo, gemResearchHi, ghmConfig},
		{smName, smResearchLo, smResearchHi, func(sp mcx.Spec, c *cost.Schedule, slip int64, f, t market.Date) mcx.EMAConfig {
			_, cfg, _ := smConfig("silverm_hma_1h", sp, c, slip, f, t)
			return cfg
		}},
	} {
		spec := mcx.Specs[u.name]
		days, err := store.Days(u.name, u.lo, u.lo.AddDays(3650))
		if err != nil {
			return err
		}
		last := days[len(days)-1]
		bars, err := loadMCXBars(ctx, store, []string{u.name}, u.lo.AddDays(-30), last)
		if err != nil {
			return err
		}
		ed := mcx.PrepareEMA(bars[u.name], time.Hour)
		fmt.Printf("\n%s 1h HMA-21 turn, research %s..%s (halves split %s), after = %s..%s, 1 tick except net3\n",
			u.name, u.lo, u.hi, split, u.hi.AddDays(1), last)
		fmt.Printf("%-18s %6s %10s %10s %10s %10s %5s %9s %10s %10s %9s\n",
			"variant", "trades", "net", "net3", "H1", "H2", "PF", "maxDD", "long", "short", "after")
		row := func(label string, pct float64, mode mcx.StructMode, rule mcx.StructRule) error {
			run := func(slip int64, from, to market.Date) ([]mcx.Trade, error) {
				c := u.cfg(spec, costs, slip, from, to)
				c.StructPct, c.StructMode, c.StructN, c.StructRule = pct, mode, *nSw, rule
				return mcx.EMABacktest(ed, c)
			}
			ts, err := run(1, u.lo, u.hi)
			if err != nil {
				return err
			}
			ts3, err := run(3, u.lo, u.hi)
			if err != nil {
				return err
			}
			after, err := run(1, u.hi.AddDays(1), last)
			if err != nil {
				return err
			}
			var h1, h2, long, short float64
			for _, tr := range ts {
				n := tr.Net.Rupees()
				if market.DateOf(tr.Entry).Before(split) {
					h1 += n
				} else {
					h2 += n
				}
				if tr.Side == market.Buy {
					long += n
				} else {
					short += n
				}
			}
			s, s3, sa := summarize(ts), summarize(ts3), summarize(after)
			fmt.Printf("%-18s %6d %10.0f %10.0f %10.0f %10.0f %5s %9.0f %10.0f %10.0f %9.0f\n",
				label, s.n, s.net, s3.net, h1, h2, pf(s.pf), s.dd, long, short, sa.net)
			return nil
		}
		if err := row("frozen (no gate)", 0, 0, 0); err != nil {
			return err
		}
		rules := []mcx.StructRule{mcx.RuleAll}
		if *nSw > 2 {
			rules = []mcx.StructRule{mcx.RuleAll, mcx.RuleMost, mcx.RuleEnds}
		}
		for _, r := range rules {
			for _, m := range modes {
				for _, p := range pcts {
					label := fmt.Sprintf("zz %.2g%% %s", p, m)
					if *nSw > 2 {
						label = fmt.Sprintf("%s %.2g%% %s", r, p, m)
					}
					if err := row(label, p, m, r); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
