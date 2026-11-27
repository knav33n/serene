package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"serene/internal/fyers"
	"serene/internal/kite"
	"serene/internal/market"
	"serene/internal/paper"
	"serene/internal/slack"
)

// The near-HMA auto trade (user, 2026-11-25; widened 2026-11-26): on a
// fresh near-HMA alert, buy that leg at market, intraday, on Fyers and on
// Zerodha, at most twice a day per index on each broker, one lot on each
// (user, 2026-11-27; before, Fyers bought with all its free cash and Zerodha
// with up to ₹1L). The server watches the price and, once it reaches 2% over the fill, sells at a LIMIT of that
// target. No GTT, no resting target order, no stop loss. From 2026-11-27
// (user): alerts come from the ATM and ±100 strikes, a contract is entered
// at most once a day per broker, and never while that broker already holds
// a position in it (another strategy's, e.g. stbt's straddle, or a manual one).
const (
	nearTradeTag    = "hmanear"
	nearTargetPct   = 0.02
	nearMaxAge      = 3 * time.Minute // a near-HMA minute older than this isn't bought
	nearLastEntry   = 15 * 60         // minutes since midnight: no buys from 15:00
	nearPerDay      = 2               // trades a day per index per broker
	nearMaxLots     = 1               // lots per trade on each broker (user, 2026-11-27)
	nearFundsUse    = 0.98            // of the cap, leaving room for charges and the move since the quote
	nearWatch       = 35 * time.Second
	nearPollEvery   = 3 * time.Second
	nearOrderPolls  = 15
	nearOrderPollDT = 2 * time.Second
)

// nearTrade is one auto trade, in data/hmanear/<day>-<INDEX>-<broker>.json.
type nearTrade struct {
	Day    market.Date  `json:"day"`
	Index  string       `json:"index"`
	Broker string       `json:"broker"`
	Near   string       `json:"near"`  // the near alert's sent id
	Cross  string       `json:"cross"` // hmaCrossID of the up-cross
	Right  market.Right `json:"right"`
	Quote  string       `json:"quote"`  // Kite's EXCHANGE:SYMBOL, for the price
	Ticker string       `json:"ticker"` // the broker's symbol, for the orders
	Qty    int64        `json:"qty"`    // bought, once filled
	Lot    int64        `json:"lot"`
	Freeze int64        `json:"freeze"` // largest single order
	// Phase: buying (buys placed, fills unknown), holding (target watched),
	// selling (target limits placed), done, failed; skipped (not bought, as
	// the broker already held the contract) doesn't count as a trade.
	Phase   string   `json:"phase"`
	BuyIDs  []string `json:"buy_ids,omitempty"`
	Entry   float64  `json:"entry,omitempty"`
	Target  float64  `json:"target,omitempty"`
	SellIDs []string `json:"sell_ids,omitempty"`
	Sold    int64    `json:"sold,omitempty"`
	SoldAmt float64  `json:"sold_amt,omitempty"` // ₹ value of what was sold
	// The tick CSV (from 2026-11-27): from the near alert's minute to the
	// close, sent to Slack once.
	SignalAt time.Time `json:"signal_at,omitzero"` // the near-HMA minute's start
	BuyAt    time.Time `json:"buy_at,omitzero"`    // the buys were placed
	ClosedAt time.Time `json:"closed_at,omitzero"` // the target sells filled
}

func (t *nearTrade) active() bool {
	return t.Phase == "buying" || t.Phase == "holding" || t.Phase == "selling"
}

// nearOrder is an order's state as the trade needs it.
type nearOrder struct {
	Done   bool // no longer working
	Filled int64
	Avg    float64
	Status string
}

