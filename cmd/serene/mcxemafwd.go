package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"serene/internal/analytics"
	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/market"
	"serene/internal/mcx"
	"serene/internal/strategy"
)

// The GOLDM EMA cross frozen on 2026-11-11 from `serene research mcxema`; see
// docs/strategies/goldm_ema_15m.md. None of these may change during the
// forward test: a change is a new strategy that needs data of its own.
const (
	gemName     = "GOLDM"
	gemFrame    = 15 * time.Minute
	gemFast     = 13
	gemSlow     = 34
	gemMinTrade = 100 // trades needed before the one judgement
)

var (
	gemForward    = market.Date{Year: 2026, Month: time.November, Day: 11}
	gemResearchLo = market.Date{Year: 2026, Month: time.June, Day: 15}
	gemResearchHi = market.Date{Year: 2026, Month: time.November, Day: 10}
)

// runMCXEMAForward runs the frozen GOLDM 15m 13/34 long+short cross on
// sessions from 2026-11-11. Until --judge it prints only how many trades
// there are, so the forward result isn't watched and stopped on a good day.
// --replay reruns the research window to show the frozen code matches it.
func runMCXEMAForward(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcxemafwd", flag.ContinueOnError)
	toS := fs.String("to", "", "last session (default: the last one in the data)")
	judge := fs.Bool("judge", false, "show the result: do this once, at "+fmt.Sprint(gemMinTrade)+"+ trades")
	replay := fs.Bool("replay", false, "rerun the research window "+gemResearchLo.String()+" to "+gemResearchHi.String())
	dir := fs.String("data", "data/mcx", "data directory")
	costsFile := fs.String("costs", "configs/costs_mcx.yaml", "charges schedule")
	if err := fs.Parse(args); err != nil {
		return err
	}
	spec, ok := mcx.Specs[gemName]
	if !ok {
		return fmt.Errorf("no contract spec for %s", gemName)
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

	from, to := gemForward, market.Date{}
	if *replay {
		from, to = gemResearchLo, gemResearchHi
	} else if *toS != "" {
		if to, err = market.ParseDate(*toS); err != nil {
			return err
		}
	} else {
		days, err := store.Days(gemName, from, from.AddDays(3650))
		if err != nil {
			return err
		}
		if len(days) == 0 {
			fmt.Printf("%s 15m %d/%d forward test: no sessions from %s fetched yet\n", gemName, gemFast, gemSlow, from)
			return nil
		}
		to = days[len(days)-1]
	}
	if to.Before(from) {
		return fmt.Errorf("--to %s is before the forward start %s", to, from)
	}

	// Same 30-day warmup as the research run.
	bars, err := loadMCXBars(ctx, store, []string{gemName}, from.AddDays(-30), to)
	if err != nil {
		return err
	}
	ed := mcx.PrepareEMA(bars[gemName], gemFrame)
	run := func(slip int64) ([]mcx.Trade, error) {
		return mcx.EMABacktest(ed, mcx.EMAConfig{Name: gemName, Spec: spec, Frame: gemFrame, Fast: gemFast,
			Slow: gemSlow, Slip: slip, Costs: costs, From: from, To: to})
	}
	ts, err := run(1)
	if err != nil {
		return err
	}
	label := "forward test"
	if *replay {
		label = "research replay"
	}
	fmt.Printf("%s 15m %d/%d long+short, %s %s to %s: %d trades\n", gemName, gemFast, gemSlow, label, from, to, len(ts))
	if !*replay && !*judge {
		if len(ts) < gemMinTrade {
			fmt.Printf("judge once at %d trades (%d to go); until then the result stays unread\n", gemMinTrade, gemMinTrade-len(ts))
		} else {
			fmt.Printf("enough trades: run once with --judge and check docs/strategies/goldm_ema_15m.md\n")
		}
		return nil
	}
	ts3, err := run(3)
	if err != nil {
		return err
	}
	s1, s3 := summarize(ts), summarize(ts3)
	var long, short float64
	for _, tr := range ts {
		if tr.Side == market.Buy {
			long += tr.Net.Rupees()
		} else {
			short += tr.Net.Rupees()
		}
	}
	fmt.Printf("1 tick: net ₹%.0f, PF %s, win %.0f%%, max DD ₹%.0f, long ₹%.0f, short ₹%.0f\n",
		s1.net, pf(s1.pf), s1.win*100, s1.dd, long, short)
	fmt.Printf("3 ticks: net ₹%.0f, PF %s\n", s3.net, pf(s3.pf))
	if *replay {
		return nil
	}
	pass := len(ts) >= gemMinTrade && s1.net > 0 && s1.pf >= 1.3 && s3.net > 0 && s1.dd <= 150000
	fmt.Printf("criteria (≥%d trades, net > 0, PF ≥ 1.3, net > 0 at 3 ticks, max DD ≤ ₹1.5L): %s\n",
		gemMinTrade, map[bool]string{true: "pass", false: "fail"}[pass])
	var net market.Paise
	for _, tr := range ts {
		net += tr.Net
	}
	return analytics.AppendTrials(analytics.LedgerPath(*dir), []analytics.TrialRecord{{
		Time: time.Now(), Command: "research mcxemafwd --judge", Strategy: "goldm_ema_15m", Underlying: gemName,
		Params: strategy.Params{"frame_min": 15, "fast": gemFast, "slow": gemSlow, "slip_ticks": 1},
		From:   from, To: to, Count: 1, Unsealed: true, Net: net, Trades: len(ts),
	}})
}
