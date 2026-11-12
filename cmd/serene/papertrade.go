package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"serene/internal/cost"
	"serene/internal/kite"
	"serene/internal/market"
	"serene/internal/paper"
	"serene/internal/slack"
)

// niftyIndexToken is Kite's instrument token for the NIFTY 50 index.
const niftyIndexToken = 256265

// paperDayState is the day's watched contracts, chosen once at the open and
// kept so every later run watches the same two.
type paperDayState struct {
	Day    market.Date `json:"day"`
	Open   float64     `json:"index_open"`
	Strike float64     `json:"strike"`
	Expiry market.Date `json:"expiry"`
	Lot    int64       `json:"lot_size"`
	Legs   []struct {
		Right  market.Right `json:"right"`
		Token  int64        `json:"token"`
		Symbol string       `json:"symbol"`
	} `json:"legs"`
}

// runPaper paper trades nifty_hma_retest: each run fetches today's NIFTY
// open and the two ATM options' minute bars from Kite, replays the day and
// posts the entry and exit once each. No order goes to a broker.
func runPaper(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("paper hmaretest", flag.ContinueOnError)
	useSlack := fs.Bool("slack", false, "post new entries and exits to SLACK_PAPER_CHANNEL")
	date := fs.String("date", "", "day to replay (default today, IST)")
	dir := fs.String("dir", "data/paper", "per-day contract choices")
	sentPath := fs.String("sent", "data/paper_sent.json", "events already posted")
	alertPath := fs.String("alert", "data/paper_alert.json", "whether a failure has been posted")
	costsFile := fs.String("costs", "configs/costs.yaml", "NFO charges schedule")
	report := fs.String("report", "reports/paper_hmaretest.csv", "closed trades, appended as they are posted")
	step := fs.Float64("step", 100, "the index open is rounded to this for the strike")
	from := fs.String("from", "2026-11-13", "first day of the paper trade; earlier days post nothing")
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
	start, err := market.ParseDate(*from)
	if err != nil {
		return err
	}
	if d.Before(start) {
		fmt.Println("the paper trade starts", start)
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
		if client, channel, err = openSlackChannel(ctx, "SLACK_PAPER_CHANNEL"); err != nil {
			return err
		}
	}
	err = paperHMARetest(ctx, client, channel, d, now, *dir, *sentPath, *costsFile, *report, *step)
	alertOnce(ctx, client, channel, *alertPath, "serene paper "+paper.HMARetestName, err)
	return err
}

func paperHMARetest(ctx context.Context, client *slack.Client, channel string, d market.Date, now time.Time,
	dir, sentPath, costsFile, report string, step float64) error {
	costs, err := cost.Load(costsFile)
	if err != nil {
		return err
	}
	kc, err := kiteClient(ctx)
	if err != nil {
		return err
	}
	st, err := paperDay(ctx, kc, d, now, dir, step)
	if err != nil || st == nil {
		return err
	}
	prev := d.AddDays(-1)
	for !market.NSETradingDay(prev) {
		prev = prev.AddDays(-1)
	}
	var legs []paper.Leg
	for _, l := range st.Legs {
		inst := market.Option(market.NFO, "NIFTY", st.Expiry, market.Rupees(st.Strike), l.Right, st.Lot)
		cs, err := kc.Candles(ctx, l.Token, "minute", prev.At(9, 15), earlier(now, d.At(15, 40)), false)
		if err != nil {
			return fmt.Errorf("%s candles: %w", l.Symbol, err)
		}
		var bars []market.Bar
		for _, c := range cs {
			t := c.Time.In(market.IST)
			b := market.Bar{Key: inst.Key(), Timeframe: market.Minute1, Time: t, End: t.Add(time.Minute),
				Open: market.Rupees(c.Open), High: market.Rupees(c.High), Low: market.Rupees(c.Low),
				Close: market.Rupees(c.Close), Volume: c.Volume, OI: c.OI}
			// Only whole minutes: Kite includes the one still forming.
			if b.End.After(now) {
				continue
			}
			if bd := market.DateOf(t); bd == d || bd == prev {
				bars = append(bars, b)
			}
		}
		legs = append(legs, paper.Leg{Instrument: inst, Bars: bars})
	}
	tr, err := paper.HMARetest(legs, paper.DefaultHMARetest(d))
	if err != nil {
		return err
	}
	if tr == nil {
		fmt.Printf("%s: no entry yet (watching NIFTY %s CE/PE, exp %s)\n", d, market.Rupees(st.Strike), st.Expiry)
		return nil
	}
	return postPaper(ctx, client, channel, tr, costs, sentPath, report)
}

