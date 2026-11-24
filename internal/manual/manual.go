// Package manual is the manual trades desk, moved from project_calm: a Nifty
// option bought by hand on Zerodha and / or Upstox with a target and the
// capital to spend, every F&O position on both accounts with an Exit, and
// today's trades as each broker has them.
//
// A buy is a LIMIT order at the price typed (at market with none), sized to
// the capital or the account's free margin when that's less, split at the
// freeze quantity (at most MaxOrders orders). On an account with more than
// SplitAbove free, half the spend buys the strike typed (taken as ATM) and half
// the strike ITMDepth in the money (CE lower, PE higher), each leg with its own
// orders and target; with that ITM strike missing or unpriced it's ATM only.
// What hasn't filled in LimitWait
// is cancelled. The target is the price that nets the target percentage after
// the broker's charges both ways; nothing rests at the broker for it until the
// LTP reaches it: the desk watches (Watch) and then places a LIMIT sell at the
// target. There's no stop:
// what's still held at 15:20 is squared off by the desk (SquareOff), and the
// product is intraday (MIS / I), so the broker squares off anything the desk
// misses.
//
// A manual trade is a position whose orders today carry Tag; its targets are
// kept in Targets' file so a restart keeps watching them.
package manual

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"serene/internal/cost"
	"serene/internal/market"
)

// Tag marks the desk's orders: Kite takes up to 20 alphanumeric characters.
const Tag = "serenemanual"

// MaxOrders is the most orders one buy goes out as: Kite takes 10 a second.
const MaxOrders = 10

// SplitAbove is the free margin above which a buy is split half ATM, half ITM.
const SplitAbove = 500000

// ITMDepth is how far in the money the split's second leg is, in points.
const ITMDepth = 100

// Hours manual buys are taken, IST: what's left at the close hour is squared off.
var (
	OpenAt  = [2]int{9, 15}
	CloseAt = [2]int{15, 20}
)

// Names are the brokers' labels.
var Names = map[string]string{"zerodha": "Zerodha", "upstox": "Upstox", "fyers": "Fyers", "icici": "ICICI"}

// Contract is one live Nifty option.
type Contract struct {
	Expiry      market.Date
	Strike      float64
	Right       string // CE, PE
	LotSize     int64
	FreezeQty   int64   // the most one order may carry; 0 when unknown
	Tick        float64 // rupees
	UpstoxKey   string
	KiteSymbol  string
	FyersSymbol string
}

// Position is a broker's F&O position: held now or traded today.
type Position struct {
	Symbol   string  `json:"symbol"` // what the broker's orders name: Kite's trading symbol, Upstox's instrument key
	Name     string  `json:"name"`   // the trading symbol
	Exchange string  `json:"exchange"`
	Product  string  `json:"product"`
	Qty      int64   `json:"qty"` // open now, negative short
	Avg      float64 `json:"avg"`
	LTP      float64 `json:"ltp"`
	PnL      float64 `json:"pnl"` // the broker's, before charges
	Bought   int64   `json:"bought"`
	BuyAvg   float64 `json:"buy_avg"`
	Sold     int64   `json:"sold"`
	SellAvg  float64 `json:"sell_avg"`
	Carried  int64   `json:"carried"`
}

// Fill is one order's fills today, summed.
type Fill struct {
	OrderID  string   `json:"order_id"`
	Time     string   `json:"time"` // HH:MM:SS IST, the first fill's
	Symbol   string   `json:"-"`
	Exchange string   `json:"-"`
	Product  string   `json:"-"`
	Side     string   `json:"side"`
	Qty      int64    `json:"qty"`
	Price    float64  `json:"price"`
	Charges  *float64 `json:"charges"`
}

// Order is one of today's orders.
type Order struct {
	ID, Symbol, Exchange, Side, Tag string
	Open                            bool // still working
	Price                           float64
	Pending                         int64
}

// OrderReq is an order to place.
type OrderReq struct {
	Symbol, Exchange, Product string
	Side                      string // BUY, SELL
	Type                      string // LIMIT, MARKET
	Qty                       int64
	Price                     float64
}

// OrderState is an order as the broker has it now.
type OrderState struct {
	Done    bool // complete, rejected or cancelled
	Filled  int64
	Average float64
	Status  string
}

