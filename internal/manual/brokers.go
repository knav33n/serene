package manual

import (
	"context"
	"fmt"
	"strings"
	"time"

	"serene/internal/fyers"
	"serene/internal/kite"
	"serene/internal/market"
	"serene/internal/upstox"
)

// Zerodha is the Kite account, trading MIS.
type Zerodha struct{ C *kite.Client }

func (z *Zerodha) Name() string    { return "zerodha" }
func (z *Zerodha) Product() string { return "MIS" }

func (z *Zerodha) Symbol(c Contract) (string, error) {
	if c.KiteSymbol == "" {
		return "", fmt.Errorf("no Kite symbol for %s", Label(c))
	}
	return c.KiteSymbol, nil
}

func (z *Zerodha) Funds(ctx context.Context) (float64, error) { return z.C.EquityFunds(ctx) }

var kiteFO = map[string]bool{"NFO": true, "BFO": true, "MCX": true}

func (z *Zerodha) Positions(ctx context.Context) ([]Position, error) {
	ps, err := z.C.Positions(ctx)
	if err != nil {
		return nil, err
	}
	var out []Position
	for _, p := range ps {
		if !kiteFO[p.Exchange] || (p.Qty == 0 && p.DayBuyQty == 0 && p.DaySellQty == 0 && p.PnL == 0) {
			continue
		}
		out = append(out, Position{Symbol: p.Symbol, Name: p.Symbol, Exchange: p.Exchange, Product: p.Product,
			Qty: p.Qty, Avg: p.Average, LTP: p.Last, PnL: round2(p.PnL), Bought: p.DayBuyQty, BuyAvg: round2(p.DayBuyPrice),
			Sold: p.DaySellQty, SellAvg: round2(p.DaySellPrice), Carried: p.Overnight})
	}
	return out, nil
}

func clock(ts string) string {
	if i := strings.LastIndexByte(ts, ' '); i >= 0 {
		return ts[i+1:]
	}
	return ts
}

func (z *Zerodha) Fills(ctx context.Context) ([]Fill, error) {
	ts, err := z.C.Trades(ctx)
	if err != nil {
		return nil, err
	}
	var out []Fill
	for _, t := range ts {
		if !kiteFO[t.Exchange] {
			continue
		}
		when := t.FillTime
		if when == "" {
			when = t.ExchangeTime
		}
		out = append(out, Fill{OrderID: t.OrderID, Time: clock(when), Symbol: t.Symbol, Exchange: t.Exchange,
			Product: t.Product, Side: t.Side, Qty: t.Qty, Price: t.Average})
	}
	return ByOrder(out), nil
}

// Charges is Kite's contract note for the fills' orders.
func (z *Zerodha) Charges(ctx context.Context, fills []Fill) (map[string]float64, error) {
	want := map[string]bool{}
	for _, f := range fills {
		want[f.OrderID] = true
	}
	os, err := z.C.Orders(ctx)
	if err != nil {
		return nil, err
	}
	var done []kite.Order
	for _, o := range os {
		if want[o.ID] && o.Filled > 0 {
			done = append(done, o)
		}
	}
	out := map[string]float64{}
	if len(done) == 0 {
		return out, nil
	}
	totals, err := z.C.OrderCharges(ctx, done)
	if err != nil {
		return nil, err
	}
	for i, o := range done {
		out[o.ID] = totals[i]
	}
	return out, nil
}

func kiteDone(status string) bool {
	switch strings.ToUpper(status) {
	case "COMPLETE", "REJECTED", "CANCELLED":
		return true
	}
	return false
}

func (z *Zerodha) Orders(ctx context.Context) ([]Order, error) {
	os, err := z.C.Orders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Order, len(os))
	for i, o := range os {
		out[i] = Order{ID: o.ID, Symbol: o.Symbol, Exchange: o.Exchange, Side: o.Side, Tag: o.Tag,
			Open: !kiteDone(o.Status), Price: o.Price, Pending: o.Pending}
	}
	return out, nil
}

