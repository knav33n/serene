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

// runLiveSTBT trades the two 1DTE STBT straddle cells (nifty_stbt_1dte,
// sensex_stbt_1dte) with one lot each on Zerodha, NRML. It runs every minute
// 09:15–15:59 IST; each run is one pass per cell (repeated every few seconds
// while an order is being chased). Everything goes to SLACK_STBT_CHANNEL.
// State is data/live/stbt-<index>.json; closed straddles go to
// reports/live_stbt.csv.
func runLiveSTBT(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("live stbt", flag.ContinueOnError)
	useSlack := fs.Bool("slack", false, "post everything to SLACK_STBT_CHANNEL")
	check := fs.Bool("check", false, "show state, the next entry and today's pick with margin; place nothing")
	only := fs.String("only", "", "nifty or sensex")
	liveDir := fs.String("live-dir", "data/live", "live state")
	report := fs.String("report", "reports/live_stbt.csv", "closed live straddles")
	costsFile := fs.String("costs", "configs/costs.yaml", "charges schedule")
	runFor := fs.Duration("for", 50*time.Second, "keep chasing working orders for at most this long")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var client *slack.Client
	var channel string
	if *useSlack {
		var err error
		if client, channel, err = openSlackChannel(ctx, "SLACK_STBT_CHANNEL"); err != nil {
			return err
		}
	}
	costs, err := cost.Load(*costsFile)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range []live.STBTRules{live.NiftySTBT, live.SensexSTBT} {
		if *only != "" && !strings.EqualFold(*only, r.Index) {
			continue
		}
		err := liveSTBT(ctx, client, channel, r, *check, *liveDir, *report, costs, *runFor)
		if !*check {
			stbtAlertOnce(ctx, client, channel, filepath.Join(*liveDir, "stbt-"+strings.ToLower(r.Index)+"_alert.json"), r.Name, err)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Name, err))
		}
	}
	return errors.Join(errs...)
}

// stbtAlertOnce posts a failure once and its recovery once, to the STBT
// channel (the user wants every detail there).
func stbtAlertOnce(ctx context.Context, client *slack.Client, channel, path, what string, err error) {
	if client == nil {
		return
	}
	failing := fileExists(path)
	switch {
	case err != nil && !failing:
		post(ctx, client, channel, fmt.Sprintf(":x: *%s failed:* %v\nRetrying every minute; posted once until it recovers.", what, err))
		_ = os.WriteFile(path, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644)
	case err == nil && failing:
		post(ctx, client, channel, fmt.Sprintf(":white_check_mark: *%s recovered*", what))
		_ = os.Remove(path)
	}
}

type stbtState struct {
	live.STBTState
	MarkDay string // the day the 15:41 holding mark was posted
}

// stbtKite is the Zerodha account and market data for the engine.
type stbtKite struct {
	kc   *kite.Client
	exch string
}