// nearBroker is one account the trade buys on.
type nearBroker interface {
	Name() string // fyers, zerodha
	// Cap is the ₹ to spend on one trade now.
	Cap(ctx context.Context) (float64, error)
	// MaxLots caps a trade's lots; 0 = as many as Cap buys.
	MaxLots() int64
	// Ticker is the broker's symbol and lot size of a leg of the day's contracts.
	Ticker(ctx context.Context, x hmaIndex, st *paperDayState, leg paperLeg) (string, int64, error)
	Place(ctx context.Context, ticker, side, typ string, qty int64, price float64) (string, error)
	Order(ctx context.Context, ticker, id string) (nearOrder, error)
	// Held is the intraday quantity held long of ticker.
	Held(ctx context.Context, ticker string) (int64, error)
	// Open is the size of every position open in ticker, long or short, in
	// any product.
	Open(ctx context.Context, ticker string) (int64, error)
}

// nearRun is one index's day for the trade.
type nearRun struct {
	idx   hmaIndex
	st    *paperDayState
	nears []paper.Near
}

type nearDeps struct {
	brokers []nearBroker
	// prices is the last traded price of each Kite EXCHANGE:SYMBOL.
	prices func(ctx context.Context, keys []string) (map[string]float64, error)
	notify func(ctx context.Context, t *nearTrade, msg string)
	sleep  func(ctx context.Context, d time.Duration) error
	now    func() time.Time
}

// nearTargetPrice is 2% over the fill, rounded up to the 5-paise tick.
func nearTargetPrice(entry float64) float64 {
	return math.Round(math.Ceil(entry*(1+nearTargetPct)/0.05-1e-6)*0.05*100) / 100
}

// nearLots is how many lots cap buys at price.
func nearLots(cap, price float64, lot int64) int64 {
	if price <= 0 || lot <= 0 || cap <= 0 {
		return 0
	}
	return int64(math.Floor(cap * nearFundsUse / (price * float64(lot))))
}

// nearChunks splits qty into orders of at most freeze, each whole lots.
func nearChunks(qty, lot, freeze int64) []int64 {
	step := qty
	if freeze > 0 && lot > 0 {
		step = max(lot, freeze/lot*lot)
	}
	var out []int64
	for left := qty; left > 0; left -= min(left, step) {
		out = append(out, min(left, step))
	}
	return out
}

type nearBook struct {
	path   string
	trades []nearTrade
}

func (b *nearBook) save() error { return writeJSONFile(b.path, b.trades) }