func (z *Zerodha) Place(ctx context.Context, o OrderReq) (string, error) {
	return z.C.PlaceOrder(ctx, kite.OrderRequest{Exchange: o.Exchange, Symbol: o.Symbol, Side: o.Side, Type: o.Type,
		Product: o.Product, Qty: o.Qty, Price: o.Price, Tag: Tag})
}

func (z *Zerodha) Cancel(ctx context.Context, id string) error { return z.C.CancelOrder(ctx, id) }

func (z *Zerodha) Order(ctx context.Context, id string) (OrderState, error) {
	o, err := z.C.OrderStatus(ctx, id)
	if err != nil {
		return OrderState{}, err
	}
	return OrderState{Done: kiteDone(o.Status), Filled: o.Filled, Average: o.Average,
		Status: strings.TrimSpace(strings.ToLower(o.Status) + " " + o.StatusMessage)}, nil
}

// Upstox is the Upstox account, trading I (intraday). Its orders name a
// contract by instrument key.
type Upstox struct{ C *upstox.Client }

func (u *Upstox) Name() string    { return "upstox" }
func (u *Upstox) Product() string { return "I" }

func (u *Upstox) Symbol(c Contract) (string, error) {
	if c.UpstoxKey == "" {
		return "", fmt.Errorf("no Upstox key for %s", Label(c))
	}
	return c.UpstoxKey, nil
}

func (u *Upstox) Funds(ctx context.Context) (float64, error) { return u.C.EquityFunds(ctx) }

func upstoxFO(key string) bool {
	seg, _, _ := strings.Cut(key, "|")
	return seg == "NSE_FO" || seg == "BSE_FO" || seg == "MCX_FO"
}

func (u *Upstox) Positions(ctx context.Context) ([]Position, error) {
	ps, err := u.C.Positions(ctx)
	if err != nil {
		return nil, err
	}
	var out []Position
	for _, p := range ps {
		if !upstoxFO(p.Instrument) || (p.Qty == 0 && p.DayBuyQty == 0 && p.DaySellQty == 0 && p.PnL == 0) {
			continue
		}
		out = append(out, Position{Symbol: p.Instrument, Name: p.Symbol, Exchange: p.Exchange, Product: p.Product,
			Qty: p.Qty, Avg: p.Average, LTP: p.Last, PnL: round2(p.PnL), Bought: p.DayBuyQty, BuyAvg: round2(p.DayBuyPrice),
			Sold: p.DaySellQty, SellAvg: round2(p.DaySellPrice), Carried: p.Overnight})
	}
	return out, nil
}

func secs(hms string) (int, bool) {
	t, err := time.Parse("15:04:05", hms)
	if err != nil {
		return 0, false
	}
	return t.Hour()*3600 + t.Minute()*60 + t.Second(), true
}

// UpstoxFillTime is a tradebook fill's time of day, IST. Upstox sends
// exchange_timestamp 5:30 ahead (an IST time read as UTC and put in IST
// again), so it's taken back 5:30 unless that would put the fill before its
// order went in: then it was IST already. With none, the order's time.
func UpstoxFillTime(exchangeTS, orderTS string) string {
	filled, placed := clock(exchangeTS), clock(orderTS)
	f, ok := secs(filled)
	if !ok {
		return placed
	}
	back := ((f-(5*3600+30*60))%86400 + 86400) % 86400
	if p, ok := secs(placed); ok && back < p {
		return filled
	}
	return fmt.Sprintf("%02d:%02d:%02d", back/3600, back%3600/60, back%60)
}

func (u *Upstox) Fills(ctx context.Context) ([]Fill, error) {
	ts, err := u.C.Trades(ctx)
	if err != nil {
		return nil, err
	}
	var out []Fill
	for _, t := range ts {
		if !upstoxFO(t.Instrument) {
			continue
		}
		out = append(out, Fill{OrderID: t.OrderID, Time: UpstoxFillTime(t.ExchangeTime, t.OrderTime), Symbol: t.Instrument,
			Exchange: t.Exchange, Product: t.Product, Side: strings.ToUpper(t.Side), Qty: t.Qty, Price: t.Average})
	}
	return ByOrder(out), nil
}

