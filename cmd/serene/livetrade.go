package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"serene/internal/cost"
	"serene/internal/ingest"
	"serene/internal/kite"
	"serene/internal/live"
	"serene/internal/market"
	"serene/internal/paper"
	"serene/internal/slack"
	"serene/internal/upstox"
)

// liveOpts are the flags for real orders.
type liveOpts struct {
	brokers []string // zerodha, upstox
	capital market.Paise
	maxQty  int64
	dir     string
	report  string
}

func parseBrokers(s string) ([]string, error) {
	var out []string
	for _, b := range strings.Split(s, ",") {
		b = strings.TrimSpace(b)
		switch b {
		case "":
		case "zerodha", "upstox":
			out = append(out, b)
		default:
			return nil, fmt.Errorf("--live: unknown broker %q (zerodha, upstox)", b)
		}
	}
	return out, nil
}

// liveHMARetest runs one minute of real orders on each broker. A broker's
// failure is alerted and doesn't stop the others.
func liveHMARetest(ctx context.Context, client *slack.Client, channel string, kc *kite.Client, ds *paperDayState,
	legs []paper.Leg, costs *cost.Schedule, d market.Date, now time.Time, o liveOpts) error {
	cfg := live.Config{Strategy: paper.DefaultHMARetest(d), Capital: o.capital, MaxQty: o.maxQty, Buffer: 0.08,
		NoEntry: fileExists(filepath.Join(o.dir, "STOP"))}
	var errs []error
	for _, name := range o.brokers {
		err := liveOne(ctx, client, channel, kc, ds, legs, costs, cfg, d, now, o, name)
		alertOnce(ctx, client, channel, filepath.Join(o.dir, name+"_alert.json"), "serene live "+paper.HMARetestName+" on "+name, err)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func liveOne(ctx context.Context, client *slack.Client, channel string, kc *kite.Client, ds *paperDayState,
	legs []paper.Leg, costs *cost.Schedule, cfg live.Config, d market.Date, now time.Time, o liveOpts, name string) error {
	path := filepath.Join(o.dir, fmt.Sprintf("%s-%s.json", name, d))
	var st live.State
	if _, err := readJSONFile(path, &st); err != nil {
		return err
	}
	if st.Phase == live.Done {
		return nil
	}
	var b live.Broker
	switch name {
	case "zerodha":
		syms := map[string]string{}
		for _, l := range ds.Legs {
			syms[market.Option(market.NFO, "NIFTY", ds.Expiry, market.Rupees(ds.Strike), l.Right, ds.Lot).Key()] = l.Symbol
		}
		b = &live.Zerodha{C: kc, Symbols: syms}
	case "upstox":
		uc, err := upstoxClient(ctx)
		if err != nil {
			return err
		}
		keys, err := upstoxKeys(ctx, uc, ds, filepath.Join(o.dir, fmt.Sprintf("upstox-keys-%s.json", d)))
		if err != nil {
			return err
		}
		b = &live.Upstox{C: uc, Keys: keys}
	}
	evs, stepErr := live.Step(ctx, b, &st, legs, cfg, now)
	// Saved even after an error: it holds the tag of a failed request.
	if err := writeJSONFile(path, st); err != nil {
		return errors.Join(stepErr, err)
	}
	for _, e := range evs {
		text := e.Text
		if c := e.Closed; c != nil {
			net, charges, err := liveNet(costs, c)
			if err == nil {
				text += fmt.Sprintf(" · net ₹%s after ₹%s charges", net, charges)
				row := []string{d.String(), name, paperName(c.Instrument), c.EntryTime.In(market.IST).Format("15:04"),
					c.Entry.String(), fmt.Sprint(c.Qty), c.Stop.String(), c.ExitTime.In(market.IST).Format("15:04"),
					c.Exit.String(), c.Reason, c.LastStop.String(), net.String(), charges.String()}
				if err := appendCSVRow(o.report, liveHeader, row); err != nil {
					fmt.Fprintln(os.Stderr, "live report:", err)
				}
			}
		}
		fmt.Println(text)
		if client != nil {
			if err := client.Post(ctx, channel, text); err != nil {
				fmt.Fprintln(os.Stderr, "slack:", err)
			}
		}
	}
	if stepErr == nil {
		fmt.Printf("%s: %s\n", name, st.Phase)
	}
	return stepErr
}

var liveHeader = []string{"day", "broker", "contract", "entry_time", "entry", "qty", "stop", "exit_time", "exit",
	"reason", "last_stop", "net", "charges"}

func liveNet(costs *cost.Schedule, c *live.Closed) (net, charges market.Paise, err error) {
	in, err := costs.Charges(market.Fill{Instrument: c.Instrument, Side: market.Buy, Qty: c.Qty, Price: c.Entry, Time: c.EntryTime, Kind: market.Traded})
	if err != nil {
		return 0, 0, err
	}
	out, err := costs.Charges(market.Fill{Instrument: c.Instrument, Side: market.Sell, Qty: c.Qty, Price: c.Exit, Time: c.ExitTime, Kind: market.Traded})
	if err != nil {
		return 0, 0, err
	}
	return (c.Exit-c.Entry)*market.Paise(c.Qty) - in - out, in + out, nil
}

// upstoxKeys maps the day's two contracts to Upstox instrument keys, looked
// up once and kept in path.
func upstoxKeys(ctx context.Context, uc *upstox.Client, ds *paperDayState, path string) (map[string]string, error) {
	keys := map[string]string{}
	if ok, err := readJSONFile(path, &keys); err != nil || ok {
		return keys, err
	}
	cs, err := uc.LiveOptionContracts(ctx, ingest.IndexKeys["NIFTY"], ds.Expiry)
	if err != nil {
		return nil, fmt.Errorf("upstox contracts: %w", err)
	}
	for _, l := range ds.Legs {
		inst := market.Option(market.NFO, "NIFTY", ds.Expiry, market.Rupees(ds.Strike), l.Right, ds.Lot)
		for _, c := range cs {
			if c.Strike == ds.Strike && c.Type == string(l.Right) && c.Expiry == ds.Expiry.String() {
				if c.LotSize != ds.Lot {
					return nil, fmt.Errorf("upstox lot %d for %s, Kite %d", c.LotSize, c.Symbol, ds.Lot)
				}
				keys[inst.Key()] = c.Key
			}
		}
		if keys[inst.Key()] == "" {
			return nil, fmt.Errorf("no Upstox contract for %s", inst.Key())
		}
	}
	return keys, writeJSONFile(path, keys)
}

// liveCheck reads each account and places nothing: the morning's check that
// logins, funds and order access are in place.
func liveCheck(ctx context.Context, brokers []string, dayDir string, step float64) error {
	kc, err := kiteClient(ctx)
	if err != nil {
		return err
	}
	d := market.DateOf(time.Now().In(market.IST))
	var ds *paperDayState
	if market.NSETradingDay(d) {
		var st paperDayState
		if ok, err := readJSONFile(filepath.Join(dayDir, "day-"+d.String()+".json"), &st); err == nil && ok {
			ds = &st
		}
	}
	var errs []error
	for _, name := range brokers {
		fmt.Println("==", name)
		var b live.Broker
		switch name {
		case "zerodha":
			b = &live.Zerodha{C: kc}
		case "upstox":
			uc, err := upstoxClient(ctx)
			if err != nil {
				errs = append(errs, fmt.Errorf("upstox: %w", err))
				continue
			}
			if p, s, err := uc.StaticIPs(ctx); err != nil {
				fmt.Println("static IPs:", err)
			} else {
				fmt.Printf("static IPs: primary %q secondary %q\n", p, s)
			}
			if ds != nil {
				keys, err := upstoxKeys(ctx, uc, ds, filepath.Join(os.TempDir(), "serene-upstox-keys-check.json"))
				os.Remove(filepath.Join(os.TempDir(), "serene-upstox-keys-check.json"))
				fmt.Println("today's contracts:", keys, err)
			}
			b = &live.Upstox{C: uc}
		}
		f, err := b.Funds(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s funds: %w", name, err))
			continue
		}
		fmt.Printf("funds ₹%s\n", f)
		switch x := b.(type) {
		case *live.Zerodha:
			ps, err := x.C.Positions(ctx)
			fmt.Println("positions:", len(ps), err)
			os, err := x.C.Orders(ctx)
			fmt.Println("orders today:", len(os), err)
		case *live.Upstox:
			ps, err := x.C.Positions(ctx)
			fmt.Println("positions:", len(ps), err)
			os, err := x.C.Orders(ctx)
			fmt.Println("orders today:", len(os), err)
		}
	}
	_ = step
	return errors.Join(errs...)
}
