package main

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"time"

	"serene/internal/fyers"
	"serene/internal/market"
	"serene/internal/paper"
)

// The near-HMA auto trade (user, 2026-11-25): on a fresh near-HMA alert, buy
// one lot of that leg on Fyers at market, INTRADAY, at most once a day; then
// watch the price on the server and, once it reaches 2% over the fill, sell
// at a LIMIT of that target. No GTT, no resting target order, no stop loss.
const (
	nearTradeTag    = "hmanear"
	nearTargetPct   = 0.02
	nearMaxAge      = 3 * time.Minute // a near-HMA minute older than this isn't bought
	nearLastEntry   = 15 * 60         // minutes since midnight: no buys from 15:00
	nearWatch       = 35 * time.Second
	nearPollEvery   = 3 * time.Second
	nearOrderPolls  = 15
	nearOrderPollDT = 2 * time.Second
)

// nearTrade is the day's one trade, in data/hmanear/<day>.json.
type nearTrade struct {
	Day    market.Date  `json:"day"`
	Cross  string       `json:"cross"` // hmaCrossID of the up-cross
	Right  market.Right `json:"right"`
	Symbol string       `json:"symbol"` // Kite's, for the price
	Ticker string       `json:"ticker"` // Fyers', for the orders
	Qty    int64        `json:"qty"`
	// Phase: buying (buy placed, fill unknown), holding (target watched),
	// selling (target limit placed), done, failed.
	Phase  string  `json:"phase"`
	BuyID  string  `json:"buy_id,omitempty"`
	Entry  float64 `json:"entry,omitempty"`
	Target float64 `json:"target,omitempty"`
	SellID string  `json:"sell_id,omitempty"`
	Exit   float64 `json:"exit,omitempty"`
}

// nearBroker is the part of the Fyers Trader the trade uses.
type nearBroker interface {
	PlaceOrder(ctx context.Context, o fyers.OrderRequest) (string, error)
	Order(ctx context.Context, id string) (fyers.Order, error)
	Positions(ctx context.Context) ([]fyers.Position, error)
}

type nearDeps struct {
	broker nearBroker
	// ticker is the Fyers ticker and lot size of the day's leg.
	ticker func(ctx context.Context, right market.Right) (string, int64, error)
	// price is the leg's last traded price.
	price  func(ctx context.Context, symbol string) (float64, error)
	notify func(ctx context.Context, t *nearTrade, msg string)
	sleep  func(ctx context.Context, d time.Duration) error
	now    func() time.Time
}

// nearTargetPrice is 2% over the fill, rounded up to the 5-paise tick.
func nearTargetPrice(entry float64) float64 {
	return math.Round(math.Ceil(entry*(1+nearTargetPct)/0.05-1e-6)*0.05*100) / 100
}