// hmaNearTrade advances the day's near-HMA trades by one run: opens a trade
// on each broker for each fresh near alert while that broker has fewer than
// two of the index today, then watches every open trade for about half a
// minute: buys' fills, the target, and the target sells' fills.
func hmaNearTrade(ctx context.Context, dep nearDeps, runs []nearRun, dir string) error {
	var books []*nearBook
	var errs []error
	for _, r := range runs {
		for _, b := range dep.brokers {
			bk := &nearBook{path: filepath.Join(dir, fmt.Sprintf("%s-%s-%s.json", r.st.Day, r.idx.Name, b.Name()))}
			if _, err := readJSONFile(bk.path, &bk.trades); err != nil {
				return err
			}
			books = append(books, bk)
			if err := nearOpen(ctx, dep, b, bk, r); err != nil {
				errs = append(errs, fmt.Errorf("%s %s: %w", r.idx.Name, b.Name(), err))
			}
		}
	}
	if err := nearWatchAll(ctx, dep, books); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// nearOpen buys, on b, each fresh near alert of r not yet traded there,
// while b has fewer than nearPerDay trades of the index today, in a
// contract b hasn't traded today and holds no position in.
func nearOpen(ctx context.Context, dep nearDeps, b nearBroker, bk *nearBook, r nearRun) error {
	now := dep.now()
	for _, n := range r.nears {
		if nearCount(bk.trades) >= nearPerDay {
			return nil
		}
		id := "near|" + hmaCrossID(n.Cross)
		if now.Sub(n.Time.Add(time.Minute)) > nearMaxAge || minuteOfDay(n.Time) >= nearLastEntry || slicesHasNear(bk.trades, id) {
			continue
		}
		leg := r.st.legOf(n.Cross.Instrument)
		if leg == nil {
			return fmt.Errorf("no watched leg for %s", n.Cross.Instrument.Key())
		}
		quote := string(r.idx.Exchange) + ":" + leg.Symbol
		if nearEntered(bk.trades, quote) {
			fmt.Printf("%s %s: %s already entered today; near alert not bought\n", b.Name(), r.idx.Name, leg.Symbol)
			continue
		}
		t := nearTrade{Day: r.st.Day, Index: r.idx.Name, Broker: b.Name(), Near: id, Cross: hmaCrossID(n.Cross),
			Right: n.Cross.Instrument.Right, Quote: quote, Freeze: r.idx.Freeze, Phase: "failed", SignalAt: n.Time}
		// Every failure from here on still uses up one of the day's trades.
		fail := func(why string) error {
			bk.trades = append(bk.trades, t)
			dep.notify(ctx, &bk.trades[len(bk.trades)-1], fmt.Sprintf(":rotating_light: %s %s near-HMA auto buy on %s: %s; %d of %d trades today used",
				paper.HMACrossName, r.idx.Name, brokerTitle(b.Name()), why, nearCount(bk.trades), nearPerDay))
			return bk.save()
		}
		ticker, lot, err := b.Ticker(ctx, r.idx, r.st, *leg)
		if err != nil {
			return fail(fmt.Sprintf("no ticker for %s: %v", leg.Symbol, err))
		}
		t.Ticker, t.Lot = ticker, lot
		// Another strategy's (or a manual) position in the contract: not
		// bought, and not counted. Unknown positions aren't bought either.
		open, err := b.Open(ctx, ticker)
		if err != nil || open != 0 {
			why := fmt.Sprintf("%s already has a position of %d in %s", brokerTitle(b.Name()), open, ticker)
			if err != nil {
				why = fmt.Sprintf("couldn't check %s positions: %v", brokerTitle(b.Name()), err)
			}
			t.Phase = "skipped"
			bk.trades = append(bk.trades, t)
			dep.notify(ctx, &bk.trades[len(bk.trades)-1], fmt.Sprintf(":no_entry: %s %s near-HMA auto buy skipped: %s; %d of %d trades today used",
				paper.HMACrossName, r.idx.Name, why, nearCount(bk.trades), nearPerDay))
			if err := bk.save(); err != nil {
				return err
			}
			continue
		}
		cap, err := b.Cap(ctx)
		if err != nil {
			return fail(fmt.Sprintf("funds: %v", err))
		}
		px, err := dep.prices(ctx, []string{t.Quote})
		if err != nil || px[t.Quote] <= 0 {
			return fail(fmt.Sprintf("no price for %s: %v", t.Quote, err))
		}
		lots := nearLots(cap, px[t.Quote], lot)
		if m := b.MaxLots(); m > 0 {
			lots = min(lots, m)
		}
		if lots <= 0 {
			return fail(fmt.Sprintf("₹%.0f buys no lot of %s at ₹%.2f (lot %d)", cap, ticker, px[t.Quote], lot))
		}
		t.Qty, t.Phase, t.BuyAt = lots*lot, "buying", dep.now()
		bk.trades = append(bk.trades, t)
		tp := &bk.trades[len(bk.trades)-1]
		// Saved before the orders, so a crash can't buy twice.
		if err := bk.save(); err != nil {
			return err
		}
		fmt.Printf("%s %s: buying %d %s at market (cap ₹%.0f, LTP ₹%.2f)\n", b.Name(), r.idx.Name, tp.Qty, ticker, cap, px[t.Quote])
		for _, q := range nearChunks(tp.Qty, lot, tp.Freeze) {
			oid, err := b.Place(ctx, ticker, "BUY", "MARKET", q, 0)
			if err != nil {
				dep.notify(ctx, tp, fmt.Sprintf(":rotating_light: %s auto buy of %d %s on %s failed: %v", r.idx.Name, q, ticker, brokerTitle(b.Name()), err))
				break
			}
			tp.BuyIDs = append(tp.BuyIDs, oid)
			if err := bk.save(); err != nil {
				return err
			}
		}
		if len(tp.BuyIDs) == 0 {
			tp.Phase = "failed"
			if err := bk.save(); err != nil {
				return err
			}
		}
	}
	return nil
}

// nearCount is the trades that count toward nearPerDay: all but skipped.
func nearCount(ts []nearTrade) int {
	n := 0
	for _, t := range ts {
		if t.Phase != "skipped" {
			n++
		}
	}
	return n
}

// nearEntered says whether a trade other than a skipped one was opened in
// the contract quote (Kite's EXCHANGE:SYMBOL).
func nearEntered(ts []nearTrade, quote string) bool {
	for _, t := range ts {
		if t.Quote == quote && t.Phase != "skipped" {
			return true
		}
	}
	return false
}

func slicesHasNear(ts []nearTrade, id string) bool {
	for _, t := range ts {
		if t.Near == id {
			return true
		}
	}
	return false
}

// nearWatchAll polls every active trade of books every few seconds for
// about nearWatch: buys filling, prices reaching targets, sells filling.
func nearWatchAll(ctx context.Context, dep nearDeps, books []*nearBook) error {
	brokers := map[string]nearBroker{}
	for _, b := range dep.brokers {
		brokers[b.Name()] = b
	}
	deadline := dep.now().Add(nearWatch)
	var errs []error
	for {
		var keys []string
		seen := map[string]bool{}
		for _, bk := range books {
			for i := range bk.trades {
				if t := &bk.trades[i]; t.Phase == "holding" && !seen[t.Quote] {
					keys, seen[t.Quote] = append(keys, t.Quote), true
				}
			}
		}
		var px map[string]float64
		if len(keys) > 0 {
			var err error
			if px, err = dep.prices(ctx, keys); err != nil {
				errs = append(errs, fmt.Errorf("prices: %w", err))
			}
		}
		active := false
		for _, bk := range books {
			for i := range bk.trades {
				t := &bk.trades[i]
				b := brokers[t.Broker]
				if b == nil || !t.active() {
					continue
				}
				if err := nearStep(ctx, dep, b, bk, t, px); err != nil {
					errs = append(errs, fmt.Errorf("%s %s %s: %w", t.Index, t.Broker, t.Ticker, err))
				}
				if err := bk.save(); err != nil {
					return err
				}
				active = active || t.active()
			}
		}
		if !active || !dep.now().Add(nearPollEvery).Before(deadline) {
			return errors.Join(errs...)
		}
		if err := dep.sleep(ctx, nearPollEvery); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
}

// nearOrders is the combined state of orders ids: done when all are.
func nearOrders(ctx context.Context, b nearBroker, ticker string, ids []string) (nearOrder, error) {
	all := nearOrder{Done: true}
	var amt float64
	for _, id := range ids {
		o, err := b.Order(ctx, ticker, id)
		if err != nil {
			return nearOrder{}, err
		}
		all.Done = all.Done && o.Done
		all.Filled += o.Filled
		amt += o.Avg * float64(o.Filled)
		if o.Status != "" {
			all.Status = o.Status
		}
	}
	if all.Filled > 0 {
		all.Avg = amt / float64(all.Filled)
	}
	return all, nil
}

// nearStep moves one trade on by what is known now.
func nearStep(ctx context.Context, dep nearDeps, b nearBroker, bk *nearBook, t *nearTrade, px map[string]float64) error {
	who := fmt.Sprintf("%s %s", t.Index, brokerTitle(t.Broker))
	switch t.Phase {
	case "buying":
		if len(t.BuyIDs) == 0 {
			// Saved, then the process stopped before any order.
			t.Phase = "failed"
			dep.notify(ctx, t, fmt.Sprintf(":rotating_light: %s auto buy of %s: no order was placed; not retried", who, t.Ticker))
			return nil
		}
		o, err := nearOrders(ctx, b, t.Ticker, t.BuyIDs)
		if err != nil || !o.Done {
			return err
		}
		if o.Filled <= 0 {
			t.Phase = "failed"
			dep.notify(ctx, t, fmt.Sprintf(":rotating_light: %s auto buy of %d %s %s; nothing bought", who, t.Qty, t.Ticker, o.Status))
			return nil
		}
		t.Qty, t.Entry, t.Target, t.Phase = o.Filled, o.Avg, nearTargetPrice(o.Avg), "holding"
		dep.notify(ctx, t, fmt.Sprintf(":white_check_mark: %s auto buy: BUY %d %s filled @ ₹%.2f; target ₹%.2f (+2%%) is watched here and sold by limit when reached — no stop loss",
			who, t.Qty, t.Ticker, t.Entry, t.Target))
	case "holding":
		p := px[t.Quote]
		if p <= 0 || p < t.Target {
			return nil
		}
		held, err := b.Held(ctx, t.Ticker)
		if err != nil {
			return fmt.Errorf("positions: %w", err)
		}
		// Every unsold trade of this ticker on this broker is still in held.
		var need int64
		for _, u := range bk.trades {
			if u.Ticker == t.Ticker && (u.Phase == "holding" || u.Phase == "selling") {
				need += u.Qty - u.Sold
			}
		}
		left := t.Qty - t.Sold
		if held < need {
			t.Phase, t.ClosedAt = "done", dep.now()
			dep.notify(ctx, t, fmt.Sprintf(":warning: %s: %s reached the ₹%.2f target but %d is held of the %d the auto trades bought; nothing sold, sell it yourself",
				who, t.Ticker, t.Target, held, need))
			return nil
		}
		t.SellIDs = nil
		for _, q := range nearChunks(left, t.Lot, t.Freeze) {
			id, err := b.Place(ctx, t.Ticker, "SELL", "LIMIT", q, t.Target)
			if err != nil {
				dep.notify(ctx, t, fmt.Sprintf(":rotating_light: %s target sell of %d %s @ ₹%.2f failed: %v; retrying", who, q, t.Ticker, t.Target, err))
				break
			}
			t.SellIDs = append(t.SellIDs, id)
		}
		if len(t.SellIDs) > 0 {
			t.Phase = "selling"
		}
	case "selling":
		o, err := nearOrders(ctx, b, t.Ticker, t.SellIDs)
		if err != nil || !o.Done {
			return err
		}
		t.Sold += o.Filled
		t.SoldAmt += o.Avg * float64(o.Filled)
		t.SellIDs = nil
		if t.Sold < t.Qty {
			t.Phase = "holding"
			dep.notify(ctx, t, fmt.Sprintf(":warning: %s target sell of %s %s; %d of %d sold, watching the target again", who, t.Ticker, o.Status, t.Sold, t.Qty))
			return nil
		}
		t.Phase, t.ClosedAt = "done", dep.now()
		exit := t.SoldAmt / float64(t.Sold)
		dep.notify(ctx, t, fmt.Sprintf(":moneybag: %s near-HMA target hit: SELL %d %s filled @ ₹%.2f (bought ₹%.2f, ₹%+.0f before charges)",
			who, t.Qty, t.Ticker, exit, t.Entry, t.SoldAmt-t.Entry*float64(t.Qty)))
	}
	return nil
}

func brokerTitle(name string) string {
	switch name {
	case "fyers":
		return "Fyers"
	case "zerodha":
		return "Zerodha"
	}
	return name
}

func minuteOfDay(t time.Time) int {
	t = t.In(market.IST)
	return t.Hour()*60 + t.Minute()
}

// nearLiveDeps wires the trade to the real brokers, Kite quotes and Slack.
func nearLiveDeps(kc *kite.Client, brokers []string, client *slack.Client, channel, tsPath string) nearDeps {
	dep := nearDeps{
		prices: func(ctx context.Context, keys []string) (map[string]float64, error) { return kiteLast(ctx, kc, keys) },
		notify: func(ctx context.Context, t *nearTrade, msg string) {
			fmt.Println(msg)
			if client == nil {
				return
			}
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
	}
	for _, name := range brokers {
		switch name {
		case "fyers":
			dep.brokers = append(dep.brokers, &fyersNear{f: &lazyFyers{c: &clients{at: map[string]time.Time{}}}})
		case "zerodha":
			dep.brokers = append(dep.brokers, &kiteNear{kc: kc})
		}
	}
	return dep
}

func kiteLast(ctx context.Context, kc *kite.Client, keys []string) (map[string]float64, error) {
	q, err := kc.Quotes(ctx, keys)
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for k, v := range q {
		if v.Last > 0 {
			out[k] = v.Last
		}
	}
	return out, nil
}

// fyersNear trades on Fyers, INTRADAY, one lot a trade.
type fyersNear struct {
	f      *lazyFyers
	master map[string][]fyers.MasterRow
}

func (*fyersNear) Name() string { return "fyers" }

func (b *fyersNear) Cap(ctx context.Context) (float64, error) { return b.f.Funds(ctx) }

func (*fyersNear) MaxLots() int64 { return nearMaxLots }

func (b *fyersNear) Ticker(ctx context.Context, x hmaIndex, st *paperDayState, leg paperLeg) (string, int64, error) {
	if b.master == nil {
		b.master = map[string][]fyers.MasterRow{}
	}
	rows, ok := b.master[x.FyersSeg]
	if !ok {
		var err error
		if rows, err = fyers.New("", "", nil).SymbolMaster(ctx, x.FyersSeg); err != nil {
			return "", 0, err
		}
		b.master[x.FyersSeg] = rows
	}
	k := st.legStrike(leg)
	for _, r := range rows {
		if r.Underlying == x.Name && r.Expiry == st.Expiry && r.Strike == k && r.Right == string(leg.Right) {
			return r.Ticker, r.LotSize, nil
		}
	}
	return "", 0, fmt.Errorf("no Fyers %s %v %s for %s", x.Name, k, leg.Right, st.Expiry)
}

func (b *fyersNear) Place(ctx context.Context, ticker, side, typ string, qty int64, price float64) (string, error) {
	return b.f.PlaceOrder(ctx, fyers.OrderRequest{Symbol: ticker, Side: side, Type: typ, Product: "INTRADAY", Qty: qty, Price: price, Tag: nearTradeTag})
}

func (b *fyersNear) Order(ctx context.Context, _, id string) (nearOrder, error) {
	o, err := b.f.Order(ctx, id)
	if err != nil {
		return nearOrder{}, err
	}
	out := nearOrder{Done: o.Done(), Status: o.StatusText()}
	if o.Status == 2 {
		out.Filled = o.FilledQty
		if out.Filled <= 0 {
			out.Filled = o.Qty
		}
		out.Avg = o.TradedPrice
	} else if o.Done() && o.FilledQty > 0 {
		out.Filled, out.Avg = o.FilledQty, o.TradedPrice
	}
	return out, nil
}

func (b *fyersNear) Open(ctx context.Context, ticker string) (int64, error) {
	ps, err := b.f.Positions(ctx)
	if err != nil {
		return 0, err
	}
	var open int64
	for _, p := range ps {
		if p.Symbol == ticker {
			open += abs64(p.NetQty)
		}
	}
	return open, nil
}

func (b *fyersNear) Held(ctx context.Context, ticker string) (int64, error) {
	ps, err := b.f.Positions(ctx)
	if err != nil {
		return 0, err
	}
	var held int64
	for _, p := range ps {
		if p.Symbol == ticker && p.ProductType == "INTRADAY" {
			held += p.NetQty
		}
	}
	return held, nil
}

// kiteBroker is the part of Kite kiteNear uses.
type kiteBroker interface {
	EquityFunds(ctx context.Context) (float64, error)
	PlaceOrder(ctx context.Context, o kite.OrderRequest) (string, error)
	OrderStatus(ctx context.Context, id string) (kite.Order, error)
	Positions(ctx context.Context) ([]kite.Position, error)
}

// kiteNear trades on Zerodha, MIS, one lot a trade. Its tickers
// are Kite's EXCHANGE:SYMBOL.
type kiteNear struct{ kc kiteBroker }

func (*kiteNear) Name() string { return "zerodha" }

func (b *kiteNear) Cap(ctx context.Context) (float64, error) {
	f, err := b.kc.EquityFunds(ctx)
	if err != nil {
		return 0, err
	}
	return f, nil
}

func (*kiteNear) MaxLots() int64 { return nearMaxLots }

func (b *kiteNear) Ticker(_ context.Context, x hmaIndex, st *paperDayState, leg paperLeg) (string, int64, error) {
	if leg.Symbol == "" {
		return "", 0, fmt.Errorf("no %v %s leg", st.legStrike(leg), leg.Right)
	}
	return string(x.Exchange) + ":" + leg.Symbol, st.Lot, nil
}

func (b *kiteNear) Open(ctx context.Context, ticker string) (int64, error) {
	exch, sym, _ := strings.Cut(ticker, ":")
	ps, err := b.kc.Positions(ctx)
	if err != nil {
		return 0, err
	}
	var open int64
	for _, p := range ps {
		if p.Exchange == exch && p.Symbol == sym {
			open += abs64(p.Qty)
		}
	}
	return open, nil
}

func (b *kiteNear) Place(ctx context.Context, ticker, side, typ string, qty int64, price float64) (string, error) {
	exch, sym, _ := strings.Cut(ticker, ":")
	return b.kc.PlaceOrder(ctx, kite.OrderRequest{Exchange: exch, Symbol: sym, Side: side, Type: typ, Product: "MIS",
		Qty: qty, Price: price, Tag: nearTradeTag})
}

func (b *kiteNear) Order(ctx context.Context, _, id string) (nearOrder, error) {
	o, err := b.kc.OrderStatus(ctx, id)
	if err != nil {
		return nearOrder{}, err
	}
	st := strings.ToUpper(o.Status)
	out := nearOrder{Filled: o.Filled, Avg: o.Average, Status: strings.ToLower(strings.TrimSpace(o.Status + " " + o.StatusMessage))}
	out.Done = st == "COMPLETE" || st == "REJECTED" || st == "CANCELLED"
	return out, nil
}

func (b *kiteNear) Held(ctx context.Context, ticker string) (int64, error) {
	exch, sym, _ := strings.Cut(ticker, ":")
	ps, err := b.kc.Positions(ctx)
	if err != nil {
		return 0, err
	}
	var held int64
	for _, p := range ps {
		if p.Exchange == exch && p.Symbol == sym && p.Product == "MIS" {
			held += p.Qty
		}
	}
	return held, nil
}

// hmaNearSizes prints what each broker would buy of each index's legs now,
// with live funds and prices. It places nothing.
func hmaNearSizes(ctx context.Context, d market.Date, now time.Time, dir string) error {
	kc, err := kiteClient(ctx)
	if err != nil {
		return err
	}
	dep := nearLiveDeps(kc, []string{"fyers", "zerodha"}, nil, "", "")
	for _, x := range hmaIndices {
		st, err := paperDay(ctx, kc, x, d, now, dir)
		if err != nil || st == nil {
			fmt.Println(x.Name, "no contracts yet:", err)
			continue
		}
		for _, b := range dep.brokers {
			cap, err := b.Cap(ctx)
			if err != nil {
				fmt.Printf("%s %s: funds: %v\n", x.Name, b.Name(), err)
				continue
			}
			for _, l := range st.Legs {
				ticker, lot, err := b.Ticker(ctx, x, st, l)
				if err != nil {
					fmt.Printf("%s %s %s: %v\n", x.Name, b.Name(), l.Right, err)
					continue
				}
				key := string(x.Exchange) + ":" + l.Symbol
				px, err := dep.prices(ctx, []string{key})
				if err != nil {
					return err
				}
				lots := nearLots(cap, px[key], lot)
				if m := b.MaxLots(); m > 0 {
					lots = min(lots, m)
				}
				fmt.Printf("%s %-7s %s: cap ₹%.0f, LTP ₹%.2f, lot %d → %d lots = %d qty in %v\n",
					x.Name, b.Name(), ticker, cap, px[key], lot, lots, lots*lot, nearChunks(lots*lot, lot, x.Freeze))
			}
		}
	}
	return nil
}
