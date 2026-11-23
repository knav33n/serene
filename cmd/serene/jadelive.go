package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"serene/internal/cost"
	"serene/internal/kite"
	"serene/internal/live"
	"serene/internal/market"
	"serene/internal/slack"
)

// runLiveJade trades the NIFTY Jade Lizard with one lot on Zerodha (NRML):
// a ~200-point credit, 50% take profit, exit at 21 days, no stop. It runs at
// 15:20 IST (and again at 15:30 to finish anything left): the daily decision
// is taken once, then legs are placed one at a time and followed until they
// fill. State is data/live/jade.json.
func runLiveJade(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("live jade", flag.ContinueOnError)
	useSlack := fs.Bool("slack", false, "post entries, exits and the daily mark to SLACK_JADE_CHANNEL, halts to SLACK_ALERT_CHANNEL")
	check := fs.Bool("check", false, "show the state, today's pick (or the next expiry's), margin and positions; place nothing")
	liveDir := fs.String("live-dir", "data/live", "live state")
	report := fs.String("report", "reports/live_jade.csv", "closed live trades")
	costsFile := fs.String("costs", "configs/costs.yaml", "charges schedule")
	buffer := fs.Float64("buffer", 0.02, "how far through the touch a LIMIT is priced")
	decideAt := fs.String("decide-at", "15:20", "the daily decision runs from this IST time")
	runFor := fs.Duration("for", 8*time.Minute, "keep following orders for at most this long")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var client *slack.Client
	var channel string
	if *useSlack {
		var err error
		if client, channel, err = openSlackChannel(ctx, "SLACK_JADE_CHANNEL"); err != nil {
			return err
		}
	}
	err := liveJade(ctx, client, channel, *check, *liveDir, *report, *costsFile, *buffer, *decideAt, *runFor)
	if !*check {
		alertOnce(ctx, client, channel, filepath.Join(*liveDir, "jade_alert.json"), "serene live jade_lizard on zerodha", err)
	}
	return err
}

type jadeLiveState struct {
	live.JadeState
}

// jadeMarket is what Kite lists for NIFTY options and futures.
type jadeMarket struct {
	kc      *kite.Client
	syms    map[string]string // instrument key → Kite symbol
	futs    map[market.Date]string
	chains  map[market.Date][]kite.Instrument
	lot     int64
	tick    market.Paise
	expires []market.Date
}

func loadJadeMarket(ctx context.Context, kc *kite.Client) (*jadeMarket, error) {
	ins, err := kc.Instruments(ctx, "NFO")
	if err != nil {
		return nil, fmt.Errorf("kite NFO instruments: %w", err)
	}
	m := &jadeMarket{kc: kc, syms: map[string]string{}, futs: map[market.Date]string{}, chains: map[market.Date][]kite.Instrument{}}
	seen := map[market.Date]bool{}
	for _, i := range ins {
		if i.Name != "NIFTY" || i.Exchange != "NFO" {
			continue
		}
		switch i.Type {
		case "FUT":
			m.futs[i.Expiry] = i.Symbol
		case "CE", "PE":
			strike := int(math.Round(i.Strike))
			m.syms[live.JadeKey(i.Expiry, strike, i.Type == "CE")] = i.Symbol
			m.chains[i.Expiry] = append(m.chains[i.Expiry], i)
			if !seen[i.Expiry] {
				seen[i.Expiry] = true
				m.expires = append(m.expires, i.Expiry)
			}
			if m.lot == 0 {
				m.lot, m.tick = i.LotSize, market.Rupees(i.TickSize)
			}
		}
	}
	if m.lot <= 0 {
		return nil, errors.New("kite lists no NIFTY options")
	}
	sort.Slice(m.expires, func(a, b int) bool { return m.expires[a].Before(m.expires[b]) })
	return m, nil
}

func toJadeQuote(q kite.Quote, lot int64) live.JadeQuote {
	return live.JadeQuote{Last: q.Last, Bid: q.Bid(), Ask: q.Ask(), Contracts: q.Volume / max(lot, 1)}
}

