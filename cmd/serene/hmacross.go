package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"serene/internal/auth"
	"serene/internal/kite"
	"serene/internal/manual"
	"serene/internal/market"
	"serene/internal/paper"
	"serene/internal/slack"
)

// hmaCrossMaxAge: an older cross (a backfill after downtime) is posted
// without order buttons.
const hmaCrossMaxAge = 15 * time.Minute

// hmaCrossTest is the strategy on test buttons: Hearth answers their clicks
// as a dry run, whatever its own setting.
const hmaCrossTest = paper.HMACrossName + "_test"

// runHMACross posts nifty_hma_crossover's signals: each run fetches the day's
// ATM NIFTY CE and PE minute bars from Kite, builds 10-minute bars, and posts
// every HMA 13/34 cross of the day not yet posted, with Zerodha buy and sell
// buttons for both legs. It places no order itself.
func runHMACross(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("paper hmacross", flag.ContinueOnError)
	useSlack := fs.Bool("slack", false, "post new crosses to SLACK_PAPER_CHANNEL")
	date := fs.String("date", "", "day to replay (default today, IST)")
	dir := fs.String("dir", "data/paper", "per-day contract choices")
	sentPath := fs.String("sent", "data/hmacross_sent.json", "crosses already posted")
	alertPath := fs.String("alert", "data/hmacross_alert.json", "whether a failure has been posted")
	step := fs.Float64("step", 100, "the index open is rounded to this for the strike")
	warm := fs.Int("warm", 4, "prior trading days of bars to warm the averages")
	testChannel := fs.String("test", "", "post a TEST up-cross and down-cross, with dry-run buttons, to this channel ID, and stop")
	if err := fs.Parse(args); err != nil {
		return err
	}
	now := time.Now().In(market.IST)
	d := market.DateOf(now)
	if *date != "" {
		var err error
		if d, err = market.ParseDate(*date); err != nil {
			return err
		}
	}
	if *testChannel != "" && *date == "" {
		// The test uses the latest session's contracts.
		if now.Before(d.At(9, 16)) {
			d = d.AddDays(-1)
		}
		for !market.NSETradingDay(d) {
			d = d.AddDays(-1)
		}
	}
	if !market.NSETradingDay(d) {
		fmt.Println(d, "is not an NSE trading day")
		return nil
	}
	if *testChannel == "" && now.Before(d.At(9, 16)) {
		fmt.Println("before the first bar of", d)
		return nil
	}
	var client *slack.Client
	var channel string
	var err error
	if *testChannel != "" {
		return hmaCrossTestPost(ctx, d, now, *dir, *step, *testChannel)
	}
	if *useSlack {
		if client, channel, err = openSlackChannel(ctx, "SLACK_PAPER_CHANNEL"); err != nil {
			return err
		}
	}
	err = hmaCross(ctx, client, channel, d, now, *dir, *sentPath, *step, *warm)
	alertOnce(ctx, client, channel, *alertPath, "serene paper "+paper.HMACrossName, err)
	return err
}

func hmaCross(ctx context.Context, client *slack.Client, channel string, d market.Date, now time.Time,
	dir, sentPath string, step float64, warm int) error {
	kc, err := kiteClient(ctx)
	if err != nil {
		return err
	}
	st, err := paperDay(ctx, kc, d, now, dir, step)
	if err != nil || st == nil {
		return err
	}
	cfg := paper.DefaultHMACross(d)
	first := d
	for range warm {
		first = first.AddDays(-1)
		for !market.NSETradingDay(first) {
			first = first.AddDays(-1)
		}
	}
	var legs []paper.Leg
	for _, l := range st.Legs {
		inst := market.Option(market.NFO, "NIFTY", st.Expiry, market.Rupees(st.Strike), l.Right, st.Lot)
		cs, err := kc.Candles(ctx, l.Token, "minute", first.At(9, 15), earlier(now, d.At(15, 40)), false)
		if err != nil {
			return fmt.Errorf("%s candles: %w", l.Symbol, err)
		}
		bars, err := hmaCrossBars(inst, cs, d, now, cfg.TF)
		if err != nil {
			return fmt.Errorf("%s: %w", l.Symbol, err)
		}
		legs = append(legs, paper.Leg{Instrument: inst, Bars: bars})
	}
	crosses := paper.HMACrosses(legs, cfg)
	if len(crosses) == 0 {
		fmt.Printf("%s: no cross yet (watching NIFTY %s CE/PE, exp %s, %dm HMA %d/%d)\n",
			d, market.Rupees(st.Strike), st.Expiry, cfg.TF, cfg.Fast, cfg.Slow)
		return nil
	}
	return postHMACrosses(ctx, client, channel, st, cfg, crosses, now, sentPath)
}

