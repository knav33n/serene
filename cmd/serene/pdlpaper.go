package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"serene/internal/indicator"
	"serene/internal/kite"
	"serene/internal/market"
	"serene/internal/paper"
	"serene/internal/slack"
)

// pdlDayState is the day's decisions, kept so every later run agrees: whether
// yesterday set up, the level, and once NIFTY breaks it, the contract.
type pdlDayState struct {
	Day     market.Date `json:"day"`
	SetupOn market.Date `json:"setup_day"`
	Armed   bool        `json:"armed"`
	PDL     float64     `json:"pdl"`
	EMA20   float64     `json:"ema20"`
	// Set at the break.
	Signal  time.Time   `json:"signal,omitzero"`
	Spot    float64     `json:"spot,omitempty"`
	Strike  float64     `json:"strike,omitempty"`
	Expiry  market.Date `json:"expiry,omitzero"`
	Token   int64       `json:"token,omitempty"`
	Symbol  string      `json:"symbol,omitempty"`
	Lot     int64       `json:"lot_size,omitempty"`
	Premium float64     `json:"premium_at_signal,omitempty"`
	NoTrade string      `json:"no_trade,omitempty"`
}

// runPDLPaper posts pdl_swing_day's forward signals: each run checks whether
// yesterday set up, watches NIFTY's minute bars for a break of yesterday's low
// and, once it breaks, picks the deepest ITM put at or under ₹400 and posts
// the buy at the next minute's open and the sell at 15:15. No P&L is posted
// and no order goes to a broker.
func runPDLPaper(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("paper pdlswing", flag.ContinueOnError)
	useSlack := fs.Bool("slack", false, "post new signals to SLACK_PDL_CHANNEL")
	date := fs.String("date", "", "day to replay (default today, IST)")
	dir := fs.String("dir", "data/paper", "per-day decisions")
	sentPath := fs.String("sent", "data/pdl_sent.json", "events already posted")
	alertPath := fs.String("alert", "data/pdl_alert.json", "whether a failure has been posted")
	report := fs.String("report", "reports/paper_pdlswing.csv", "closed trades, appended as they are posted")
	from := fs.String("from", "2026-11-13", "first day of the forward test; earlier days post nothing")
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
		if dd := d.At(23, 59); now.After(dd) {
			now = dd
		}
	}
	start, err := market.ParseDate(*from)
	if err != nil {
		return err
	}
	if d.Before(start) {
		fmt.Println("the forward test starts", start)
		return nil
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
	if *useSlack {
		if client, channel, err = openSlackChannel(ctx, "SLACK_PDL_CHANNEL"); err != nil {
			return err
		}
	}
	err = pdlPaper(ctx, client, channel, d, now, *dir, *sentPath, *report)
	alertOnce(ctx, client, channel, *alertPath, "serene paper "+paper.PDLSwingName, err)
	return err
}

func prevTradingDay(d market.Date) market.Date {
	p := d.AddDays(-1)
	for !market.NSETradingDay(p) {
		p = p.AddDays(-1)
	}
	return p
}

// tradingDaysAfter counts NSE trading days in (a, b].
func tradingDaysAfter(a, b market.Date) int {
	n := 0
	for x := a.AddDays(1); !b.Before(x); x = x.AddDays(1) {
		if market.NSETradingDay(x) {
			n++
		}
	}
	return n
}

func kiteBars(cs []kite.Candle, key string, now time.Time) []market.Bar {
	var out []market.Bar
	for _, c := range cs {
		t := c.Time.In(market.IST)
		b := market.Bar{Key: key, Timeframe: market.Minute1, Time: t, End: t.Add(time.Minute),
			Open: market.Rupees(c.Open), High: market.Rupees(c.High), Low: market.Rupees(c.Low),
			Close: market.Rupees(c.Close), Volume: c.Volume, OI: c.OI}
		// Only whole minutes: Kite includes the one still forming.
		if b.End.After(now) {
			continue
		}
		out = append(out, b)
	}
	return out
}