// future is expiry exp's future quote, and whether it traded in the last 10 minutes today.
func (m *jadeMarket) future(ctx context.Context, exp market.Date, now time.Time) (float64, bool, error) {
	sym, ok := m.futs[exp]
	if !ok {
		return 0, false, fmt.Errorf("kite lists no NIFTY future for %s", exp)
	}
	qs, err := m.kc.Quotes(ctx, []string{"NFO:" + sym})
	if err != nil {
		return 0, false, err
	}
	q := qs["NFO:"+sym]
	fresh := market.DateOf(q.LastTradeTime.In(market.IST)) == market.DateOf(now.In(market.IST)) && now.Sub(q.LastTradeTime.Time) < 10*time.Minute
	return q.Last, fresh, nil
}

// chain quotes exp's 100-point strikes within 15% of fwd.
func (m *jadeMarket) chain(ctx context.Context, exp market.Date, fwd float64, now time.Time) (live.JadeChain, error) {
	ch := live.JadeChain{Expiry: exp, Fwd: fwd, Now: now, Quotes: map[live.JadeStrike]live.JadeQuote{}}
	var keys []string
	idx := map[string]live.JadeStrike{}
	for _, i := range m.chains[exp] {
		k := int(math.Round(i.Strike))
		if k%100 != 0 || math.Abs(float64(k)-fwd) > 0.15*fwd {
			continue
		}
		key := "NFO:" + i.Symbol
		keys = append(keys, key)
		idx[key] = live.JadeStrike{Strike: k, Call: i.Type == "CE"}
	}
	qs, err := m.kc.Quotes(ctx, keys)
	if err != nil {
		return ch, err
	}
	for k, q := range qs {
		ch.Quotes[idx[k]] = toJadeQuote(q, m.lot)
	}
	return ch, nil
}

// legQuotes quotes the position's legs.
func (m *jadeMarket) legQuotes(ctx context.Context, legs []live.JadeLeg) (map[string]live.JadeQuote, error) {
	out := map[string]live.JadeQuote{}
	if len(legs) == 0 {
		return out, nil
	}
	var keys []string
	for _, l := range legs {
		sym, ok := m.syms[l.Key]
		if !ok {
			return nil, fmt.Errorf("kite lists no %s", l.Key)
		}
		keys = append(keys, "NFO:"+sym)
	}
	qs, err := m.kc.Quotes(ctx, keys)
	if err != nil {
		return nil, err
	}
	for i, l := range legs {
		out[l.Key] = toJadeQuote(qs[keys[i]], m.lot)
	}
	return out, nil
}

// margin is what Kite would block for the pick, and the funds available.
func (m *jadeMarket) margin(ctx context.Context, p live.JadePick) (need, have float64, err error) {
	o := func(strike int, call bool, side string, px float64) kite.OrderRequest {
		return kite.OrderRequest{Exchange: "NFO", Symbol: m.syms[live.JadeKey(p.Expiry, strike, call)], Side: side, Type: "LIMIT",
			Product: "NRML", Qty: m.lot, Price: math.Round(px*20) / 20}
	}
	need, err = m.kc.BasketMargin(ctx, []kite.OrderRequest{
		o(p.Wing, true, "BUY", p.WingPx), o(p.Call, true, "SELL", p.CallPx), o(p.Put, false, "SELL", p.PutPx)})
	if err != nil {
		return 0, 0, fmt.Errorf("basket margin: %w", err)
	}
	have, err = m.kc.EquityFunds(ctx)
	return need, have, err
}