// paperDay loads or makes the day's contract choice. It is nil, with no
// error, until the index's 09:15 bar exists.
func paperDay(ctx context.Context, kc *kite.Client, d market.Date, now time.Time, dir string, step float64) (*paperDayState, error) {
	path := filepath.Join(dir, "day-"+d.String()+".json")
	var st paperDayState
	if ok, err := readJSONFile(path, &st); err != nil || ok {
		return &st, err
	}
	cs, err := kc.Candles(ctx, niftyIndexToken, "minute", d.At(9, 15), d.At(9, 16), false)
	if err != nil {
		return nil, fmt.Errorf("NIFTY 50 candles: %w", err)
	}
	if len(cs) == 0 || !cs[0].Time.Equal(d.At(9, 15)) {
		if now.After(d.At(9, 25)) {
			return nil, fmt.Errorf("Kite has no NIFTY 50 bar at 09:15 on %s", d)
		}
		return nil, nil
	}
	st = paperDayState{Day: d, Open: cs[0].Open, Strike: math.Round(cs[0].Open/step) * step}
	ins, err := kc.Instruments(ctx, "NFO")
	if err != nil {
		return nil, fmt.Errorf("NFO instruments: %w", err)
	}
	for _, in := range ins {
		if in.Name == "NIFTY" && (in.Type == "CE" || in.Type == "PE") && !in.Expiry.Before(d) &&
			(st.Expiry.IsZero() || in.Expiry.Before(st.Expiry)) {
			st.Expiry = in.Expiry
		}
	}
	for _, in := range ins {
		if in.Name != "NIFTY" || in.Expiry != st.Expiry || in.Strike != st.Strike || (in.Type != "CE" && in.Type != "PE") {
			continue
		}
		st.Lot = in.LotSize
		st.Legs = append(st.Legs, struct {
			Right  market.Right `json:"right"`
			Token  int64        `json:"token"`
			Symbol string       `json:"symbol"`
		}{market.Right(in.Type), in.Token, in.Symbol})
	}
	if len(st.Legs) != 2 || st.Lot <= 0 {
		return nil, fmt.Errorf("NIFTY %v CE/PE for %s not in Kite's NFO list", st.Strike, st.Expiry)
	}
	if err := writeJSONFile(path, st); err != nil {
		return nil, err
	}
	return &st, nil
}

func paperName(in market.Instrument) string {
	return fmt.Sprintf("NIFTY %s %s (exp %s)", in.Strike, in.Right, in.Expiry)
}

func postPaper(ctx context.Context, client *slack.Client, channel string, tr *paper.Trade, costs *cost.Schedule,
	sentPath, report string) error {
	sent, err := loadSent(sentPath)
	if err != nil {
		return err
	}
	name := paperName(tr.Instrument)
	type event struct{ id, text string }
	at := func(t time.Time) string { return t.In(market.IST).Format("2006-01-02 15:04") }
	evs := []event{{
		id: fmt.Sprintf("%s|entry|%s|%s", paper.HMARetestName, tr.Instrument.Key(), tr.EntryTime.UTC().Format(time.RFC3339)),
		text: fmt.Sprintf(":large_green_circle: *%s* BUY %s @ ₹%s · %d lots (%d qty) · SL ₹%s · %s",
			paper.HMARetestName, name, tr.Entry, tr.Lots, tr.Qty, tr.Stop, at(tr.EntryTime)),
	}}
	var row []string
	if !tr.Open {
		in, err := costs.Charges(market.Fill{Instrument: tr.Instrument, Side: market.Buy, Qty: tr.Qty, Price: tr.Entry, Time: tr.EntryTime, Kind: market.Traded})
		if err != nil {
			return err
		}
		out, err := costs.Charges(market.Fill{Instrument: tr.Instrument, Side: market.Sell, Qty: tr.Qty, Price: tr.Exit, Time: tr.ExitTime, Kind: market.Traded})
		if err != nil {
			return err
		}
		gross := (tr.Exit - tr.Entry) * market.Paise(tr.Qty)
		net := gross - in - out
		sign := "+"
		if net < 0 {
			sign = ""
		}
		evs = append(evs, event{
			id: fmt.Sprintf("%s|exit|%s|%s", paper.HMARetestName, tr.Instrument.Key(), tr.ExitTime.UTC().Format(time.RFC3339)),
			text: fmt.Sprintf(":red_circle: *%s* SELL %s @ ₹%s · %s (SL ₹%s) · P&L ₹%s%s after ₹%s charges · %s",
				paper.HMARetestName, name, tr.Exit, tr.Reason, tr.LastStop, sign, net, in+out, at(tr.ExitTime)),
		})
		row = []string{tr.EntryTime.In(market.IST).Format("2006-01-02"), name, at(tr.EntryTime), tr.Entry.String(),
			fmt.Sprint(tr.Lots), fmt.Sprint(tr.Qty), tr.Stop.String(), at(tr.ExitTime), tr.Exit.String(), tr.Reason,
			tr.LastStop.String(), gross.String(), (in + out).String(), net.String()}
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
			if err := appendPaperRow(report, row); err != nil {
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

var paperHeader = []string{"day", "contract", "entry_time", "entry", "lots", "qty", "stop", "exit_time", "exit",
	"reason", "last_stop", "gross", "charges", "net"}

func appendPaperRow(path string, row []string) error {
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
		_ = w.Write(paperHeader)
	}
	_ = w.Write(row)
	w.Flush()
	return errors.Join(w.Error(), f.Close())
}

func readJSONFile(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