// Charges is Upstox's brokerage calculator, one call an order.
func (u *Upstox) Charges(ctx context.Context, fills []Fill) (map[string]float64, error) {
	out := map[string]float64{}
	for _, f := range fills {
		c, err := u.C.Charges(ctx, f.Symbol, f.Product, f.Side, f.Qty, f.Price)
		if err != nil {
			return out, err
		}
		out[f.OrderID] = c
	}
	return out, nil
}

func upstoxDone(status string) bool {
	switch strings.ToLower(status) {
	case "complete", "rejected", "cancelled":
		return true
	}
	return false
}

func (u *Upstox) Orders(ctx context.Context) ([]Order, error) {
	os, err := u.C.Orders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Order, len(os))
	for i, o := range os {
		out[i] = Order{ID: o.ID, Symbol: o.Instrument, Exchange: o.Exchange, Side: strings.ToUpper(o.Side), Tag: o.Tag,
			Open: !upstoxDone(o.Status), Price: o.Price, Pending: o.Pending}
	}
	return out, nil
}

func (u *Upstox) Place(ctx context.Context, o OrderReq) (string, error) {
	return u.C.PlaceOrder(ctx, upstox.OrderRequest{Instrument: o.Symbol, Side: o.Side, Type: o.Type, Product: o.Product,
		Qty: o.Qty, Price: o.Price, Tag: Tag})
}

func (u *Upstox) Cancel(ctx context.Context, id string) error { return u.C.CancelOrder(ctx, id) }

func (u *Upstox) Order(ctx context.Context, id string) (OrderState, error) {
	o, err := u.C.OrderStatus(ctx, id)
	if err != nil {
		return OrderState{}, err
	}
	return OrderState{Done: upstoxDone(o.Status), Filled: o.Filled, Average: o.Average,
		Status: strings.TrimSpace(strings.ToLower(o.Status) + " " + o.StatusMessage)}, nil
}

// NiftyContracts joins Upstox's live Nifty options (instrument keys, freeze
// quantity) with Kite's (trading symbols) and Fyers' (tickers). Kite or Fyers
// may be missing: that broker's buys then fail on the symbol, the rest work.
func NiftyContracts(ups []upstox.Contract, kites []kite.Instrument, fys []fyers.MasterRow) ([]Contract, error) {
	type k struct {
		e      string
		strike float64
		right  string
	}
	sym := map[k]kite.Instrument{}
	for _, in := range kites {
		if in.Name == "NIFTY" && in.Exchange == "NFO" && (in.Type == "CE" || in.Type == "PE") {
			sym[k{in.Expiry.String(), in.Strike, in.Type}] = in
		}
	}
	fy := map[k]fyers.MasterRow{}
	for _, r := range fys {
		if r.Underlying == "NIFTY" && (r.Right == "CE" || r.Right == "PE") && strings.HasPrefix(r.Ticker, "NSE:") {
			fy[k{r.Expiry.String(), r.Strike, r.Right}] = r
		}
	}
	var out []Contract
	for _, c := range ups {
		if c.Type != "CE" && c.Type != "PE" {
			continue
		}
		e, err := market.ParseDate(c.Expiry)
		if err != nil {
			return nil, err
		}
		ct := Contract{Expiry: e, Strike: c.Strike, Right: c.Type, LotSize: c.LotSize, FreezeQty: int64(c.FreezeQty),
			Tick: c.TickSize / 100, UpstoxKey: c.Key}
		if in, ok := sym[k{c.Expiry, c.Strike, c.Type}]; ok {
			if in.LotSize != c.LotSize {
				return nil, fmt.Errorf("%s: Kite's lot is %d, Upstox's %d", Label(ct), in.LotSize, c.LotSize)
			}
			ct.KiteSymbol = in.Symbol
			if in.TickSize > 0 {
				ct.Tick = in.TickSize
			}
		}
		if r, ok := fy[k{c.Expiry, c.Strike, c.Type}]; ok {
			if r.LotSize != c.LotSize {
				return nil, fmt.Errorf("%s: Fyers' lot is %d, Upstox's %d", Label(ct), r.LotSize, c.LotSize)
			}
			ct.FyersSymbol = r.Ticker
		}
		out = append(out, ct)
	}
	return out, nil
}