func liveJade(ctx context.Context, client *slack.Client, channel string, check bool, liveDir, report, costsFile string,
	buffer float64, decideAt string, runFor time.Duration) error {
	at, err := time.Parse("15:04", decideAt)
	if err != nil {
		return fmt.Errorf("--decide-at: %w", err)
	}
	costs, err := cost.Load(costsFile)
	if err != nil {
		return err
	}
	path := filepath.Join(liveDir, "jade.json")
	var st jadeLiveState
	ok, err := readJSONFile(path, &st)
	if err != nil {
		return err
	}
	now := time.Now()
	if !ok {
		st.Since = now
		if !check {
			if err := writeJSONFile(path, st); err != nil {
				return err
			}
		}
		fmt.Printf("jade_lizard: live from %s\n", now.In(market.IST).Format("2006-01-02 15:04"))
	}
	ist := now.In(market.IST)
	today := market.DateOf(ist)
	pastDecide := ist.Hour()*60+ist.Minute() >= at.Hour()*60+at.Minute()
	if !check && !pastDecide && !st.Busy() {
		fmt.Printf("jade_lizard: before %s, nothing to do\n", decideAt)
		return nil
	}
	kc, err := kiteClient(ctx)
	if err != nil {
		return err
	}
	m, err := loadJadeMarket(ctx, kc)
	if err != nil {
		return err
	}
	rules := live.DefaultJade
	monthlies := live.JadeMonthlies(m.expires)
	broker := &live.Zerodha{C: kc, Symbols: m.syms, Exchange: "NFO", Product: "NRML"}
	cfg := live.JadeConfig{Rules: rules, Buffer: buffer, MaxMods: 10}

	// The market is open if the front monthly future traded in the last 10 minutes.
	var front market.Date
	for _, e := range monthlies {
		if !e.Before(today) {
			front = e
			break
		}
	}
	_, open, err := m.future(ctx, front, now)
	if err != nil {
		return err
	}

	// Today's entry, when flat on an entry day.
	var pick *live.JadePick
	marginOK := false
	entryExp, entryDay := rules.EntryExpiry(today, monthlies)
	previewExp := entryExp
	if !entryDay && check {
		for _, e := range monthlies {
			if live.DTE(today, e) >= rules.LastEntryDTE-15 {
				previewExp = e
				break
			}
		}
	}
	if (entryDay && st.Phase == live.JadeFlat && !st.DoneFor(entryExp) && open && pastDecide) || (check && !previewExp.IsZero()) {
		fwd, _, err := m.future(ctx, previewExp, now)
		if err != nil {
			return err
		}
		ch, err := m.chain(ctx, previewExp, fwd, now)
		if err != nil {
			return err
		}
		pr := rules
		label := "today's entry"
		if check && !open {
			pr.MinContracts = 0 // no volume while closed: liquidity isn't checked
			label = "market closed, liquidity not checked; " + label
		}
		p, skip := pr.PickJade(ch)
		if !entryDay {
			label = strings.TrimSuffix(label, "today's entry") + fmt.Sprintf("preview only, %s is %d days out (entries %d-%d)", previewExp, live.DTE(today, previewExp), rules.EntryDTE, rules.LastEntryDTE)
		}
		switch {
		case skip != "":
			fmt.Printf("jade_lizard %s: no entry: %s\n", label, skip)
			if entryDay && !check {
				st.Done = append(st.Done, entryExp.String())
				post(ctx, client, channel, fmt.Sprintf(":warning: *jade_lizard* skipped the %s expiry: %s", entryExp, skip))
			}
		default:
			need, have, err := m.margin(ctx, p)
			if err != nil {
				return err
			}
			marginOK = need*1.10 <= have
			fmt.Printf("jade_lizard %s: %s fwd %.1f  -P%d @%.2f (Δ%.2f)  -C%d @%.2f (Δ%.2f)  +C%d @%.2f  credit %.2f/unit = ₹%.0f/lot\n"+
				"  margin ₹%.0f, funds ₹%.0f, ok (with 10%% spare) %v\n",
				label, p.Expiry, fwd, p.Put, p.PutPx, p.PutDelta, p.Call, p.CallPx, p.CallDelta, p.Wing, p.WingPx,
				p.Credit, p.Credit*float64(m.lot), need, have, marginOK)
			if entryDay {
				pick = &p
			}
		}
	}

	if check {
		fmt.Printf("state: phase %q expiry %s legs %d credit %.2f done %v halted %q since %s\n", st.Phase, st.Expiry, len(st.Legs),
			st.Credit(), st.Done, st.Halted, st.Since.In(market.IST).Format("2006-01-02 15:04"))
		fmt.Printf("market open %v, lot %d, monthlies %v\n", open, m.lot, monthlies[:min(4, len(monthlies))])
		for _, l := range st.Legs {
			inst, _ := market.ParseKey(l.Key)
			pos, perr := broker.Position(ctx, inst)
			fmt.Printf("  %s %s: serene %d, Zerodha NRML %d %v\n", l.Key, m.syms[l.Key], int64(l.Sign)*(l.EntryFilled-l.ExitFilled), pos, perr)
		}
		return nil
	}
	if st.Halted != "" {
		fmt.Println("jade_lizard: halted:", st.Halted)
		return writeJSONFile(path, st)
	}
	if !open {
		fmt.Println("jade_lizard: the market isn't trading; waiting")
		return writeJSONFile(path, st)
	}

	deadline := now.Add(runFor)
	var stepErr error
	for {
		now = time.Now()
		var legs []live.JadeLeg
		legs = append(legs, st.Legs...)
		if st.Phase == live.JadeFlat && pick != nil {
			legs = []live.JadeLeg{
				{Key: live.JadeKey(pick.Expiry, pick.Wing, true)},
				{Key: live.JadeKey(pick.Expiry, pick.Call, true)},
				{Key: live.JadeKey(pick.Expiry, pick.Put, false)}}
		}
		quotes, err := m.legQuotes(ctx, legs)
		if err != nil {
			stepErr = err
			break
		}
		snap := live.JadeSnap{Now: now, Decide: pastDecide, Entry: pick, MarginOK: marginOK, Quotes: quotes, Lot: m.lot, Tick: m.tick}
		wasPhase := st.Phase
		evs, err := live.StepJade(ctx, broker, &st.JadeState, snap, cfg)
		if werr := writeJSONFile(path, st); werr != nil {
			return errors.Join(err, werr)
		}
		for _, e := range evs {
			jadeReport(ctx, client, channel, &st.JadeState, e, m, costs, report, wasPhase)
		}
		if err != nil {
			stepErr = err
			break
		}
		if !st.Busy() || time.Now().After(deadline) {
			break
		}
		if err := sleep(ctx, 3*time.Second); err != nil {
			return err
		}
	}
	if stepErr == nil && st.Busy() {
		stepErr = fmt.Errorf("still %s after %s; the next run continues", st.Phase, runFor)
	}
	fmt.Printf("jade_lizard: phase %q, expiry %s, credit %.2f\n", st.Phase, st.Expiry, st.Credit())
	return stepErr
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func post(ctx context.Context, client *slack.Client, channel, text string) {
	fmt.Println(text)
	if client == nil {
		return
	}
	if err := client.Post(ctx, channel, text); err != nil {
		fmt.Fprintln(os.Stderr, "slack:", err)
	}
}

func jadeLegName(l live.JadeLeg) string {
	cp := "P"
	if l.Call {
		cp = "C"
	}
	sign := "-"
	if l.Sign > 0 {
		sign = "+"
	}
	return fmt.Sprintf("%s%s%d", sign, cp, l.Strike)
}

// jadeReport posts an event: an entry with its fills, the daily mark, an exit
// with P&L after charges (also written to the report), a note or a halt.
func jadeReport(ctx context.Context, client *slack.Client, channel string, st *live.JadeState, e live.JadeEvent,
	m *jadeMarket, costs *cost.Schedule, report, wasPhase string) {
	lot := float64(m.lot)
	switch e.Kind {
	case "fill":
		fmt.Println("fill:", e.Text)
	case "entered":
		var parts []string
		for i := len(st.Legs) - 1; i >= 0; i-- {
			l := st.Legs[i]
			parts = append(parts, fmt.Sprintf("%s @ ₹%s", jadeLegName(l), l.Entry))
		}
		c := st.Credit()
		exitBy := st.Expiry.AddDays(-live.DefaultJade.ExitDTE)
		post(ctx, client, channel, fmt.Sprintf(":lizard: *LIVE jade_lizard* entered NIFTY %s, 1 lot (%d): %s · credit ₹%.2f/unit = ₹%.0f · take profit at ₹%.0f · time exit by %s",
			st.Expiry.At(12, 0).Format("02 Jan 2006"), m.lot, strings.Join(parts, ", "), c, c*lot, live.DefaultJade.TP*c*lot, exitBy.At(12, 0).Format("02 Jan")))
	case "mark":
		var pnl float64
		var dte int
		fmt.Sscanf(e.Text, "%f %d", &pnl, &dte)
		c := st.Credit()
		what := "holding"
		switch {
		case st.Phase == live.JadeExiting && st.Reason == "tp":
			what = "take profit hit, closing"
		case st.Phase == live.JadeExiting:
			what = fmt.Sprintf("%d days left, closing", dte)
		}
		post(ctx, client, channel, fmt.Sprintf(":lizard: jade_lizard NIFTY %s: open P&L ₹%.0f (%.0f%% of the ₹%.0f credit), %d days to expiry · %s",
			st.Expiry.At(12, 0).Format("02 Jan"), pnl*lot, 100*pnl/c, c*lot, dte, what))
	case "exited":
		var gross, charges market.Paise
		var parts []string
		for _, l := range st.Legs {
			gross += market.Paise(float64(l.Sign) * float64(l.Exit-l.Entry) * float64(l.ExitFilled))
			r := market.Put
			if l.Call {
				r = market.Call
			}
			inst := market.Option(market.NFO, "NIFTY", st.Expiry, market.Rupees(float64(l.Strike)), r, m.lot)
			side := market.Side(l.Sign)
			c1, _ := costs.Charges(market.Fill{Instrument: inst, Side: side, Qty: l.EntryFilled, Price: l.Entry, Time: l.EntryTime, Kind: market.Traded})
			c2, _ := costs.Charges(market.Fill{Instrument: inst, Side: -side, Qty: l.ExitFilled, Price: l.Exit, Time: l.ExitTime, Kind: market.Traded})
			charges += c1 + c2
		}
		for i := len(st.Legs) - 1; i >= 0; i-- {
			l := st.Legs[i]
			parts = append(parts, fmt.Sprintf("%s ₹%s→₹%s", jadeLegName(l), l.Entry, l.Exit))
		}
		net := gross - charges
		reason := map[string]string{"tp": "take profit", "time": "21 days to expiry"}[st.Reason]
		post(ctx, client, channel, fmt.Sprintf(":checkered_flag: *LIVE jade_lizard* closed NIFTY %s (%s): %s · net ₹%s after ₹%s charges",
			st.Expiry.At(12, 0).Format("02 Jan 2006"), reason, strings.Join(parts, ", "), net, charges))
		entered := st.Legs[0].EntryTime
		row := []string{st.Expiry.String(), entered.In(market.IST).Format(time.DateTime), st.Legs[len(st.Legs)-1].ExitTime.In(market.IST).Format(time.DateTime),
			strings.Join(parts, " "), fmt.Sprintf("%.2f", st.Credit()), st.Reason, gross.String(), charges.String(), net.String()}
		if err := appendCSVRow(report, []string{"expiry", "entry_time", "exit_time", "legs", "credit", "reason", "gross", "charges", "net"}, row); err != nil {
			fmt.Fprintln(os.Stderr, "live report:", err)
		}
	case "halt":
		post(ctx, client, channel, e.Text)
		if client != nil {
			postAlert(ctx, client, channel, e.Text)
		}
	default:
		post(ctx, client, channel, e.Text)
	}
	_ = wasPhase
}
