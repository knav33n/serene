package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"serene/internal/fyers"
	"serene/internal/kite"
	"serene/internal/manual"
	"serene/internal/market"
	"serene/internal/paper"
	"serene/internal/slack"
)

// hmaCrossMaxAge: an older cross (a backfill after downtime) is posted
// without order buttons.
const hmaCrossMaxAge = 15 * time.Minute

// runHMACross posts nifty_hma_crossover's signals: each run fetches the day's
// ATM NIFTY CE and PE minute bars from Kite, builds 10-minute bars, and posts
// every HMA 13/34 cross of the day not yet posted, with Zerodha buy and sell
// buttons for both legs. With --exit, a fresh down-cross also squares off,
// at market, what the strategy's buttons bought of that leg today.
func runHMACross(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("paper hmacross", flag.ContinueOnError)
	useSlack := fs.Bool("slack", false, "post new crosses to SLACK_PAPER_CHANNEL")
	date := fs.String("date", "", "day to replay (default today, IST)")
	dir := fs.String("dir", "data/paper", "per-day contract choices")
	sentPath := fs.String("sent", "data/hmacross_sent.json", "crosses and near-HMA alerts already posted")
	tsPath := fs.String("ts", "data/hmacross_ts.json", "Slack ts of each posted cross, for near-HMA thread replies")
	alertPath := fs.String("alert", "data/hmacross_alert.json", "whether a failure has been posted")
	step := fs.Float64("step", 100, "the index open is rounded to this for the strike")
	warm := fs.Int("warm", 4, "prior trading days of bars to warm the averages")
	exit := fs.Bool("exit", false, "with --slack: on a fresh down-cross, sell on Zerodha what the Buy buttons bought of that leg today (REAL orders)")
	fyersTrade := fs.Bool("fyers", false, "with --slack: on a fresh near-HMA alert buy 1 lot on Fyers, once a day, and sell at a 2% target by limit (REAL orders)")
	nearDir := fs.String("near-dir", "data/hmanear", "the near-HMA Fyers trade's state, one file a day")
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
	if !market.NSETradingDay(d) {
		fmt.Println(d, "is not an NSE trading day")
		return nil
	}
	if now.Before(d.At(9, 16)) {
		fmt.Println("before the first bar of", d)
		return nil
	}
	var client *slack.Client
	var channel string
	var err error
	if *useSlack {
		if client, channel, err = openSlackChannel(ctx, "SLACK_PAPER_CHANNEL"); err != nil {
			return err
		}
	}
	if (*exit || *fyersTrade) && client == nil {
		return errors.New("--exit and --fyers need --slack")
	}
	nd := ""
	if *fyersTrade {
		nd = *nearDir
	}
	err = hmaCross(ctx, client, channel, d, now, *dir, *sentPath, *tsPath, nd, *step, *warm, *exit)
	alertOnce(ctx, client, channel, *alertPath, "serene paper "+paper.HMACrossName, err)
	return err
}

func hmaCross(ctx context.Context, client *slack.Client, channel string, d market.Date, now time.Time,
	dir, sentPath, tsPath, nearDir string, step float64, warm int, exit bool) error {
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
		bars, ones, err := hmaCrossBars(inst, cs, d, now, cfg.TF)
		if err != nil {
			return fmt.Errorf("%s: %w", l.Symbol, err)
		}
		legs = append(legs, paper.Leg{Instrument: inst, Bars: bars, Ones: ones})
	}
	crosses := paper.HMACrosses(legs, cfg)
	if len(crosses) == 0 {
		fmt.Printf("%s: no cross yet (watching NIFTY %s CE/PE, exp %s, %dm HMA %d/%d)\n",
			d, market.Rupees(st.Strike), st.Expiry, cfg.TF, cfg.Fast, cfg.Slow)
		return nil
	}
	var exitFn func(ctx context.Context, symbol string) (string, error)
	if exit {
		exitFn = func(ctx context.Context, symbol string) (string, error) {
			return hmaCrossExit(ctx, kc, symbol, time.Now())
		}
	}
	if err := postHMACrosses(ctx, client, channel, st, cfg, crosses, now, sentPath, tsPath, exitFn); err != nil {
		return err
	}
	nears := paper.HMANears(legs, crosses, cfg)
	if err := postHMANears(ctx, client, channel, st, cfg, nears, now, sentPath, tsPath); err != nil {
		return err
	}
	if nearDir == "" || d != market.DateOf(time.Now().In(market.IST)) {
		return nil
	}
	return hmaNearTrade(ctx, nearDeps{
		broker: &lazyFyers{c: &clients{at: map[string]time.Time{}}},
		ticker: func(ctx context.Context, right market.Right) (string, int64, error) {
			rows, err := fyers.New("", "", nil).SymbolMaster(ctx, "NSE_FO")
			if err != nil {
				return "", 0, err
			}
			for _, r := range rows {
				if r.Underlying == "NIFTY" && r.Expiry == st.Expiry && r.Strike == st.Strike && r.Right == string(right) {
					return r.Ticker, r.LotSize, nil
				}
			}
			return "", 0, fmt.Errorf("no Fyers NIFTY %v %s for %s", st.Strike, right, st.Expiry)
		},
		price: func(ctx context.Context, symbol string) (float64, error) {
			q, err := kc.Quotes(ctx, []string{"NFO:" + symbol})
			if err != nil {
				return 0, err
			}
			v, ok := q["NFO:"+symbol]
			if !ok || v.Last <= 0 {
				return 0, fmt.Errorf("no quote for %s", symbol)
			}
			return v.Last, nil
		},
		notify: func(ctx context.Context, t *nearTrade, msg string) {
			fmt.Println(msg)
			tss := map[string]string{}
			_, _ = readJSONFile(tsPath, &tss)
			var err error
			if ts := tss[t.Cross]; ts != "" {
				err = client.Reply(ctx, channel, ts, msg)
			} else {
				err = client.Post(ctx, channel, msg)
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, "slack:", err)
			}
		},
		sleep: sleepCtx,
		now:   time.Now,
	}, st, nears, nearDir)
}