func (k *stbtKite) Place(ctx context.Context, o kite.OrderRequest) (string, error) {
	return k.kc.PlaceOrder(ctx, o)
}
func (k *stbtKite) Modify(ctx context.Context, id string, o kite.OrderRequest) error {
	return k.kc.ModifyOrder(ctx, id, o)
}
func (k *stbtKite) Cancel(ctx context.Context, id string) error { return k.kc.CancelOrder(ctx, id) }
func (k *stbtKite) Order(ctx context.Context, id string) (kite.Order, error) {
	return k.kc.OrderStatus(ctx, id)
}
func (k *stbtKite) Orders(ctx context.Context) ([]kite.Order, error) { return k.kc.Orders(ctx) }
func (k *stbtKite) Position(ctx context.Context, sym string) (int64, error) {
	ps, err := k.kc.Positions(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, p := range ps {
		if p.Exchange == k.exch && p.Symbol == sym && p.Product == "NRML" {
			n += p.Qty
		}
	}
	return n, nil
}

func (k *stbtKite) LTP(ctx context.Context, syms []string) (map[string]float64, error) {
	var keys []string
	for _, s := range syms {
		keys = append(keys, k.exch+":"+s)
	}
	qs, err := k.kc.Quotes(ctx, keys)
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, s := range syms {
		out[s] = qs[k.exch+":"+s].Last
	}
	return out, nil
}

func (k *stbtKite) LowSince(ctx context.Context, token int64, from time.Time) (float64, bool, error) {
	start := from.Truncate(time.Minute).Add(time.Minute)
	now := time.Now()
	if !now.After(start) {
		return 0, false, nil
	}
	cs, err := k.kc.Candles(ctx, token, "minute", start, now, false)
	if err != nil {
		return 0, false, fmt.Errorf("candles: %w", err)
	}
	low, ok := 0.0, false
	for _, c := range cs {
		if c.Time.Before(start) || c.Low <= 0 {
			continue
		}
		// only closed minutes: the forming one may still print a lower low, which the next pass sees
		if !c.Time.Add(time.Minute).After(now) && (!ok || c.Low < low) {
			low, ok = c.Low, true
		}
	}
	return low, ok, nil
}

// stbtChain is the index's options from Kite's instrument list.
type stbtChain struct {
	byKey    map[string]kite.Instrument // expiry|strike|CE
	expiries []market.Date
	lot      int64
}

func loadSTBTChain(ctx context.Context, kc *kite.Client, r live.STBTRules) (*stbtChain, error) {
	ins, err := kc.Instruments(ctx, r.Exchange)
	if err != nil {
		return nil, fmt.Errorf("kite %s instruments: %w", r.Exchange, err)
	}
	ch := &stbtChain{byKey: map[string]kite.Instrument{}}
	seen := map[market.Date]bool{}
	for _, i := range ins {
		if i.Name != r.Index || (i.Type != "CE" && i.Type != "PE") {
			continue
		}
		ch.byKey[fmt.Sprintf("%s|%.0f|%s", i.Expiry, i.Strike, i.Type)] = i
		if !seen[i.Expiry] {
			seen[i.Expiry] = true
			ch.expiries = append(ch.expiries, i.Expiry)
		}
		ch.lot = i.LotSize
	}
	if len(ch.expiries) == 0 {
		return nil, fmt.Errorf("kite lists no %s options", r.Index)
	}
	sort.Slice(ch.expiries, func(a, b int) bool { return ch.expiries[a].Before(ch.expiries[b]) })
	return ch, nil
}

func (ch *stbtChain) nearest(today market.Date) market.Date {
	for _, e := range ch.expiries {
		if !e.Before(today) {
			return e
		}
	}
	return market.Date{}
}

var stbtIndexKey = map[string]string{"NIFTY": "NSE:NIFTY 50", "SENSEX": "BSE:SENSEX"}

// stbtPick is today's ATM straddle at the entry time, with a margin check.
func stbtPick(ctx context.Context, kc *kite.Client, r live.STBTRules, ch *stbtChain, exp market.Date) (*live.STBTPick, error) {
	qs, err := kc.Quotes(ctx, []string{stbtIndexKey[r.Index]})
	if err != nil {
		return nil, err
	}
	spot := qs[stbtIndexKey[r.Index]].Last
	if spot <= 0 {
		return nil, fmt.Errorf("no %s price", r.Index)
	}
	atm := math.Round(spot/r.Step) * r.Step
	p := &live.STBTPick{Expiry: exp, Spot: spot}
	var keys []string
	for _, right := range []string{"CE", "PE"} {
		i, ok := ch.byKey[fmt.Sprintf("%s|%.0f|%s", exp, atm, right)]
		if !ok {
			p.Skip = fmt.Sprintf("Kite lists no %s %.0f %s for %s", r.Index, atm, right, exp)
			return p, nil
		}
		p.Lot = i.LotSize
		p.Legs = append(p.Legs, live.STBTLeg{Right: right, Symbol: i.Symbol, Token: i.Token, Strike: atm})
		keys = append(keys, r.Exchange+":"+i.Symbol)
	}
	oq, err := kc.Quotes(ctx, keys)
	if err != nil {
		return nil, err
	}
	var orders []kite.OrderRequest
	for j := range p.Legs {
		px := oq[keys[j]].Last
		if px <= 0 {
			p.Skip = fmt.Sprintf("no price for %s", p.Legs[j].Symbol)
			return p, nil
		}
		p.Legs[j].Ref = px
		orders = append(orders, kite.OrderRequest{Exchange: r.Exchange, Symbol: p.Legs[j].Symbol, Side: "SELL", Type: "LIMIT",
			Product: "NRML", Qty: p.Lot, Price: math.Round(px*20) / 20})
	}
	need, err := kc.BasketMargin(ctx, orders)
	if err != nil {
		return nil, fmt.Errorf("basket margin: %w", err)
	}
	have, err := kc.EquityFunds(ctx)
	if err != nil {
		return nil, err
	}
	p.Margin, p.Funds = need, have
	if need*1.10 > have {
		p.Skip = fmt.Sprintf("margin ₹%.0f (+10%% spare) is more than the ₹%.0f available", need, have)
	}
	return p, nil
}

func liveSTBT(ctx context.Context, client *slack.Client, channel string, r live.STBTRules, check bool, liveDir, report string,
	costs *cost.Schedule, runFor time.Duration) error {
	path := filepath.Join(liveDir, "stbt-"+strings.ToLower(r.Index)+".json")
	var st stbtState
	if _, err := readJSONFile(path, &st); err != nil {
		return err
	}
	st.Rules = r.Name
	now := time.Now()
	ist := now.In(market.IST)
	today := market.DateOf(ist)
	m := ist.Hour()*60 + ist.Minute()
	inEntry := m >= hmMin(r.Entry) && m < hmMin(r.Entry)+30
	if !check {
		if st.Halted != "" || !market.NSETradingDay(today) || m < 9*60+15 {
			return nil
		}
		if !st.Active && !inEntry {
			return nil // nothing working and not the entry window: no Kite calls
		}
	}
	kc, err := kiteClient(ctx)
	if err != nil {
		return err
	}
	k := &stbtKite{kc: kc, exch: r.Exchange}
	var pick *live.STBTPick
	if !st.Active && (inEntry || check) {
		ch, err := loadSTBTChain(ctx, kc, r)
		if err != nil {
			return err
		}
		exp := ch.nearest(today)
		if check {
			next := today
			for i := 0; i < 14 && !live.EntryDay(next, ch.nearest(next)); i++ {
				next = next.AddDays(1)
			}
			fmt.Printf("%s: nearest expiry %s, entry day today %v, next entry day %s at %s; lot %d\n",
				r.Name, exp, live.EntryDay(today, exp), next, r.Entry, ch.lot)
			if p, err := stbtPick(ctx, kc, r, ch, exp); err != nil {
				fmt.Printf("  pick: %v\n", err)
			} else {
				for _, l := range p.Legs {
					fmt.Printf("  now: %s %s at ₹%.2f → W&T sell at ₹%.2f\n", l.Symbol, l.Right, l.Ref, l.Ref*(1+r.Wait/100))
				}
				fmt.Printf("  spot %.2f, margin ₹%.0f, funds ₹%.0f, skip %q\n", p.Spot, p.Margin, p.Funds, p.Skip)
			}
		} else if r.WantsPick(&st.STBTState, now, exp) {
			if pick, err = stbtPick(ctx, kc, r, ch, exp); err != nil {
				return err
			}
		}
	}
	if check {
		fmt.Printf("  state: active %v entry %s expiry %s halted %q done %v\n", st.Active, st.EntryDay, st.Expiry, st.Halted, st.Done)
		for _, l := range st.Legs {
			held, _ := k.Position(ctx, l.Symbol)
			fmt.Printf("  %s %s state %s entry %.2f stop %.2f trig %.2f reused %d order %s/%s (%s) · Zerodha NRML %d\n",
				l.Symbol, l.Right, l.State, l.Entry, l.Stop, l.Trig, l.ReUsed, l.Order, l.OrderTag, l.Kind, held)
		}
		return nil
	}

	deadline := now.Add(runFor)
	cfg := live.DefaultSTBTConfig
	for {
		evs, err := live.StepSTBT(ctx, k, k, &st.STBTState, r, cfg, time.Now(), pick)
		pick = nil
		if werr := writeJSONFile(path, st); werr != nil {
			return errors.Join(err, werr)
		}
		for _, e := range evs {
			stbtReport(ctx, client, channel, r, &st.STBTState, e, costs, report)
		}
		if err != nil {
			return err
		}
		if !st.Busy() || time.Now().After(deadline) {
			break
		}
		if err := sleep(ctx, 3*time.Second); err != nil {
			return err
		}
	}
	// the overnight holding, once after the close on the entry day
	if st.Active && m >= 15*60+41 && st.MarkDay != today.String() {
		st.MarkDay = today.String()
		stbtMark(ctx, client, channel, r, k, &st.STBTState)
		return writeJSONFile(path, st)
	}
	return nil
}

func hmMin(s string) int {
	t, _ := time.Parse("15:04", s)
	return t.Hour()*60 + t.Minute()
}

func stbtTitle(r live.STBTRules) string {
	return fmt.Sprintf("*LIVE %s* (%s 1DTE ATM straddle, entry %s → exit %s next day, W&T %g%%, SL %g%%, trail %g/%g, %d re-entr%s)",
		r.Name, r.Index, r.Entry, r.Exit, r.Wait, r.SL, r.TrailX, r.TrailY, r.Reentry, map[bool]string{true: "y", false: "ies"}[r.Reentry == 1])
}

func stbtMark(ctx context.Context, client *slack.Client, channel string, r live.STBTRules, k *stbtKite, st *live.STBTState) {
	var syms []string
	for _, l := range st.Legs {
		syms = append(syms, l.Symbol)
	}
	ltp, err := k.LTP(ctx, syms)
	if err != nil {
		post(ctx, client, channel, fmt.Sprintf(":warning: %s: no quotes for the overnight mark: %v", r.Name, err))
		return
	}
	var parts []string
	var pnl float64
	for _, l := range st.Legs {
		for _, t := range l.Trades {
			pnl += (t.Entry - t.Exit) * float64(st.Lot)
		}
		switch l.State {
		case live.LegOpen:
			pnl += (l.Entry - ltp[l.Symbol]) * float64(st.Lot)
			parts = append(parts, fmt.Sprintf("%.0f %s short @ ₹%.2f, now ₹%.2f, stop ₹%.2f", l.Strike, l.Right, l.Entry, ltp[l.Symbol], l.Stop))
		case live.LegWait, live.LegRecost:
			parts = append(parts, fmt.Sprintf("%.0f %s waiting to sell at ₹%.2f (now ₹%.2f)", l.Strike, l.Right, l.Trig, ltp[l.Symbol]))
		default:
			parts = append(parts, fmt.Sprintf("%.0f %s done", l.Strike, l.Right))
		}
	}
	post(ctx, client, channel, fmt.Sprintf(":crescent_moon: %s holding overnight into the %s expiry: %s · P&L so far ₹%.0f before charges; orders are placed again at 09:15",
		r.Name, st.Expiry, strings.Join(parts, "; "), pnl))
}

func stbtReport(ctx context.Context, client *slack.Client, channel string, r live.STBTRules, st *live.STBTState, e live.STBTEvent,
	costs *cost.Schedule, report string) {
	leg := ""
	if e.Leg != "" {
		leg = e.Leg + ": "
	}
	switch e.Kind {
	case "armed":
		post(ctx, client, channel, fmt.Sprintf(":dart: %s\nArmed %s", stbtTitle(r), e.Text))
	case "skip":
		post(ctx, client, channel, fmt.Sprintf(":double_vertical_bar: %s skipped today's entry: %s", r.Name, e.Text))
	case "sold":
		post(ctx, client, channel, fmt.Sprintf(":small_red_triangle_down: %s %s%s", r.Name, leg, e.Text))
	case "stop":
		post(ctx, client, channel, fmt.Sprintf(":octagonal_sign: %s %s%s", r.Name, leg, e.Text))
	case "trail":
		post(ctx, client, channel, fmt.Sprintf(":arrow_heading_down: %s %strail: %s", r.Name, leg, e.Text))
	case "exit":
		post(ctx, client, channel, fmt.Sprintf(":leftwards_arrow_with_hook: %s %sexit: %s", r.Name, leg, e.Text))
	case "halt":
		post(ctx, client, channel, fmt.Sprintf(":rotating_light: *%s HALTED* %s%s\nNothing more is placed until data/live/stbt-%s.json is fixed by hand; check Zerodha now.",
			r.Name, leg, e.Text, strings.ToLower(r.Index)))
	case "closed":
		stbtClosed(ctx, client, channel, r, st, costs, report)
	default:
		post(ctx, client, channel, fmt.Sprintf("%s %s%s", r.Name, leg, e.Text))
	}
}

// stbtClosed posts the straddle's result after charges and records it.
func stbtClosed(ctx context.Context, client *slack.Client, channel string, r live.STBTRules, st *live.STBTState, costs *cost.Schedule, report string) {
	exch := market.NFO
	if r.Exchange == "BFO" {
		exch = market.BFO
	}
	var gross, charges market.Paise
	var parts []string
	trades := 0
	for _, l := range st.Legs {
		right := market.Call
		if l.Right == "PE" {
			right = market.Put
		}
		inst := market.Option(exch, r.Index, st.Expiry, market.Rupees(l.Strike), right, st.Lot)
		for _, t := range l.Trades {
			trades++
			e, x := market.Rupees(t.Entry), market.Rupees(t.Exit)
			gross += (e - x) * market.Paise(st.Lot)
			c1, err1 := costs.Charges(market.Fill{Instrument: inst, Side: market.Sell, Qty: st.Lot, Price: e, Time: t.EntryTime, Kind: market.Traded})
			c2, err2 := costs.Charges(market.Fill{Instrument: inst, Side: market.Buy, Qty: st.Lot, Price: x, Time: t.ExitTime, Kind: market.Traded})
			if err := errors.Join(err1, err2); err != nil {
				fmt.Fprintln(os.Stderr, "charges:", err)
			}
			charges += c1 + c2
			parts = append(parts, fmt.Sprintf("%.0f %s ₹%.2f→₹%.2f (%s)", l.Strike, l.Right, t.Entry, t.Exit, t.Reason))
		}
	}
	net := gross - charges
	if trades == 0 {
		post(ctx, client, channel, fmt.Sprintf(":checkered_flag: *LIVE %s* %s expiry: nothing sold (no W&T trigger hit)", r.Name, st.Expiry))
	} else {
		post(ctx, client, channel, fmt.Sprintf(":checkered_flag: *LIVE %s* closed the %s expiry straddle: %s · gross ₹%s, charges ₹%s, *net ₹%s*",
			r.Name, st.Expiry, strings.Join(parts, ", "), gross, charges, net))
	}
	row := []string{r.Name, st.EntryDay.String(), st.Expiry.String(), fmt.Sprint(st.Lot), fmt.Sprintf("%.2f", st.Spot),
		strings.Join(parts, " | "), gross.String(), charges.String(), net.String()}
	if err := appendCSVRow(report, []string{"strategy", "entry_day", "expiry", "lot", "spot", "trades", "gross", "charges", "net"}, row); err != nil {
		fmt.Fprintln(os.Stderr, "live report:", err)
	}
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
