package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"serene/internal/analytics"
	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/indicator"
	"serene/internal/market"
	"serene/internal/mcx"
	"serene/internal/strategy"
)

// The two frozen GOLDM rules, unchanged, on SILVERM, frozen on 2026-11-11
// after `serene research mcxmetals`; see docs/strategies/silverm.md. They
// were not picked on silver, so silver's grid adds no selection to them.
// None of these may change during the forward test.
const (
	smName     = "SILVERM"
	smMinTrade = 100 // trades needed before each strategy's one judgement
)

var (
	smForward    = market.Date{Year: 2026, Month: time.November, Day: 12}
	smResearchLo = market.Date{Year: 2026, Month: time.July, Day: 1}
	smResearchHi = market.Date{Year: 2026, Month: time.November, Day: 10}
)

// smStrategies are the SILVERM forward tests by name.
var smStrategies = []string{"silverm_ema_15m", "silverm_hma_1h"}

// smConfig is a frozen SILVERM rule and its signal frame; forward signals use the same one.
func smConfig(strat string, spec mcx.Spec, costs *cost.Schedule, slip int64, from, to market.Date) (time.Duration, mcx.EMAConfig, error) {
	switch strat {
	case "silverm_ema_15m":
		return 15 * time.Minute, mcx.EMAConfig{Name: smName, Spec: spec, Frame: 15 * time.Minute, Fast: 13, Slow: 34,
			Slip: slip, Costs: costs, From: from, To: to}, nil
	case "silverm_hma_1h":
		return time.Hour, mcx.EMAConfig{Name: smName, Spec: spec, Frame: time.Hour, Fast: 21, Slope: true, MA: indicator.HMA,
			Slip: slip, Costs: costs, From: from, To: to}, nil
	}
	return 0, mcx.EMAConfig{}, fmt.Errorf("unknown SILVERM strategy %q", strat)
}

// runSilvermForward runs one frozen SILVERM rule on sessions from 2026-11-12.
// Until --judge it prints only how many trades there are, so the forward
// result isn't watched and stopped on a good day. --replay reruns the
// research window.
func runSilvermForward(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("silvermfwd", flag.ContinueOnError)
	strat := fs.String("strategy", "silverm_ema_15m", "silverm_ema_15m or silverm_hma_1h")
	toS := fs.String("to", "", "last session (default: the last one in the data)")
	judge := fs.Bool("judge", false, "show the result: do this once per strategy, at "+fmt.Sprint(smMinTrade)+"+ trades")
	replay := fs.Bool("replay", false, "rerun the research window "+smResearchLo.String()+" to "+smResearchHi.String())
	dir := fs.String("data", "data/mcx", "data directory")
	costsFile := fs.String("costs", "configs/costs_mcx.yaml", "charges schedule")
	if err := fs.Parse(args); err != nil {
		return err
	}
	spec, ok := mcx.Specs[smName]
	if !ok {
		return fmt.Errorf("no contract spec for %s", smName)
	}
	costs, err := cost.Load(*costsFile)
	if err != nil {
		return err
	}
	if _, _, err := smConfig(*strat, spec, costs, 1, smForward, smForward); err != nil {
		return err
	}
	store, err := data.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()

	from, to := smForward, market.Date{}
	if *replay {
		from, to = smResearchLo, smResearchHi
	} else if *toS != "" {
		if to, err = market.ParseDate(*toS); err != nil {
			return err
		}
	} else {
		days, err := store.Days(smName, from, from.AddDays(3650))
		if err != nil {
			return err
		}
		if len(days) == 0 {
			fmt.Printf("%s forward test: no sessions from %s fetched yet\n", *strat, from)
			return nil
		}
		to = days[len(days)-1]
	}
	if to.Before(from) {
		return fmt.Errorf("--to %s is before the forward start %s", to, from)
	}

	bars, err := loadMCXBars(ctx, store, []string{smName}, from.AddDays(-30), to)
	if err != nil {
		return err
	}
	frame, _, _ := smConfig(*strat, spec, costs, 1, from, to)
	ed := mcx.PrepareEMA(bars[smName], frame)
	run := func(slip int64) ([]mcx.Trade, error) {
		_, cfg, _ := smConfig(*strat, spec, costs, slip, from, to)
		return mcx.EMABacktest(ed, cfg)
	}
	ts, err := run(1)
	if err != nil {
		return err
	}
	label := "forward test"
	if *replay {
		label = "research replay"
	}
	fmt.Printf("%s long+short, %s %s to %s: %d trades\n", *strat, label, from, to, len(ts))
	if !*replay && !*judge {
		if len(ts) < smMinTrade {
			fmt.Printf("judge once at %d trades (%d to go); until then the result stays unread\n", smMinTrade, smMinTrade-len(ts))
		} else {
			fmt.Printf("enough trades: run once with --judge and check docs/strategies/silverm.md\n")
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
	pass := len(ts) >= smMinTrade && s1.net > 0 && s1.pf >= 1.3 && s3.net > 0 && s1.dd <= 150000
	fmt.Printf("criteria (≥%d trades, net > 0, PF ≥ 1.3, net > 0 at 3 ticks, max DD ≤ ₹1.5L): %s\n",
		smMinTrade, map[bool]string{true: "pass", false: "fail"}[pass])
	var net market.Paise
	for _, tr := range ts {
		net += tr.Net
	}
	return analytics.AppendTrials(analytics.LedgerPath(*dir), []analytics.TrialRecord{{
		Time: time.Now(), Command: "research silvermfwd --judge", Strategy: *strat, Underlying: smName,
		Params: strategy.Params{"slip_ticks": 1}, From: from, To: to, Count: 1, Unsealed: true, Net: net, Trades: len(ts),
	}})
}