func pdlPaper(ctx context.Context, client *slack.Client, channel string, d market.Date, now time.Time,
	dir, sentPath, report string) error {
	path := filepath.Join(dir, "pdl-"+d.String()+".json")
	var st pdlDayState
	have, err := readJSONFile(path, &st)
	if err != nil {
		return err
	}
	if have && (!st.Armed || st.NoTrade != "") {
		fmt.Printf("%s: %s\n", d, pdlStatus(st))
		return nil
	}
	kc, err := kiteClient(ctx)
	if err != nil {
		return err
	}
	if !have {
		if st, err = pdlSetupToday(ctx, kc, d); err != nil {
			return err
		}
		if err := writeJSONFile(path, st); err != nil {
			return err
		}
		if !st.Armed {
			fmt.Printf("%s: %s\n", d, pdlStatus(st))
			return nil
		}
	}

	if st.Signal.IsZero() {
		cs, err := kc.Candles(ctx, niftyIndexToken, "minute", d.At(9, 15), earlier(now, d.At(15, 30)), false)
		if err != nil {
			return fmt.Errorf("NIFTY 50 candles: %w", err)
		}
		idx := kiteBars(cs, market.Index(market.NSE, "NIFTY").Key(), now)
		i := paper.PDLBreak(idx, d, st.PDL)
		if i < 0 {
			fmt.Printf("%s: armed, NIFTY has not broken %.2f\n", d, st.PDL)
			return nil
		}
		if err := pdlPickContract(ctx, kc, &st, idx[i]); err != nil {
			return err
		}
		if err := writeJSONFile(path, st); err != nil {
			return err
		}
		if st.NoTrade != "" {
			fmt.Printf("%s: %s\n", d, pdlStatus(st))
			return nil
		}
	}

	inst := market.Option(market.NFO, "NIFTY", st.Expiry, market.Rupees(st.Strike), market.Put, st.Lot)
	cs, err := kc.Candles(ctx, st.Token, "minute", d.At(9, 15), earlier(now, d.At(15, 40)), false)
	if err != nil {
		return fmt.Errorf("%s candles: %w", st.Symbol, err)
	}
	opt := kiteBars(cs, inst.Key(), now)
	sigEnd := st.Signal.Add(time.Minute)
	j := paper.PDLFill(opt, sigEnd)
	if j < 0 {
		if now.Sub(sigEnd) > 4*time.Minute {
			st.NoTrade = "no trade in " + st.Symbol + " within 3 minutes of the signal"
			if err := writeJSONFile(path, st); err != nil {
				return err
			}
			fmt.Printf("%s: %s\n", d, pdlStatus(st))
		}
		return nil
	}
	entry := opt[j]
	xp, xt, closed := paper.PDLExit(opt, d, !now.Before(d.At(15, 30)))
	return postPDL(ctx, client, channel, st, inst, entry, xp, xt, closed, sentPath, report)
}

// pdlSetupToday decides from Kite's daily NIFTY candles whether yesterday set up.
func pdlSetupToday(ctx context.Context, kc *kite.Client, d market.Date) (pdlDayState, error) {
	prev := prevTradingDay(d)
	st := pdlDayState{Day: d, SetupOn: prev}
	cs, err := kc.Candles(ctx, niftyIndexToken, "day", d.AddDays(-120).At(0, 0), prev.At(23, 59), false)
	if err != nil {
		return st, fmt.Errorf("NIFTY 50 daily candles: %w", err)
	}
	var days []paper.PDLDay
	var bars []market.Bar
	for _, c := range cs {
		cd := market.DateOf(c.Time.In(market.IST))
		if !cd.Before(d) {
			continue
		}
		days = append(days, paper.PDLDay{D: cd, O: c.Open, H: c.High, L: c.Low, C: c.Close})
		bars = append(bars, market.Bar{Close: market.Rupees(c.Close)})
	}
	if len(days) < 25 || days[len(days)-1].D != prev {
		return st, fmt.Errorf("Kite's daily NIFTY 50 candles end before %s", prev)
	}
	ema := indicator.EMA(bars, 20)
	y := len(days) - 1
	st.Armed, st.PDL, st.EMA20 = paper.PDLSetup(days, ema, y), days[y].L, ema[y]
	return st, nil
}