// hmaCrossBars is Kite's minute candles, whole minutes in the session up to
// d only, resampled to tf, without the tf bar still forming at now.
func hmaCrossBars(inst market.Instrument, cs []kite.Candle, d market.Date, now time.Time, tf market.Timeframe) ([]market.Bar, error) {
	var ones []market.Bar
	for _, c := range cs {
		t := c.Time.In(market.IST)
		b := market.Bar{Key: inst.Key(), Timeframe: market.Minute1, Time: t, End: t.Add(time.Minute),
			Open: market.Rupees(c.Open), High: market.Rupees(c.High), Low: market.Rupees(c.Low),
			Close: market.Rupees(c.Close), Volume: c.Volume, OI: c.OI}
		// Only whole minutes: Kite includes the one still forming.
		if b.End.After(now) || d.Before(market.DateOf(t)) || !market.NSESession.Contains(t) {
			continue
		}
		ones = append(ones, b)
	}
	bars, err := market.Resample(ones, market.NSESession, tf)
	if err != nil {
		return nil, err
	}
	for len(bars) > 0 && bars[len(bars)-1].End.After(now) {
		bars = bars[:len(bars)-1]
	}
	return bars, nil
}

func postHMACrosses(ctx context.Context, client *slack.Client, channel string, st *paperDayState, cfg paper.CrossConfig,
	crosses []paper.Cross, now time.Time, sentPath string) error {
	sent, err := loadSent(sentPath)
	if err != nil {
		return err
	}
	buttons := os.Getenv("SLACK_ORDER_USERS") != ""
	fresh := 0
	for _, c := range crosses {
		dir := "down"
		if c.Up {
			dir = "up"
		}
		id := fmt.Sprintf("%s|%s|%s|%s", paper.HMACrossName, c.Instrument.Key(), c.Time.UTC().Format(time.RFC3339), dir)
		if sent[id] {
			continue
		}
		fresh++
		text := hmaCrossText(c, cfg)
		fmt.Println(text)
		if client == nil {
			continue
		}
		blocks := []slack.Block{slack.SectionBlock(text)}
		if buttons && now.Sub(c.End) <= hmaCrossMaxAge {
			if row := hmaCrossButtons(st, c); row != nil {
				blocks = append(blocks, row)
			}
		}
		if _, err := client.PostBlocks(ctx, channel, text, blocks); err != nil {
			return err
		}
		sent[id] = true
		if err := saveSent(sentPath, sent); err != nil {
			return err
		}
	}
	switch {
	case fresh == 0:
		fmt.Println("no new crosses")
	case client == nil:
		fmt.Printf("%d new cross(es); --slack posts them\n", fresh)
	}
	return nil
}

func hmaCrossText(c paper.Cross, cfg paper.CrossConfig) string {
	emoji, word, rel := ":large_green_circle:", "UP", ">"
	if !c.Up {
		emoji, word, rel = ":red_circle:", "DOWN", "<"
	}
	return fmt.Sprintf("%s *%s* %s crossed %s · %dm HMA%d %.2f %s HMA%d %.2f · close ₹%s · bar %s–%s",
		emoji, paper.HMACrossName, paperName(c.Instrument), word, cfg.TF, cfg.Fast, c.Fast, rel, cfg.Slow, c.Slow, c.Close,
		c.Time.In(market.IST).Format("15:04"), c.End.In(market.IST).Format("15:04"))
}

