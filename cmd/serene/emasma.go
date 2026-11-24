package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/indicator"
	"serene/internal/market"
	"serene/internal/mcx"
)

// The 30-minute EMA-13 / SMA-34 crossover, long and short, on GOLDM and
// SILVERM: a silent forward test. It was picked on 2026-11-24 from `serene
// research mcxma10` on Fyers history 2019-2026 (reports/mcxma10_fyers/), as
// the only rule in the top 10 of both metals by the worse period's net / MTM
// drawdown at 3 ticks. Signals only, no orders and no Slack until both
// strategies have esJudgeAt closed trades; then one message with the verdict.
// See docs/strategies/emasma_30m.md. None of these may change: a change is a
// new strategy.
const (
	esFrame   = 30 * time.Minute
	esFast    = 13 // EMA of closes
	esSlow    = 34 // SMA of closes
	esLead    = 20 // days replayed before esForward, so the side held at the start comes from a real cross
	esJudgeAt = 100
)

// esForward is the first day whose entries count.
var esForward = market.Date{Year: 2026, Month: time.November, Day: 25}

// esStrategies maps each strategy to its commodity and its pass bar on
// mark-to-market drawdown: 1.5x the worst backtest MTM drawdown at 1 tick
// (GOLDM ₹2,49,921, SILVERM ₹3,74,544, both in 2024-26).
var esStrategies = []struct {
	name, metal string
	maxDD       float64
}{
	{"goldm_emasma_30m", gemName, 1.5 * 249921},
	{"silverm_emasma_30m", smName, 1.5 * 374544},
}

func esConfig(name string, spec mcx.Spec, costs *cost.Schedule, slip int64, from, to market.Date) mcx.EMAConfig {
	return mcx.EMAConfig{Name: name, Spec: spec, Frame: esFrame, Fast: esFast, Slow: esSlow,
		MA: indicator.EMA, SlowMA: indicator.SMA, Slip: slip, Costs: costs, From: from, To: to}
}

// esResult is one strategy's forward record so far.
type esResult struct {
	name           string
	trades, won    int
	net1, net3     float64
	gw, gl         float64
	mtm            ddStats
	open           string // the position held now, if any
	maxDD          float64
	first, through market.Date
}

func (r esResult) pf() float64 {
	if r.gl == 0 {
		return math.Inf(1)
	}
	return r.gw / r.gl
}

// pass is the verdict agreed before the test began: net above zero at 1 and
// 3 ticks, PF at least 1.1, MTM drawdown within 1.5x the backtest's.
func (r esResult) pass() bool {
	return r.net1 > 0 && r.net3 > 0 && r.pf() >= 1.1 && r.mtm.depth <= r.maxDD
}

// runForwardEMASMA replays both strategies from esForward and, with --slack,
// posts the judgement once both reach esJudgeAt closed trades. It posts
// nothing else: no signals, no progress, no failures.
func runForwardEMASMA(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("forward emasma", flag.ContinueOnError)
	fetch := fs.Bool("fetch", false, "fetch GOLDM and SILVERM minute bars from Kite first")
	useSlack := fs.Bool("slack", false, "post the judgement, once, to SLACK_ALERT_CHANNEL (else SLACK_TRADES_CHANNEL) when both reach the trade count")
	dir := fs.String("mcx-data", "data/mcx", "MCX data directory")
	costsFile := fs.String("mcx-costs", "configs/costs_mcx.yaml", "MCX charges schedule")
	marker := fs.String("judged", "data/forward_emasma_judged.json", "written once the judgement is posted")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *fetch {
		if err := fetchMCX(ctx, *dir, []string{gemName, smName}); err != nil {
			return fmt.Errorf("fetch: %w", err)
		}
	}
	costs, err := cost.Load(*costsFile)
	if err != nil {
		return err
	}
	var rs []esResult
	for _, s := range esStrategies {
		r, err := esReplay(ctx, *dir, costs, s.name, s.metal)
		if err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
		r.maxDD = s.maxDD
		rs = append(rs, r)
	}
	ready := true
	for _, r := range rs {
		fmt.Printf("%s: %d/%d closed trades from %s through %s; %s\n", r.name, r.trades, esJudgeAt, esForward, r.through, r.open)
		ready = ready && r.trades >= esJudgeAt
	}
	if !ready {
		return nil // results stay unread until the judgement
	}
	text := esVerdict(rs)
	fmt.Println(text)
	if !*useSlack {
		return nil
	}
	if fileExists(*marker) {
		return nil
	}
	client, channel, err := openSlackChannel(ctx, "SLACK_TRADES_CHANNEL")
	if err != nil {
		return err
	}
	if target := os.Getenv("SLACK_ALERT_CHANNEL"); target != "" {
		if channel, err = client.Resolve(ctx, target); err != nil {
			return err
		}
	}
	if err := client.Post(ctx, channel, text); err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]string{"posted": time.Now().Format(time.RFC3339)})
	return os.WriteFile(*marker, append(b, '\n'), 0o644)
}