// Broker is one account.
type Broker interface {
	Name() string
	Funds(ctx context.Context) (float64, error)
	Positions(ctx context.Context) ([]Position, error) // F&O only
	Fills(ctx context.Context) ([]Fill, error)         // F&O only, one per order
	Charges(ctx context.Context, fills []Fill) (map[string]float64, error)
	Orders(ctx context.Context) ([]Order, error)
	Place(ctx context.Context, o OrderReq) (string, error)
	Cancel(ctx context.Context, id string) error
	Order(ctx context.Context, id string) (OrderState, error)
	// Symbol is what the broker's orders name c by; Product its intraday product.
	Symbol(c Contract) (string, error)
	Product() string
}

// Desk runs the page's actions.
type Desk struct {
	Broker    func(ctx context.Context, name string) (Broker, error)
	Contracts func(ctx context.Context) ([]Contract, error)
	LTP       func(ctx context.Context, c Contract) (float64, error)
	Costs     *cost.Schedule
	Notify    func(ctx context.Context, text string)
	Now       func() time.Time
	LimitWait time.Duration // how long a buy's LIMIT waits before what's left is cancelled
	Targets   *Targets      // the targets the desk watches (Watch), placing a LIMIT sell at each once reached
	Poll      time.Duration
	Ledger    *Ledger // each day's manual nets and its one trade; nil: no limit, no card

	mu sync.Mutex // one order action at a time
}

func (d *Desk) now() time.Time {
	if d.Now != nil {
		return d.Now().In(market.IST)
	}
	return time.Now().In(market.IST)
}

func (d *Desk) say(ctx context.Context, format string, a ...any) {
	if d.Notify != nil {
		d.Notify(ctx, fmt.Sprintf(format, a...))
	}
}

func before(t time.Time, hm [2]int) bool { return t.Hour()*60+t.Minute() < hm[0]*60+hm[1] }