// hmaCrossButtons is Buy CE and Buy PE on every cross, the crossing leg's
// buy green on an up-cross, and on a down-cross a red Sell for the crossing
// leg, which sells what is held of it.
func hmaCrossButtons(st *paperDayState, c paper.Cross) slack.Block {
	var bs []manual.OrderButton
	var sell *manual.OrderButton
	for _, l := range st.Legs {
		r := string(l.Right)
		b := manual.OrderButton{Order: manual.FutOrder{Strategy: paper.HMACrossName, Side: market.Buy, Symbol: l.Symbol},
			Text:    "Buy " + r,
			Confirm: fmt.Sprintf("*BUY 1 lot (%d qty) %s* on Zerodha at market (MIS), for %s.", st.Lot, l.Symbol, paper.HMACrossName)}
		if l.Right == c.Instrument.Right {
			if c.Up {
				b.Style = "primary"
			} else {
				sell = &manual.OrderButton{Order: manual.FutOrder{Strategy: paper.HMACrossName, Side: market.Sell, Symbol: l.Symbol},
					Text: "Sell " + r, Style: "danger",
					Confirm: fmt.Sprintf("*SELL all the MIS %s you hold* on Zerodha at market, for %s.", l.Symbol, paper.HMACrossName)}
			}
		}
		bs = append(bs, b)
	}
	if len(bs) == 0 {
		return nil
	}
	if sell != nil {
		bs = append(bs, *sell)
	}
	return manual.OrderButtons(bs...)
}

// optBroker is the part of Kite placeOpt uses, so tests can fake it.
type optBroker interface {
	Instruments(ctx context.Context, exchange string) ([]kite.Instrument, error)
	Positions(ctx context.Context) ([]kite.Position, error)
	PlaceOrder(ctx context.Context, o kite.OrderRequest) (string, error)
	OrderStatus(ctx context.Context, id string) (kite.Order, error)
}

// niftyFreezeQty is the largest NIFTY option order NSE accepts.
const niftyFreezeQty = 1755

// placeOpt places a nifty_hma_crossover button's order on Zerodha, at market,
// MIS: a buy is one lot of an unexpired NIFTY option, a sell is all of the
// MIS quantity held long in it. A dry run stops before the order.
func placeOpt(ctx context.Context, kc optBroker, o manual.FutOrder, now time.Time, dryRun bool) (string, error) {
	ins, err := kc.Instruments(ctx, "NFO")
	if err != nil {
		return "", fmt.Errorf("kite NFO instruments: %w", err)
	}
	today := market.DateOf(now.In(market.IST))
	var inst *kite.Instrument
	for i := range ins {
		in := &ins[i]
		if in.Symbol == o.Symbol && in.Name == "NIFTY" && (in.Type == "CE" || in.Type == "PE") && !in.Expiry.Before(today) {
			inst = in
			break
		}
	}
	if inst == nil {
		return "", fmt.Errorf("%s isn't a live NIFTY option on Kite", o.Symbol)
	}
	side, qty := "BUY", inst.LotSize
	if o.Side == market.Sell {
		side = "SELL"
		ps, err := kc.Positions(ctx)
		if err != nil {
			return "", fmt.Errorf("kite positions: %w", err)
		}
		qty = 0
		for _, p := range ps {
			if p.Exchange == "NFO" && p.Symbol == o.Symbol && p.Product == "MIS" {
				qty += p.Qty
			}
		}
		if qty <= 0 {
			return "", fmt.Errorf("no MIS %s held on Zerodha; nothing sold", o.Symbol)
		}
		if qty > niftyFreezeQty {
			return "", fmt.Errorf("%d %s held is over the %d freeze quantity; sell it on Kite", qty, o.Symbol, niftyFreezeQty)
		}
	}
	if dryRun {
		return fmt.Sprintf(":test_tube: dry run: would %s %d %s on Zerodha (MARKET, MIS) for %s; nothing placed", side, qty, o.Symbol, o.Strategy), nil
	}
	id, err := kc.PlaceOrder(ctx, kite.OrderRequest{Exchange: "NFO", Symbol: o.Symbol, Side: side, Type: "MARKET", Product: "MIS",
		Qty: qty, Tag: "slack" + now.In(market.IST).Format("0102150405")})
	if err != nil {
		return "", err
	}
	return awaitFill(ctx, kc, id, fmt.Sprintf("%s %d %s on Zerodha (order %s)", side, qty, o.Symbol, id))
}