// hmaCrossBars is Kite's minute candles, whole minutes in the session up to
// d only, resampled to tf, without the tf bar still forming at now; and the
// whole minutes themselves.
func hmaCrossBars(inst market.Instrument, cs []kite.Candle, d market.Date, now time.Time, tf market.Timeframe) (bars, ones []market.Bar, err error) {
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
	if bars, err = market.Resample(ones, market.NSESession, tf); err != nil {
		return nil, nil, err
	}
	for len(bars) > 0 && bars[len(bars)-1].End.After(now) {
		bars = bars[:len(bars)-1]
	}
	return bars, ones, nil
}

func postHMACrosses(ctx context.Context, client *slack.Client, channel string, st *paperDayState, cfg paper.CrossConfig,
	crosses []paper.Cross, now time.Time, sentPath, tsPath string, exit func(ctx context.Context, symbol string) (string, error)) error {
	sent, err := loadSent(sentPath)
	if err != nil {
		return err
	}
	tss := map[string]string{}
	if _, err := readJSONFile(tsPath, &tss); err != nil {
		return err
	}
	buttons := os.Getenv("SLACK_ORDER_USERS") != ""
	fresh := 0
	for _, c := range crosses {
		id := hmaCrossID(c)
		if sent[id] {
			continue
		}
		fresh++
		text := hmaCrossText(c, cfg)
		fmt.Println(text)
		if client == nil {
			continue
		}
		recent := now.Sub(c.End) <= hmaCrossMaxAge
		// The square-off goes first: it's the time-critical part.
		var exitMsg string
		if exit != nil && recent && !c.Up {
			if sym := hmaCrossLegSymbol(st, c); sym != "" {
				msg, err := exit(ctx, sym)
				if err != nil {
					msg = fmt.Sprintf(":rotating_light: square-off of %s on the down-cross failed: %v; sell it on Kite", sym, err)
				}
				exitMsg = msg
				fmt.Println(msg)
			}
		}
		blocks := []slack.Block{slack.SectionBlock(text)}
		if buttons && recent {
			if row := hmaCrossButtons(st, c); row != nil {
				blocks = append(blocks, row)
			}
		}
		ts, err := client.PostBlocks(ctx, channel, text, blocks)
		if err != nil {
			return err
		}
		if exitMsg != "" {
			if err := client.Reply(ctx, channel, ts, exitMsg); err != nil {
				fmt.Fprintln(os.Stderr, "slack reply:", err)
			}
		}
		sent[id] = true
		if err := saveSent(sentPath, sent); err != nil {
			return err
		}
		tss[id] = ts
		if err := writeJSONFile(tsPath, tss); err != nil {
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

// hmaCrossID names a cross in the sent and ts files.
func hmaCrossID(c paper.Cross) string {
	dir := "down"
	if c.Up {
		dir = "up"
	}
	return fmt.Sprintf("%s|%s|%s|%s", paper.HMACrossName, c.Instrument.Key(), c.Time.UTC().Format(time.RFC3339), dir)
}

// postHMANears posts each near-HMA pullback not yet posted, in the thread of
// its up-cross when that message's ts is known, with the order buttons while
// the minute is fresh.
func postHMANears(ctx context.Context, client *slack.Client, channel string, st *paperDayState, cfg paper.CrossConfig,
	nears []paper.Near, now time.Time, sentPath, tsPath string) error {
	if len(nears) == 0 {
		return nil
	}
	sent, err := loadSent(sentPath)
	if err != nil {
		return err
	}
	tss := map[string]string{}
	if _, err := readJSONFile(tsPath, &tss); err != nil {
		return err
	}
	buttons := os.Getenv("SLACK_ORDER_USERS") != ""
	for _, n := range nears {
		id := "near|" + hmaCrossID(n.Cross)
		if sent[id] {
			continue
		}
		text := hmaNearText(n, cfg)
		fmt.Println(text)
		if client == nil {
			continue
		}
		blocks := []slack.Block{slack.SectionBlock(text)}
		if buttons && now.Sub(n.Time.Add(time.Minute)) <= hmaCrossMaxAge {
			if row := hmaCrossButtons(st, n.Cross); row != nil {
				blocks = append(blocks, row)
			}
		}
		if ts := tss[hmaCrossID(n.Cross)]; ts != "" {
			err = client.ReplyBlocks(ctx, channel, ts, text, blocks)
		} else {
			_, err = client.PostBlocks(ctx, channel, text, blocks)
		}
		if err != nil {
			return err
		}
		sent[id] = true
		if err := saveSent(sentPath, sent); err != nil {
			return err
		}
	}
	return nil
}

func hmaNearText(n paper.Near, cfg paper.CrossConfig) string {
	return fmt.Sprintf(":dart: *%s* %s near HMA%d · low ₹%s at %s vs %dm HMA%d %.2f (within %.0f%%) · after the %s up-cross",
		paper.HMACrossName, paperName(n.Cross.Instrument), n.Period, n.Low, n.Time.In(market.IST).Format("15:04"),
		cfg.TF, n.Period, n.MA, cfg.NearPct*100, n.Cross.End.In(market.IST).Format("15:04"))
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

// hmaCrossLegSymbol is the Kite symbol of the leg c crossed on.
func hmaCrossLegSymbol(st *paperDayState, c paper.Cross) string {
	for _, l := range st.Legs {
		if l.Right == c.Instrument.Right {
			return l.Symbol
		}
	}
	return ""
}

// hmaCrossExitTag marks the down-cross square-off's orders.
const hmaCrossExitTag = "hmaexit"

// exitBroker is the part of Kite hmaCrossExit uses.
type exitBroker interface {
	Orders(ctx context.Context) ([]kite.Order, error)
	Positions(ctx context.Context) ([]kite.Position, error)
	PlaceOrder(ctx context.Context, o kite.OrderRequest) (string, error)
	OrderStatus(ctx context.Context, id string) (kite.Order, error)
}

// hmaCrossExit sells at market, MIS, what the Buy buttons bought of symbol on
// Zerodha today and haven't sold: the buttons' filled buys less the buttons'
// and earlier square-offs' filled sells, at most the MIS quantity held. MIS
// bought some other way (the manual desk) is left alone. "" when there's
// nothing to sell.
func hmaCrossExit(ctx context.Context, kc exitBroker, symbol string, now time.Time) (string, error) {
	orders, err := kc.Orders(ctx)
	if err != nil {
		return "", fmt.Errorf("kite orders: %w", err)
	}
	var mine int64
	for _, o := range orders {
		if o.Symbol != symbol || o.Exchange != "NFO" || o.Product != "MIS" ||
			!(strings.HasPrefix(o.Tag, "slack") || strings.HasPrefix(o.Tag, hmaCrossExitTag)) {
			continue
		}
		if strings.EqualFold(o.Side, "BUY") {
			mine += o.Filled
		} else {
			mine -= o.Filled
		}
	}
	ps, err := kc.Positions(ctx)
	if err != nil {
		return "", fmt.Errorf("kite positions: %w", err)
	}
	var held int64
	for _, p := range ps {
		if p.Exchange == "NFO" && p.Symbol == symbol && p.Product == "MIS" {
			held += p.Qty
		}
	}
	qty := min(mine, held)
	if qty <= 0 {
		return "", nil
	}
	tag := hmaCrossExitTag + now.In(market.IST).Format("0102150405")
	var lines []string
	var errs []error
	for left := qty; left > 0; {
		q := min(left, niftyFreezeQty)
		left -= q
		id, err := kc.PlaceOrder(ctx, kite.OrderRequest{Exchange: "NFO", Symbol: symbol, Side: "SELL", Type: "MARKET", Product: "MIS", Qty: q, Tag: tag})
		if err != nil {
			errs = append(errs, err)
			break
		}
		msg, err := awaitFill(ctx, kc, id, fmt.Sprintf("down-cross square-off: SELL %d %s on Zerodha (order %s)", q, symbol, id))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		lines = append(lines, msg)
	}
	return strings.Join(lines, "\n"), errors.Join(errs...)
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

// lazyFyers logs in to Fyers on the first call, so a day without a near-HMA
// trade doesn't need the day's Fyers login.
type lazyFyers struct{ c *clients }

func (l *lazyFyers) PlaceOrder(ctx context.Context, o fyers.OrderRequest) (string, error) {
	t, err := l.c.fyers(ctx)
	if err != nil {
		return "", err
	}
	return t.PlaceOrder(ctx, o)
}

func (l *lazyFyers) Order(ctx context.Context, id string) (fyers.Order, error) {
	t, err := l.c.fyers(ctx)
	if err != nil {
		return fyers.Order{}, err
	}
	return t.Order(ctx, id)
}

func (l *lazyFyers) Positions(ctx context.Context) ([]fyers.Position, error) {
	t, err := l.c.fyers(ctx)
	if err != nil {
		return nil, err
	}
	return t.Positions(ctx)
}
