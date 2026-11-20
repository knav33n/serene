package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/kite"
	"serene/internal/live"
	"serene/internal/market"
	"serene/internal/mcx"
	"serene/internal/slack"
)

// mcxLiveTags are the order-tag codes of the MCX strategies that may trade.
var mcxLiveTags = map[string]string{"goldm_hma_1h": "gh", "silverm_hma_1h": "sh"}

// runLiveMCX trades the frozen MCX HMA strategies with one lot each on
// Zerodha (NRML, held overnight). It runs each minute after forward signals
// has fetched the bars. A strategy's state is data/live/mcx-<strategy>.json;
// the first run writes it with since = now, and only a turn after that is
// traded, so live trading starts at the next HMA turn, not mid-trade.
func runLiveMCX(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("live mcx", flag.ContinueOnError)
	strategies := fs.String("strategies", "goldm_hma_1h,silverm_hma_1h", "strategies to trade")
	useSlack := fs.Bool("slack", false, "post fills to SLACK_TRADES_CHANNEL (SILVERM to SLACK_SILVERM_CHANNEL if set), halts to SLACK_ALERT_CHANNEL")
	dir := fs.String("data", "data/mcx", "data directory")
	costsFile := fs.String("costs", "configs/costs_mcx.yaml", "charges schedule")
	liveDir := fs.String("live-dir", "data/live", "live state; a STOP file here stops new entries")
	report := fs.String("report", "reports/live_mcx.csv", "closed live trades")
	rollDays := fs.Int("roll-days", 7, "hold the nearest contract expiring more than this many days ahead")
	buffer := fs.Float64("buffer", 0.003, "how far through the last close a LIMIT is priced")
	check := fs.Bool("check", false, "show each strategy's target, contract and position, and place nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var names []string
	for _, n := range strings.Split(*strategies, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if mcxLiveTags[n] == "" {
			return fmt.Errorf("%q can't trade live (only goldm_hma_1h, silverm_hma_1h)", n)
		}
		names = append(names, n)
	}
	var client *slack.Client
	var channel string
	if *useSlack {
		var err error
		if client, channel, err = openSlackChannel(ctx, "SLACK_TRADES_CHANNEL"); err != nil {
			return err
		}
	}
	route := func(strat string) string {
		if c := os.Getenv("SLACK_SILVERM_CHANNEL"); c != "" && mcxForwardUnderlying(strat) == smName {
			return c
		}
		return channel
	}
	kc, err := kiteClient(ctx)
	if err != nil {
		return err
	}
	ins, err := kc.Instruments(ctx, "MCX")
	if err != nil {
		return fmt.Errorf("kite MCX instruments: %w", err)
	}
	costs, err := cost.Load(*costsFile)
	if err != nil {
		return err
	}
	now := time.Now()
	cfg := live.MCXConfig{Lots: 1, Buffer: *buffer, MaxMods: 10, NoEntry: fileExists(filepath.Join(*liveDir, "STOP"))}
	var errs []error
	for _, strat := range names {
		err := liveMCXOne(ctx, client, route(strat), kc, ins, costs, *costsFile, strat, cfg, now, *dir, *liveDir, *report, *rollDays, *check)
		if !*check {
			alertOnce(ctx, client, channel, filepath.Join(*liveDir, "mcx-"+strat+"_alert.json"), "serene live "+strat+" on zerodha", err)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", strat, err))
		}
	}
	return errors.Join(errs...)
}

// mcxLiveState is a strategy's state file.
type mcxLiveState struct {
	Since time.Time `json:"since"` // only turns after this are traded
	live.MCXState
}

func liveMCXOne(ctx context.Context, client *slack.Client, channel string, kc *kite.Client, ins []kite.Instrument,
	costs *cost.Schedule, costsFile, strat string, cfg live.MCXConfig, now time.Time, dir, liveDir, report string, rollDays int, check bool) error {
	path := filepath.Join(liveDir, "mcx-"+strat+".json")
	var st mcxLiveState
	ok, err := readJSONFile(path, &st)
	if err != nil {
		return err
	}
	if !ok {
		st.Since, st.Strategy = now, strat
		if !check {
			if err := writeJSONFile(path, st); err != nil {
				return err
			}
		}
		fmt.Printf("%s: live from %s; trades start at the next turn\n", strat, now.In(market.IST).Format("2006-01-02 15:04"))
	}
	name := mcxForwardUnderlying(strat)
	spec := mcx.Specs[name]

	// The contracts Kite lists, nearest expiry first.
	var futs []kite.Instrument
	for _, i := range ins {
		if i.Name == name && i.Type == "FUT" && i.Exchange == "MCX" {
			futs = append(futs, i)
		}
	}
	sort.Slice(futs, func(a, b int) bool { return futs[a].Expiry.Before(futs[b].Expiry) })
	syms := map[string]string{}
	for _, f := range futs {
		if f.LotSize != 1 {
			return fmt.Errorf("kite lists %s with lot size %d; serene orders in lots and expects 1", f.Symbol, f.LotSize)
		}
		syms[market.Instrument{Exchange: market.MCX, Kind: market.KindFuture, Underlying: name, Expiry: f.Expiry}.Key()] = f.Symbol
	}
	today := market.DateOf(now.In(market.IST))
	cut := today.AddDays(rollDays)
	var hold string
	for _, f := range futs {
		if cut.Before(f.Expiry) {
			hold = market.Instrument{Exchange: market.MCX, Kind: market.KindFuture, Underlying: name, Expiry: f.Expiry}.Key()
			break
		}
	}
	if hold == "" {
		return fmt.Errorf("no %s future expires after %s", name, cut)
	}

	// The strategy's side now, if its current run began after go-live.
	ts, err := mcxTrades(ctx, dir, costsFile, strat)
	if err != nil {
		return err
	}
	var side market.Side
	runStart := time.Time{}
	if n := len(ts); n > 0 && ts[n-1].Reason == "end" {
		i := n - 1
		for i > 0 && ts[i-1].Reason == "roll" && ts[i-1].Side == ts[i].Side {
			i--
		}
		runStart = ts[i].Entry
		if !runStart.Before(st.Since) {
			side = ts[n-1].Side
		}
	}

	// Last closes, and whether the market is trading.
	store, err := data.Open(dir)
	if err != nil {
		return err
	}
	bars, err := loadMCXBars(ctx, store, []string{name}, today, today)
	store.Close()
	if err != nil {
		return err
	}
	last := map[string]market.Paise{}
	lastAt := map[string]time.Time{}
	for _, b := range bars[name] {
		if b.End.After(lastAt[b.Key]) {
			last[b.Key], lastAt[b.Key] = b.Close, b.End
		}
	}
	tgt := live.MCXTarget{Side: side, Key: hold, Last: last, Tick: spec.Tick}
	b := &live.Zerodha{C: kc, Symbols: syms, Exchange: "MCX", Product: "NRML"}

	if check {
		inst, _ := market.ParseKey(hold)
		pos, perr := b.Position(ctx, inst)
		fmt.Printf("%s: since %s, strategy side %s (run from %s), hold %s (%s) last ₹%s at %s\n  state %+v\n  Zerodha NRML %s: %d lot(s) %v\n",
			strat, st.Since.In(market.IST).Format("01-02 15:04"), sideWord(side), runStart.In(market.IST).Format("01-02 15:04"),
			hold, syms[hold], last[hold], lastAt[hold].In(market.IST).Format("01-02 15:04"), st.MCXState, syms[hold], pos, perr)
		return nil
	}
	if st.Halted != "" {
		fmt.Printf("%s: halted: %s\n", strat, st.Halted)
		return nil
	}
	// Act only on a fresh bar of the contract the next order is in: the
	// market is open, the fetch worked, and the order is priced off now.
	next := hold
	switch {
	case st.Order != nil:
		next = st.Order.Key
	case st.Qty != 0:
		next = st.Key
	}
	if now.Sub(lastAt[next]) > 5*time.Minute {
		fmt.Printf("%s: no %s bar since %s; waiting\n", strat, next, lastAt[next].In(market.IST).Format("01-02 15:04"))
		return nil
	}
	cfg.TagCode = mcxLiveTags[strat]
	evs, stepErr := live.StepMCX(ctx, b, &st.MCXState, tgt, cfg, now)
	if err := writeJSONFile(path, st); err != nil {
		return errors.Join(stepErr, err)
	}
	for _, e := range evs {
		text := e.Text
		if f := e.Fill; f != nil {
			text = mcxFillText(strat, syms[f.Key], spec, costs, f, report)
		} else if strings.Contains(text, "halted") {
			postAlert(ctx, client, channel, text)
		}
		fmt.Println(text)
		if client != nil {
			if err := client.Post(ctx, channel, text); err != nil {
				fmt.Fprintln(os.Stderr, "slack:", err)
			}
		}
	}
	if stepErr == nil {
		fmt.Printf("%s: strategy %s, holding %d lot(s) %s\n", strat, sideWord(side), st.Qty, st.Key)
	}
	return stepErr
}

func sideWord(s market.Side) string {
	switch s {
	case market.Buy:
		return "long"
	case market.Sell:
		return "short"
	}
	return "flat"
}

// mcxFillText is a fill's Slack line; a close carries its P&L after charges
// and is added to the report.
func mcxFillText(strat, sym string, spec mcx.Spec, costs *cost.Schedule, f *live.MCXFill, report string) string {
	icon := ":large_green_circle:"
	if f.Side == market.Sell {
		icon = ":red_circle:"
	}
	t := fmt.Sprintf("%s *LIVE %s* Zerodha %s %d lot %s @ ₹%s · %s · %s", icon, strat, f.Side, f.Qty, sym, f.Price,
		f.Time.In(market.IST).Format("2006-01-02 15:04"), f.Why)
	if !f.Close {
		return t
	}
	held := -f.Side
	gross := (f.Price - f.Entry) * market.Paise(held) * market.Paise(spec.Multiplier*f.Qty)
	inst, _ := market.ParseKey(f.Key)
	c1, err1 := costs.Charges(market.Fill{Instrument: inst, Side: held, Qty: spec.Multiplier * f.Qty, Price: f.Entry, Time: f.EntryTime, Kind: market.Traded})
	c2, err2 := costs.Charges(market.Fill{Instrument: inst, Side: f.Side, Qty: spec.Multiplier * f.Qty, Price: f.Price, Time: f.Time, Kind: market.Traded})
	if err := errors.Join(err1, err2); err != nil {
		return t + fmt.Sprintf(" · gross ₹%s (charges: %v)", gross, err)
	}
	net := gross - c1 - c2
	t += fmt.Sprintf(" · %s from ₹%s (%s) · net ₹%s after ₹%s charges", sideWord(held), f.Entry,
		f.EntryTime.In(market.IST).Format("01-02 15:04"), net, c1+c2)
	row := []string{strat, sym, sideWord(held), f.EntryTime.In(market.IST).Format(time.DateTime), f.Entry.String(),
		f.Time.In(market.IST).Format(time.DateTime), f.Price.String(), fmt.Sprint(f.Qty), f.Why, gross.String(), (c1 + c2).String(), net.String()}
	if err := appendCSVRow(report, mcxLiveHeader, row); err != nil {
		fmt.Fprintln(os.Stderr, "live report:", err)
	}
	return t
}

var mcxLiveHeader = []string{"strategy", "contract", "side", "entry_time", "entry", "exit_time", "exit", "lots", "reason",
	"gross", "charges", "net"}