// awaitFill waits up to half a minute for order id to fill and says how it went.
func awaitFill(ctx context.Context, kc interface {
	OrderStatus(ctx context.Context, id string) (kite.Order, error)
}, id, what string) (string, error) {
	var st kite.Order
	var err error
	for range 15 {
		if err := sleepCtx(ctx, 2*time.Second); err != nil {
			break
		}
		if st, err = kc.OrderStatus(ctx, id); err != nil {
			continue
		}
		switch strings.ToUpper(st.Status) {
		case "COMPLETE":
			return fmt.Sprintf(":white_check_mark: %s filled @ ₹%.2f", what, st.Average), nil
		case "REJECTED", "CANCELLED":
			return "", fmt.Errorf("%s %s: %s", what, strings.ToLower(st.Status), st.StatusMessage)
		}
	}
	return fmt.Sprintf(":hourglass: %s placed; status %s after 30s, check Kite", what, st.Status), nil
}

// hmaCrossTestPost posts a made-up up-cross on the CE and down-cross on the
// PE of day's contracts, each with its button, marked TEST. The buttons carry
// hmaCrossTest, so a click is checked on Kite but never placed.
func hmaCrossTestPost(ctx context.Context, d market.Date, now time.Time, dir string, step float64, channel string) error {
	if err := auth.LoadDotEnv(".env"); err != nil {
		return err
	}
	token := os.Getenv("SLACK_TOKEN")
	if token == "" {
		return errors.New("needs SLACK_TOKEN")
	}
	kc, err := kiteClient(ctx)
	if err != nil {
		return err
	}
	st, err := paperDay(ctx, kc, d, d.At(15, 30), dir, step)
	if err != nil {
		return err
	}
	if st == nil {
		return fmt.Errorf("no NIFTY 09:15 bar on %s", d)
	}
	sc := slack.New(token)
	ch, err := sc.Resolve(ctx, channel)
	if err != nil {
		return err
	}
	cfg := paper.DefaultHMACross(d)
	for _, up := range []bool{true, false} {
		right := market.Call
		if !up {
			right = market.Put
		}
		c := paper.Cross{Instrument: market.Option(market.NFO, "NIFTY", st.Expiry, market.Rupees(st.Strike), right, st.Lot),
			Time: now.Add(-10 * time.Minute), End: now, Up: up, Close: market.Rupees(100), Fast: 101, Slow: 100}
		if !up {
			c.Fast = 99
		}
		text := ":test_tube: *TEST, not a signal* (buttons are a dry run) · " + hmaCrossText(c, cfg)
		row := hmaCrossButtons(st, c)
		for _, b := range row["elements"].([]map[string]any) {
			b["value"] = strings.Replace(b["value"].(string), paper.HMACrossName+"|", hmaCrossTest+"|", 1)
		}
		if _, err := sc.PostBlocks(ctx, ch, text, []slack.Block{slack.SectionBlock(text), row}); err != nil {
			return err
		}
		fmt.Println("posted:", text)
	}
	return nil
}