// esReplay replays one strategy at 1 and 3 ticks and keeps the trades
// entered on or after esForward.
func esReplay(ctx context.Context, dir string, costs *cost.Schedule, strat, name string) (esResult, error) {
	r := esResult{name: strat}
	spec, ok := mcx.Specs[name]
	if !ok {
		return r, fmt.Errorf("no contract spec for %s", name)
	}
	store, err := data.Open(dir)
	if err != nil {
		return r, err
	}
	defer store.Close()
	start := esForward.AddDays(-esLead)
	days, err := store.Days(name, start, start.AddDays(3650))
	if err != nil {
		return r, err
	}
	if len(days) == 0 {
		return r, errors.New("no bars")
	}
	to := days[len(days)-1]
	r.through = to
	bars, err := loadMCXBars(ctx, store, []string{name}, start.AddDays(-30), to)
	if err != nil {
		return r, err
	}
	ed := mcx.PrepareEMA(bars[name], esFrame)
	from := esForward.At(0, 0)
	keep := func(ts []mcx.Trade) (closed []mcx.Trade, open *mcx.Trade) {
		for i, t := range ts {
			if t.Entry.Before(from) {
				continue
			}
			if t.Reason == "end" {
				open = &ts[i]
				continue
			}
			closed = append(closed, t)
		}
		return closed, open
	}
	t1, err := mcx.EMABacktest(ed, esConfig(strat, spec, costs, 1, start, to))
	if err != nil {
		return r, err
	}
	t3, err := mcx.EMABacktest(ed, esConfig(strat, spec, costs, 3, start, to))
	if err != nil {
		return r, err
	}
	c1, open := keep(t1)
	c3, _ := keep(t3)
	r.trades = len(c1)
	r.open = "flat"
	if open != nil {
		r.open = fmt.Sprintf("holding %s since %s", sideName(open.Side), open.Entry.In(market.IST).Format("2006-01-02 15:04"))
	}
	for _, t := range c1 {
		n := t.Net.Rupees()
		r.net1 += n
		if n > 0 {
			r.won++
			r.gw += n
		} else {
			r.gl -= n
		}
	}
	for _, t := range c3 {
		r.net3 += t.Net.Rupees()
	}
	r.mtm, _ = drawdown(bars[name], c1, float64(spec.Multiplier))
	return r, nil
}

func esVerdict(rs []esResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*30m EMA13/SMA34 forward test: %d trades reached* (signals only, from %s)\n", esJudgeAt, esForward)
	for _, r := range rs {
		verdict := ":x: fail"
		if r.pass() {
			verdict = ":white_check_mark: pass"
		}
		fmt.Fprintf(&b, "%s *%s* through %s: %d trades, %d won (%.0f%%), net ₹%.0f at 1 tick, ₹%.0f at 3 ticks, PF %s, "+
			"MTM DD ₹%.0f (bar ₹%.0f), longest under water %d days\n",
			verdict, r.name, r.through, r.trades, r.won, 100*float64(r.won)/float64(max(r.trades, 1)), r.net1, r.net3, pf(r.pf()),
			r.mtm.depth, r.maxDD, int(r.mtm.longest.Hours()/24+0.5))
	}
	b.WriteString("Pass = net > 0 at 1 and 3 ticks, PF ≥ 1.1, MTM DD ≤ 1.5× backtest. GOLDM and SILVERM are correlated (0.76): two passes are not two independent confirmations.")
	return b.String()
}