// hmaNearTrade advances the day's near-HMA trade by one run.
func hmaNearTrade(ctx context.Context, dep nearDeps, st *paperDayState, nears []paper.Near, dir string) error {
	path := filepath.Join(dir, st.Day.String()+".json")
	var t nearTrade
	ok, err := readJSONFile(path, &t)
	if err != nil {
		return err
	}
	save := func() error { return writeJSONFile(path, &t) }
	if !ok {
		now := dep.now()
		var pick *paper.Near
		for i := range nears {
			n := &nears[i]
			if now.Sub(n.Time.Add(time.Minute)) <= nearMaxAge && minuteOfDay(n.Time) < nearLastEntry {
				pick = n
			}
		}
		if pick == nil {
			return nil
		}
		sym := hmaCrossLegSymbol(st, pick.Cross)
		ticker, lot, err := dep.ticker(ctx, pick.Cross.Instrument.Right)
		if err != nil {
			return fmt.Errorf("fyers ticker: %w", err)
		}
		t = nearTrade{Day: st.Day, Cross: hmaCrossID(pick.Cross), Right: pick.Cross.Instrument.Right, Symbol: sym,
			Ticker: ticker, Qty: lot, Phase: "buying"}
		// Saved before the order, so a crash can't buy twice in a day.
		if err := save(); err != nil {
			return err
		}
		id, err := dep.broker.PlaceOrder(ctx, fyers.OrderRequest{Symbol: ticker, Side: "BUY", Type: "MARKET",
			Product: "INTRADAY", Qty: lot, Tag: nearTradeTag})
		if err != nil {
			t.Phase = "failed"
			dep.notify(ctx, &t, fmt.Sprintf(":rotating_light: near-HMA auto buy of %d %s on Fyers failed: %v; no more trades today", lot, ticker, err))
			return save()
		}
		t.BuyID = id
		if err := save(); err != nil {
			return err
		}
	}
	switch t.Phase {
	case "buying":
		o, done, err := awaitFyers(ctx, dep, t.BuyID)
		if err != nil || !done {
			return err
		}
		if o.Status != 2 {
			t.Phase = "failed"
			dep.notify(ctx, &t, fmt.Sprintf(":rotating_light: near-HMA auto buy of %d %s on Fyers %s; no more trades today", t.Qty, t.Ticker, o.StatusText()))
			return save()
		}
		t.Entry, t.Target, t.Phase = o.TradedPrice, nearTargetPrice(o.TradedPrice), "holding"
		if err := save(); err != nil {
			return err
		}
		dep.notify(ctx, &t, fmt.Sprintf(":white_check_mark: near-HMA auto buy: BUY %d %s on Fyers filled @ ₹%.2f (order %s); target ₹%.2f (+2%%) is watched here and sold by limit when reached — no stop loss",
			t.Qty, t.Ticker, t.Entry, t.BuyID, t.Target))
		fallthrough
	case "holding":
		deadline := dep.now().Add(nearWatch)
		for {
			p, err := dep.price(ctx, t.Symbol)
			if err != nil {
				return fmt.Errorf("%s price: %w", t.Symbol, err)
			}
			if p >= t.Target {
				break
			}
			if !dep.now().Add(nearPollEvery).Before(deadline) {
				return nil
			}
			if err := dep.sleep(ctx, nearPollEvery); err != nil {
				return err
			}
		}
		ps, err := dep.broker.Positions(ctx)
		if err != nil {
			return fmt.Errorf("fyers positions: %w", err)
		}
		var held int64
		for _, p := range ps {
			if p.Symbol == t.Ticker && p.ProductType == "INTRADAY" {
				held += p.NetQty
			}
		}
		if held < t.Qty {
			t.Phase = "done"
			dep.notify(ctx, &t, fmt.Sprintf(":warning: %s reached the ₹%.2f target but Fyers holds %d of the %d bought; nothing sold", t.Ticker, t.Target, held, t.Qty))
			return save()
		}
		id, err := dep.broker.PlaceOrder(ctx, fyers.OrderRequest{Symbol: t.Ticker, Side: "SELL", Type: "LIMIT",
			Product: "INTRADAY", Qty: t.Qty, Price: t.Target, Tag: nearTradeTag})
		if err != nil {
			dep.notify(ctx, &t, fmt.Sprintf(":rotating_light: target sell of %d %s @ ₹%.2f on Fyers failed: %v; retrying next minute", t.Qty, t.Ticker, t.Target, err))
			return err
		}
		t.SellID, t.Phase = id, "selling"
		if err := save(); err != nil {
			return err
		}
		fallthrough
	case "selling":
		o, done, err := awaitFyers(ctx, dep, t.SellID)
		if err != nil || !done {
			return err
		}
		if o.Status != 2 {
			t.Phase = "holding"
			dep.notify(ctx, &t, fmt.Sprintf(":warning: target sell of %s on Fyers %s; watching the target again", t.Ticker, o.StatusText()))
			return save()
		}
		t.Exit, t.Phase = o.TradedPrice, "done"
		if err := save(); err != nil {
			return err
		}
		dep.notify(ctx, &t, fmt.Sprintf(":moneybag: near-HMA target hit: SELL %d %s on Fyers filled @ ₹%.2f (bought ₹%.2f, ₹%+.0f before charges)",
			t.Qty, t.Ticker, t.Exit, t.Entry, (t.Exit-t.Entry)*float64(t.Qty)))
	}
	return nil
}

// awaitFyers polls an order for up to half a minute; done is whether it
// stopped working (traded, rejected, cancelled, expired).
func awaitFyers(ctx context.Context, dep nearDeps, id string) (fyers.Order, bool, error) {
	var o fyers.Order
	var err error
	for i := range nearOrderPolls {
		if o, err = dep.broker.Order(ctx, id); err == nil && o.Done() {
			return o, true, nil
		}
		if i+1 < nearOrderPolls {
			if err := dep.sleep(ctx, nearOrderPollDT); err != nil {
				return o, false, err
			}
		}
	}
	return o, false, nil
}

func minuteOfDay(t time.Time) int {
	t = t.In(market.IST)
	return t.Hour()*60 + t.Minute()
}