// Expiries are Nifty's expiries still to trade, nearest first: today's until 15:20.
func Expiries(cs []Contract, now time.Time) []market.Date {
	first := market.DateOf(now)
	if !before(now, CloseAt) {
		first = first.AddDays(1)
	}
	seen := map[market.Date]bool{}
	var out []market.Date
	for _, c := range cs {
		if !c.Expiry.Before(first) && !seen[c.Expiry] {
			seen[c.Expiry] = true
			out = append(out, c.Expiry)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// OrderSizes splits qty into orders within the freeze quantity, whole lots
// each, the full ones first.
func OrderSizes(qty, lot, freeze int64) []int64 {
	most := int64(0)
	if freeze > 0 && lot > 0 {
		most = freeze / lot * lot
	}
	if most <= 0 || qty <= most {
		return []int64{qty}
	}
	var out []int64
	for ; qty > most; qty -= most {
		out = append(out, most)
	}
	if qty > 0 {
		out = append(out, qty)
	}
	return out
}

func (d *Desk) fill(c Contract, side market.Side, qty int64, price float64, at time.Time) market.Fill {
	in := market.Option(market.NFO, "NIFTY", c.Expiry, market.Rupees(c.Strike), market.Right(c.Right), c.LotSize)
	return market.Fill{Instrument: in, Side: side, Qty: qty, Price: market.Rupees(price), Time: at, Kind: market.Traded}
}

// charges is the schedule's charges for qty at price, split into its orders.
func (d *Desk) charges(c Contract, side market.Side, qty int64, price float64, at time.Time) (float64, error) {
	total := market.Paise(0)
	for _, q := range OrderSizes(qty, c.LotSize, c.FreezeQty) {
		ch, err := d.Costs.Charges(d.fill(c, side, q, price, at))
		if err != nil {
			return 0, err
		}
		total += ch
	}
	return total.Rupees(), nil
}

// Lots is how many lots spend buys at price with the buy's charges, at most
// MaxOrders orders of the freeze quantity.
func (d *Desk) Lots(c Contract, spend, price float64, at time.Time) (int64, error) {
	if price <= 0 || c.LotSize <= 0 {
		return 0, nil
	}
	n := int64(spend / (price * float64(c.LotSize)))
	if c.FreezeQty > 0 {
		n = min(n, MaxOrders*(c.FreezeQty/c.LotSize))
	}
	for ; n > 0; n-- {
		ch, err := d.charges(c, market.Buy, n*c.LotSize, price, at)
		if err != nil {
			return 0, err
		}
		if price*float64(n*c.LotSize)+ch <= spend {
			return n, nil
		}
	}
	return 0, nil
}

// Target is the sell price, rounded up to the tick, at which selling qty
// bought at entry nets pct % of entry × qty after the broker's charges both
// ways (each of the orders it goes out as). It's sold with a LIMIT at that
// price, so no slippage is added.
func (d *Desk) Target(c Contract, entry float64, qty int64, pct float64, at time.Time) (float64, error) {
	tick := c.Tick
	if tick <= 0 {
		tick = 0.05
	}
	in, err := d.charges(c, market.Buy, qty, entry, at)
	if err != nil {
		return 0, err
	}
	want := pct / 100 * entry * float64(qty)
	for i := int64(math.Ceil(entry/tick - 1e-9)); ; i++ {
		sell := math.Round(float64(i)*tick*100) / 100
		out, err := d.charges(c, market.Sell, qty, sell, at)
		if err != nil {
			return 0, err
		}
		if (sell-entry)*float64(qty)-in-out >= want-1e-6 {
			return sell, nil
		}
		if sell > entry*10+1000 {
			return 0, errors.New("no target price found")
		}
	}
}

// BuyReq is the page's buy.
type BuyReq struct {
	Expiry  string   `json:"expiry"`
	Strike  float64  `json:"strike"`
	Right   string   `json:"option_type"`
	Limit   *float64 `json:"limit"` // nil: at market
	Target  float64  `json:"target_pct"`
	Capital float64  `json:"capital"`
	Brokers []string `json:"brokers"`
}

// Bought is one broker's buy.
type Bought struct {
	Broker   string  `json:"broker"`
	Contract string  `json:"contract"`
	Qty      int64   `json:"qty"`
	Entry    float64 `json:"entry"`
	Target   float64 `json:"target"`
}

// BuyResult is what a buy did on each broker.
type BuyResult struct {
	Trades   []Bought `json:"trades"`
	Errors   []string `json:"errors"`   // the brokers it didn't buy on, and why
	Warnings []string `json:"warnings"` // less than the capital covers, or a target not placed
}

// Label names a contract the way the page shows it.
func Label(c Contract) string {
	return fmt.Sprintf("NIFTY %s %g %s", c.Expiry, c.Strike, c.Right)
}

func (d *Desk) contract(ctx context.Context, expiry string, strike float64, right string) (Contract, error) {
	e, err := market.ParseDate(expiry)
	if err != nil {
		return Contract{}, err
	}
	cs, err := d.Contracts(ctx)
	if err != nil {
		return Contract{}, err
	}
	for _, c := range cs {
		if c.Expiry == e && c.Strike == strike && c.Right == right {
			return c, nil
		}
	}
	return Contract{}, fmt.Errorf("no live Nifty %s %g %s (strikes are 50 apart)", expiry, strike, right)
}

// Buy buys on each broker named at once.
func (d *Desk) Buy(ctx context.Context, r BuyReq) (BuyResult, error) {
	if r.Target <= 0 || r.Capital <= 0 {
		return BuyResult{}, errors.New("the target and the capital must be above 0")
	}
	if r.Limit != nil && !(*r.Limit > 0) {
		return BuyResult{}, errors.New("the limit price must be above 0, or empty for market")
	}
	if r.Right != "CE" && r.Right != "PE" {
		return BuyResult{}, errors.New("CE or PE")
	}
	var names []string
	for _, b := range r.Brokers {
		if _, ok := Names[b]; !ok {
			return BuyResult{}, fmt.Errorf("no broker %q: zerodha / upstox / fyers / icici", b)
		}
		if !contains(names, b) {
			names = append(names, b)
		}
	}
	if len(names) == 0 {
		return BuyResult{}, errors.New("pick a broker")
	}
	now := d.now()
	if !market.NSETradingDay(market.DateOf(now)) || before(now, OpenAt) || !before(now, CloseAt) {
		return BuyResult{}, fmt.Errorf("manual trades are taken from %d:%02d to %d:%02d on a trading day (squared off at %d:%02d)",
			OpenAt[0], OpenAt[1], CloseAt[0], CloseAt[1], CloseAt[0], CloseAt[1])
	}
	c, err := d.contract(ctx, r.Expiry, r.Strike, r.Right)
	if err != nil {
		return BuyResult{}, err
	}
	price := 0.0
	if r.Limit == nil {
		if price, err = d.LTP(ctx, c); err != nil {
			return BuyResult{}, fmt.Errorf("the LTP to size a market buy: %w", err)
		}
		if price <= 0 {
			return BuyResult{}, errors.New("no LTP to size a market buy: give a limit price")
		}
	} else {
		price = *r.Limit
		tick := c.Tick
		if tick > 0 && math.Abs(price/tick-math.Round(price/tick)) > 1e-6 {
			return BuyResult{}, fmt.Errorf("the limit price %g isn't a multiple of the tick, %g", price, tick)
		}
	}

	atm := leg{c: c, price: price}
	if r.Limit != nil {
		atm.typ, atm.limit = "LIMIT", *r.Limit
	}
	itm, itmWhy := d.itmLeg(ctx, r, atm)

	d.mu.Lock()
	defer d.mu.Unlock()
	today := market.DateOf(now)
	if d.Ledger != nil && d.Ledger.TradedOn(today) {
		return BuyResult{}, errors.New("today's one manual trade has already been taken")
	}
	type res struct {
		b    []Bought
		warn string
		err  error
	}
	out := make([]res, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, warn, err := d.buyOn(ctx, name, r, atm, itm, itmWhy)
			out[i] = res{b, warn, err}
		}()
	}
	wg.Wait()
	var result BuyResult
	for i, o := range out {
		result.Trades = append(result.Trades, o.b...)
		if o.err != nil {
			result.Errors = append(result.Errors, Names[names[i]]+": "+o.err.Error())
		}
		if o.warn != "" {
			result.Warnings = append(result.Warnings, Names[names[i]]+": "+o.warn)
		}
	}
	if len(result.Trades) == 0 {
		return BuyResult{}, errors.New(strings.Join(result.Errors, "; "))
	}
	if d.Ledger != nil {
		if err := d.Ledger.MarkTraded(today, d.now().Format("15:04:05")); err != nil {
			result.Warnings = append(result.Warnings, "the day's trade wasn't recorded: "+err.Error())
		}
	}
	return result, nil
}

// leg is one contract a buy goes into, at the price it's sized at.
type leg struct {
	c     Contract
	typ   string  // MARKET or LIMIT
	limit float64 // LIMIT's price
	price float64 // what it's sized at: the limit, else the LTP
}

// ITMStrike is the split's in-the-money strike for c, taken as ATM.
func ITMStrike(c Contract) float64 {
	if c.Right == "PE" {
		return c.Strike + ITMDepth
	}
	return c.Strike - ITMDepth
}

// itmLeg is the split's ITM leg for the ATM one, or why there's none. It goes
// at market when the ATM leg does; with a limit typed, its limit sits as far
// from its LTP as the typed one does from the ATM's, rounded to the tick.
func (d *Desk) itmLeg(ctx context.Context, r BuyReq, atm leg) (*leg, string) {
	strike := ITMStrike(atm.c)
	c, err := d.contract(ctx, r.Expiry, strike, r.Right)
	if err != nil {
		return nil, fmt.Sprintf("no ITM %g %s (%v)", strike, r.Right, err)
	}
	ltp, err := d.LTP(ctx, c)
	if err != nil || ltp <= 0 {
		return nil, fmt.Sprintf("no LTP for ITM %s (%v)", Label(c), err)
	}
	l := leg{c: c, typ: "MARKET", price: ltp}
	if atm.typ == "LIMIT" {
		atmLTP, err := d.LTP(ctx, atm.c)
		if err != nil || atmLTP <= 0 {
			return nil, fmt.Sprintf("no LTP for %s to set the ITM limit (%v)", Label(atm.c), err)
		}
		tick := c.Tick
		if tick <= 0 {
			tick = 0.05
		}
		px := math.Round((ltp+atm.limit-atmLTP)/tick) * tick
		px = math.Round(px*100) / 100
		if px <= 0 {
			return nil, fmt.Sprintf("the ITM limit for %s comes out at %.2f", Label(c), px)
		}
		l.typ, l.limit, l.price = "LIMIT", px, px
	}
	return &l, ""
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// buyOn buys on one broker: min(capital, free) into the ATM leg, or split
// half and half with the ITM leg when more than SplitAbove is free.
func (d *Desk) buyOn(ctx context.Context, name string, r BuyReq, atm leg, itm *leg, itmWhy string) ([]Bought, string, error) {
	b, err := d.Broker(ctx, name)
	if err != nil {
		return nil, "", err
	}
	funds, err := b.Funds(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("free margin: %w", err)
	}
	spend := min(r.Capital, funds)
	legs, spends := []leg{atm}, []float64{spend}
	var warns []string
	if funds > SplitAbove {
		switch {
		case itm == nil:
			warns = append(warns, "ATM only: "+itmWhy)
		default:
			if n, err := d.Lots(itm.c, spend/2, itm.price, d.now()); err != nil || n < 1 {
				warns = append(warns, fmt.Sprintf("ATM only: ₹%s doesn't buy a lot of ITM %s at %.2f", inr(spend/2), Label(itm.c), itm.price))
			} else {
				legs, spends = []leg{atm, *itm}, []float64{spend / 2, spend / 2}
			}
		}
	}
	var out []Bought
	var errs []string
	for i, l := range legs {
		bought, warn, err := d.buyLeg(ctx, b, l, spends[i], r.Capital/float64(len(legs)), funds, r)
		if err != nil {
			if len(legs) > 1 {
				err = fmt.Errorf("%s: %w", Label(l.c), err)
			}
			errs = append(errs, err.Error())
			continue
		}
		out = append(out, bought)
		if warn != "" {
			if len(legs) > 1 {
				warn = Label(l.c) + ": " + warn
			}
			warns = append(warns, warn)
		}
	}
	if len(out) == 0 {
		return nil, "", errors.New(strings.Join(errs, "; "))
	}
	if len(errs) > 0 { // one leg of two bought: say why the other didn't
		warns = append(warns, errs...)
	}
	return out, strings.Join(warns, "; "), nil
}

// buyLeg buys l with spend on b and places its target; share is the leg's
// part of the capital, to say when spend buys less than it would.
func (d *Desk) buyLeg(ctx context.Context, b Broker, l leg, spend, share, funds float64, r BuyReq) (Bought, string, error) {
	c, price, typ, limit := l.c, l.price, l.typ, l.limit
	if typ == "" {
		typ = "MARKET"
	}
	name := b.Name()
	sym, err := b.Symbol(c)
	if err != nil {
		return Bought{}, "", err
	}
	now := d.now()
	lots, err := d.Lots(c, spend, price, now)
	if err != nil {
		return Bought{}, "", err
	}
	if lots < 1 {
		return Bought{}, "", fmt.Errorf("₹%s doesn't buy a lot of %d at %.2f (₹%s free)", inr(spend), c.LotSize, price, inr(funds))
	}
	sizes := OrderSizes(lots*c.LotSize, c.LotSize, c.FreezeQty)
	var ids []string
	var placeErr error
	for _, q := range sizes {
		id, err := b.Place(ctx, OrderReq{Symbol: sym, Exchange: "NFO", Product: b.Product(), Side: "BUY", Type: typ, Qty: q, Price: limit})
		if err != nil {
			placeErr = err
			break
		}
		ids = append(ids, id)
	}
	wait := d.LimitWait
	if typ == "MARKET" {
		wait = 30 * time.Second
	}
	filled, avg, waitErr := d.await(ctx, b, ids, wait, true)
	if filled == 0 {
		why := errors.Join(placeErr, waitErr)
		if why == nil {
			if typ == "LIMIT" {
				why = fmt.Errorf("the limit buy @ %g didn't fill in %s (cancelled)", limit, d.LimitWait)
			} else {
				why = errors.New("the market buy didn't fill")
			}
		}
		return Bought{}, "", why
	}
	var warns []string
	if placeErr != nil {
		warns = append(warns, "not every order went in: "+placeErr.Error())
	}
	if waitErr != nil {
		warns = append(warns, waitErr.Error())
	}
	if covers, _ := d.Lots(c, share, price, now); filled/c.LotSize < covers {
		warns = append(warns, fmt.Sprintf("%d of the %d lots ₹%s covers", filled/c.LotSize, covers, inr(share)))
	}
	target, err := d.Target(c, avg, filled, r.Target, now)
	if err == nil {
		err = d.targets().Add(Tracked{Day: market.DateOf(now).String(), Broker: name, Symbol: sym, Exchange: "NFO", Product: b.Product(),
			Expiry: c.Expiry.String(), Strike: c.Strike, Right: c.Right, Qty: filled, Entry: round2(avg), Price: target})
	}
	if err != nil {
		warns = append(warns, "the target isn't watched: "+err.Error()+" (Exit squares it off)")
	}
	bought := Bought{Broker: name, Contract: Label(c), Qty: filled, Entry: round2(avg), Target: target}
	d.say(ctx, "manual: bought %d %s on %s @ %.2f · target %.2f (%g%% net) · spend ₹%s of capital ₹%s%s", filled, Label(c), Names[name],
		avg, target, r.Target, inr(spend), inr(r.Capital), warnText(warns))
	return bought, strings.Join(warns, "; "), nil
}

func warnText(w []string) string {
	if len(w) == 0 {
		return ""
	}
	return " · " + strings.Join(w, "; ")
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// inr groups rupees the Indian way: 5,00,000.
func inr(v float64) string {
	n := int64(math.Round(v))
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprint(n)
	if len(s) > 3 {
		head, tail := s[:len(s)-3], s[len(s)-3:]
		var parts []string
		for len(head) > 2 {
			parts = append([]string{head[len(head)-2:]}, parts...)
			head = head[:len(head)-2]
		}
		if head != "" {
			parts = append([]string{head}, parts...)
		}
		s = strings.Join(parts, ",") + "," + tail
	}
	if neg {
		return "-" + s
	}
	return s
}

// await waits for orders to finish, up to wait, then cancels what's still
// working (when cancel) and reads them once more. It returns what filled at
// its average.
func (d *Desk) await(ctx context.Context, b Broker, ids []string, wait time.Duration, cancel bool) (int64, float64, error) {
	poll := d.Poll
	if poll <= 0 {
		poll = time.Second
	}
	states := make([]OrderState, len(ids))
	deadline := time.Now().Add(wait)
	var readErr error
	for {
		done := true
		readErr = nil
		for i, id := range ids {
			if states[i].Done {
				continue
			}
			s, err := b.Order(ctx, id)
			if err != nil {
				readErr = fmt.Errorf("order %s: %w", id, err)
				done = false
				continue
			}
			states[i] = s
			done = done && s.Done
		}
		if done || !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		case <-time.After(poll):
		}
	}
	if cancel {
		for i, id := range ids {
			if states[i].Done {
				continue
			}
			if err := b.Cancel(ctx, id); err != nil {
				readErr = errors.Join(readErr, fmt.Errorf("cancel %s: %w", id, err))
			}
			if s, err := b.Order(ctx, id); err == nil {
				states[i] = s
			}
		}
	}
	var filled int64
	var value float64
	var why []string
	for _, s := range states {
		filled += s.Filled
		value += float64(s.Filled) * s.Average
		if s.Done && s.Filled == 0 && s.Status != "" {
			why = append(why, s.Status)
		}
	}
	if filled == 0 {
		if len(why) > 0 {
			readErr = errors.Join(readErr, errors.New(strings.Join(why, "; ")))
		}
		return 0, 0, readErr
	}
	return filled, value / float64(filled), readErr
}

// ExitResult is a square-off.
type ExitResult struct {
	Symbol string  `json:"symbol"`
	Qty    int64   `json:"qty"`
	Price  float64 `json:"price"`
}

// Exit squares off the broker's position in symbol (its product) at market,
// now: its pending orders on the contract (a resting target would turn it
// short) are cancelled first, and the order is split at the freeze quantity.
func (d *Desk) Exit(ctx context.Context, broker, symbol, exchange, product string) (ExitResult, error) {
	if _, ok := Names[broker]; !ok {
		return ExitResult{}, fmt.Errorf("no broker %q: zerodha / upstox / fyers / icici", broker)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	b, err := d.Broker(ctx, broker)
	if err != nil {
		return ExitResult{}, err
	}
	return d.exit(ctx, b, symbol, exchange, product)
}

func (d *Desk) exit(ctx context.Context, b Broker, symbol, exchange, product string) (ExitResult, error) {
	r, err := d.sell(ctx, b, symbol, exchange, product, 0, true)
	if r.Qty > 0 || errors.Is(err, errNoPosition) {
		d.untrack(b.Name(), symbol)
	}
	return r, err
}

var errNoPosition = errors.New("no position")

// sell closes up to qty (0: all) of the broker's position in symbol (its
// product) at market, split at the freeze quantity; with cancel, its pending
// orders on the contract are cancelled first.
func (d *Desk) sell(ctx context.Context, b Broker, symbol, exchange, product string, qty int64, cancel bool) (ExitResult, error) {
	if cancel {
		orders, err := b.Orders(ctx)
		if err != nil {
			return ExitResult{}, fmt.Errorf("orders: %w", err)
		}
		for _, o := range orders {
			if o.Open && o.Symbol == symbol && (o.Exchange == "" || o.Exchange == exchange) {
				if err := b.Cancel(ctx, o.ID); err != nil {
					return ExitResult{}, fmt.Errorf("cancel order %s: %w", o.ID, err)
				}
			}
		}
	}
	ps, err := b.Positions(ctx)
	if err != nil {
		return ExitResult{}, fmt.Errorf("positions: %w", err)
	}
	var held int64
	name := symbol
	for _, p := range ps {
		if p.Symbol == symbol && p.Exchange == exchange && p.Product == product {
			held += p.Qty
			name = p.Name
		}
	}
	if held == 0 || (qty > 0 && held < 0) {
		return ExitResult{}, fmt.Errorf("%w: no %s (%s) position on %s", errNoPosition, name, product, Names[b.Name()])
	}
	side := "SELL"
	if held < 0 {
		side, held = "BUY", -held
	}
	if qty <= 0 || qty > held {
		qty = held
	}
	lot, freeze := int64(1), int64(0)
	if cs, err := d.Contracts(ctx); err == nil {
		for _, c := range cs {
			if s, err := b.Symbol(c); err == nil && s == symbol {
				lot, freeze = c.LotSize, c.FreezeQty
			}
		}
	}
	var ids []string
	var placeErr error
	for _, q := range OrderSizes(qty, lot, freeze) {
		id, err := b.Place(ctx, OrderReq{Symbol: symbol, Exchange: exchange, Product: product, Side: side, Type: "MARKET", Qty: q})
		if err != nil {
			placeErr = err
			break
		}
		ids = append(ids, id)
	}
	filled, avg, waitErr := d.await(ctx, b, ids, 30*time.Second, false)
	if filled == 0 {
		return ExitResult{}, fmt.Errorf("%s: square-off not filled: %w", name, errors.Join(placeErr, waitErr, errors.New("see the broker")))
	}
	d.say(ctx, "manual: squared off %d %s (%s) on %s @ %.2f", filled, name, product, Names[b.Name()], avg)
	r := ExitResult{Symbol: symbol, Qty: filled, Price: round2(avg)}
	if err := errors.Join(placeErr, waitErr); err != nil || filled < qty {
		return r, fmt.Errorf("squared off %d of %d %s @ %.2f: %v", filled, qty, name, avg, err)
	}
	return r, nil
}

// SquareOff exits every manual position still open on the broker: the
// positions in the desk's product whose orders today carry Tag.
func (d *Desk) SquareOff(ctx context.Context, broker string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	b, err := d.Broker(ctx, broker)
	if err != nil {
		return err
	}
	orders, err := b.Orders(ctx)
	if err != nil {
		return err
	}
	mine := map[string]bool{}
	for _, o := range orders {
		if o.Tag == Tag {
			mine[o.Symbol] = true
		}
	}
	ps, err := b.Positions(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range ps {
		if p.Qty != 0 && p.Product == b.Product() && mine[p.Symbol] {
			if _, err := d.exit(ctx, b, p.Symbol, p.Exchange, p.Product); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