// pdlPickContract chooses, at the signal bar, the nearest expiry at least 4
// trading days away and the deepest ITM put whose last price is ≤ ₹400.
func pdlPickContract(ctx context.Context, kc *kite.Client, st *pdlDayState, sig market.Bar) error {
	st.Signal, st.Spot = sig.Time, sig.Close.Rupees()
	ins, err := kc.Instruments(ctx, "NFO")
	if err != nil {
		return fmt.Errorf("NFO instruments: %w", err)
	}
	puts := map[float64]kite.Instrument{}
	var exp market.Date
	for _, in := range ins {
		if in.Name == "NIFTY" && in.Type == "PE" && tradingDaysAfter(st.Day, in.Expiry) >= paper.PDLSwingMinDays &&
			(exp.IsZero() || in.Expiry.Before(exp)) {
			exp = in.Expiry
		}
	}
	for _, in := range ins {
		if in.Name == "NIFTY" && in.Type == "PE" && in.Expiry == exp {
			puts[in.Strike] = in
		}
	}
	if len(puts) == 0 {
		st.NoTrade = "no NIFTY puts expiring 4+ trading days out"
		return nil
	}
	atm := math.Round(st.Spot/50) * 50
	for s := atm; s <= atm+30*50; s += 50 {
		in, ok := puts[s]
		if !ok {
			continue
		}
		cs, err := kc.Candles(ctx, in.Token, "minute", st.Day.At(9, 15), sig.End, false)
		if err != nil {
			return fmt.Errorf("%s candles: %w", in.Symbol, err)
		}
		var last *kite.Candle
		for k := range cs {
			if !cs[k].Time.In(market.IST).After(sig.Time) {
				last = &cs[k]
			}
		}
		if last == nil {
			continue
		}
		if last.Close > paper.PDLSwingCap {
			break
		}
		st.Strike, st.Expiry, st.Token, st.Symbol, st.Lot, st.Premium = s, exp, in.Token, in.Symbol, in.LotSize, last.Close
	}
	if st.Token == 0 {
		st.NoTrade = fmt.Sprintf("no NIFTY put at or under ₹%.0f at %s", paper.PDLSwingCap, sig.Time.Format("15:04"))
	}
	return nil
}

func pdlStatus(st pdlDayState) string {
	switch {
	case !st.Armed:
		return fmt.Sprintf("no setup on %s (EMA20 %.2f)", st.SetupOn, st.EMA20)
	case st.NoTrade != "":
		return "armed but no trade: " + st.NoTrade
	}
	return fmt.Sprintf("armed below %.2f", st.PDL)
}

func postPDL(ctx context.Context, client *slack.Client, channel string, st pdlDayState, inst market.Instrument,
	entry market.Bar, xp market.Paise, xt time.Time, closed bool, sentPath, report string) error {
	sent, err := loadSent(sentPath)
	if err != nil {
		return err
	}
	name := paperName(inst)
	at := func(t time.Time) string { return t.In(market.IST).Format("2006-01-02 15:04") }
	type event struct{ id, text string }
	evs := []event{{
		id: fmt.Sprintf("%s|entry|%s|%s", paper.PDLSwingName, inst.Key(), entry.Time.UTC().Format(time.RFC3339)),
		text: fmt.Sprintf(":large_green_circle: *%s* BUY %s @ ₹%s · 1 lot (%d qty) · NIFTY broke yesterday's low %.2f at %s · exit 15:15 · %s",
			paper.PDLSwingName, name, entry.Open, st.Lot, st.PDL, st.Signal.In(market.IST).Format("15:04"), at(entry.Time)),
	}}
	var row []string
	if closed {
		evs = append(evs, event{
			id: fmt.Sprintf("%s|exit|%s|%s", paper.PDLSwingName, inst.Key(), xt.UTC().Format(time.RFC3339)),
			text: fmt.Sprintf(":red_circle: *%s* SELL %s @ ₹%s · time exit · %s",
				paper.PDLSwingName, name, xp, at(xt)),
		})
		row = []string{st.Day.String(), name, fmt.Sprintf("%.2f", st.PDL), at(st.Signal), fmt.Sprint(st.Lot),
			at(entry.Time), entry.Open.String(), at(xt), xp.String()}
	}
	fresh := 0
	for _, e := range evs {
		if sent[e.id] {
			continue
		}
		fresh++
		fmt.Println(e.text)
		if client == nil {
			continue
		}
		if err := client.Post(ctx, channel, e.text); err != nil {
			return err
		}
		sent[e.id] = true
		if err := saveSent(sentPath, sent); err != nil {
			return err
		}
		if row != nil && e.id == evs[len(evs)-1].id {
			if err := appendCSVRow(report, pdlHeader, row); err != nil {
				return err
			}
		}
	}
	switch {
	case fresh == 0:
		fmt.Println("no new events")
	case client == nil:
		fmt.Printf("%d new event(s); --slack posts them\n", fresh)
	}
	return nil
}

var pdlHeader = []string{"day", "contract", "pdl", "signal", "lot", "entry_time", "entry", "exit_time", "exit"}

func appendCSVRow(path string, header, row []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	isNew := !fileExists(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	if isNew {
		_ = w.Write(header)
	}
	_ = w.Write(row)
	w.Flush()
	return errors.Join(w.Error(), f.Close())
}
