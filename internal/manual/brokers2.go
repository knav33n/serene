package manual

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"serene/internal/breeze"
	"serene/internal/fyers"
	"serene/internal/market"
)

// Fyers is the Fyers account, trading INTRADAY. Its orders name a contract
// by its symbol-master ticker, e.g. NSE:NIFTY25N2525400CE.
type Fyers struct{ C *fyers.Trader }

func (f *Fyers) Name() string    { return "fyers" }
func (f *Fyers) Product() string { return "INTRADAY" }

func (f *Fyers) Symbol(c Contract) (string, error) {
	if c.FyersSymbol == "" {
		return "", fmt.Errorf("no Fyers symbol for %s", Label(c))
	}
	return c.FyersSymbol, nil
}

func (f *Fyers) Funds(ctx context.Context) (float64, error) { return f.C.Funds(ctx) }

// fyersExchange is the order exchange of a Fyers exchange and segment.
func fyersExchange(exchange, segment int) string {
	switch {
	case exchange == 10 && segment == 11:
		return "NFO"
	case exchange == 12 && segment == 11:
		return "BFO"
	case exchange == 11:
		return "MCX"
	}
	return ""
}

func fyersName(sym string) string {
	if _, s, ok := strings.Cut(sym, ":"); ok {
		return s
	}
	return sym
}

func (f *Fyers) Positions(ctx context.Context) ([]Position, error) {
	ps, err := f.C.Positions(ctx)
	if err != nil {
		return nil, err
	}
	var out []Position
	for _, p := range ps {
		ex := fyersExchange(p.Exchange, p.Segment)
		if ex == "" || (p.NetQty == 0 && p.BuyQty == 0 && p.SellQty == 0 && p.PL == 0) {
			continue
		}
		out = append(out, Position{Symbol: p.Symbol, Name: fyersName(p.Symbol), Exchange: ex, Product: p.ProductType,
			Qty: p.NetQty, Avg: p.NetAvg, LTP: p.LTP, PnL: round2(p.PL), Bought: p.BuyQty, BuyAvg: round2(p.BuyAvg),
			Sold: p.SellQty, SellAvg: round2(p.SellAvg), Carried: p.CFBuyQty - p.CFSellQty})
	}
	return out, nil
}

func (f *Fyers) Fills(ctx context.Context) ([]Fill, error) {
	ts, err := f.C.Trades(ctx)
	if err != nil {
		return nil, err
	}
	var out []Fill
	for _, t := range ts {
		ex := fyersExchange(t.Exchange, t.Segment)
		if ex == "" {
			continue
		}
		side := "BUY"
		if t.Side < 0 {
			side = "SELL"
		}
		out = append(out, Fill{OrderID: t.OrderNumber, Time: clock(t.OrderDateTime), Symbol: t.Symbol, Exchange: ex,
			Product: t.ProductType, Side: side, Qty: t.TradedQty, Price: t.TradePrice})
	}
	return ByOrder(out), nil
}

// Charges: Fyers has no charges API, so the page shows its orders unpriced.
func (f *Fyers) Charges(context.Context, []Fill) (map[string]float64, error) {
	return map[string]float64{}, nil
}

func (f *Fyers) Orders(ctx context.Context) ([]Order, error) {
	os, err := f.C.Orders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Order, len(os))
	for i, o := range os {
		side := "BUY"
		if o.Side < 0 {
			side = "SELL"
		}
		out[i] = Order{ID: o.ID, Symbol: o.Symbol, Side: side, Tag: o.Tag(), Open: !o.Done(), Price: o.LimitPrice,
			Pending: o.Remaining}
	}
	return out, nil
}

func (f *Fyers) Place(ctx context.Context, o OrderReq) (string, error) {
	return f.C.PlaceOrder(ctx, fyers.OrderRequest{Symbol: o.Symbol, Side: o.Side, Type: o.Type, Product: o.Product,
		Qty: o.Qty, Price: o.Price, Tag: Tag})
}

func (f *Fyers) Cancel(ctx context.Context, id string) error { return f.C.CancelOrder(ctx, id) }

func (f *Fyers) Order(ctx context.Context, id string) (OrderState, error) {
	o, err := f.C.Order(ctx, id)
	if err != nil {
		return OrderState{}, err
	}
	return OrderState{Done: o.Done(), Filled: o.FilledQty, Average: o.TradedPrice, Status: o.StatusText()}, nil
}

// ICICI is the ICICI Direct account on Breeze, trading the "options"
// product. Breeze names an option by its parts, so serene's symbol for one is
// NIFTY|2026-11-24|25400|CE. Breeze takes no market orders: a MARKET order
// goes in as a LIMIT MarketPad through the LTP (above it to buy, below to
// sell), which fills at the best price there is.
type ICICI struct {
	C   *breeze.Client
	LTP func(ctx context.Context, c breeze.Contract) (float64, error)
}

// MarketPad is how far through the LTP an ICICI "market" order's limit goes.
const MarketPad = 0.05

func (i *ICICI) Name() string    { return "icici" }
func (i *ICICI) Product() string { return "options" }

// ICICISymbol is serene's symbol for a Breeze contract.
func ICICISymbol(c breeze.Contract) string {
	return fmt.Sprintf("%s|%s|%g|%s", c.Stock, c.Expiry, c.Strike, c.Right)
}

// ParseICICISymbol reads ICICISymbol back.
func ParseICICISymbol(s string) (breeze.Contract, error) {
	parts := strings.Split(s, "|")
	if len(parts) != 4 {
		return breeze.Contract{}, fmt.Errorf("not an ICICI symbol: %q", s)
	}
	e, err := market.ParseDate(parts[1])
	if err != nil {
		return breeze.Contract{}, err
	}
	k, err := strconv.ParseFloat(parts[2], 64)
	if err != nil {
		return breeze.Contract{}, err
	}
	return breeze.Contract{Stock: parts[0], Expiry: e, Strike: k, Right: parts[3]}, nil
}

func iciciName(c breeze.Contract) string {
	return fmt.Sprintf("%s %s %g %s", c.Stock, c.Expiry, c.Strike, c.Right)
}

func (i *ICICI) Symbol(c Contract) (string, error) {
	return ICICISymbol(breeze.Contract{Stock: "NIFTY", Expiry: c.Expiry, Strike: c.Strike, Right: c.Right}), nil
}

func (i *ICICI) Funds(ctx context.Context) (float64, error) { return i.C.Funds(ctx) }

func (i *ICICI) Positions(ctx context.Context) ([]Position, error) {
	ps, err := i.C.Positions(ctx)
	if err != nil {
		return nil, err
	}
	var out []Position
	for _, p := range ps {
		ex := p.Exchange
		if ex == "" {
			ex = "NFO"
		}
		out = append(out, Position{Symbol: ICICISymbol(p.Contract), Name: iciciName(p.Contract), Exchange: ex,
			Product: p.Product, Qty: p.Qty, Avg: p.Avg, LTP: p.LTP, PnL: round2(p.PnL)})
	}
	return out, nil
}

func (i *ICICI) Fills(ctx context.Context) ([]Fill, error) {
	ts, err := i.C.Trades(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Fill, 0, len(ts))
	for _, t := range ts {
		out = append(out, Fill{OrderID: t.OrderID, Symbol: ICICISymbol(t.Contract), Exchange: "NFO", Product: t.Product,
			Side: t.Side, Qty: t.Qty, Price: t.Price})
	}
	return ByOrder(out), nil
}

// Charges are the trade list's brokerage and taxes for each order.
func (i *ICICI) Charges(ctx context.Context, fills []Fill) (map[string]float64, error) {
	ts, err := i.C.Trades(ctx)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, f := range fills {
		want[f.OrderID] = true
	}
	out := map[string]float64{}
	for _, t := range ts {
		if want[t.OrderID] {
			out[t.OrderID] += t.Charges
		}
	}
	return out, nil
}

func (i *ICICI) Orders(ctx context.Context) ([]Order, error) {
	os, err := i.C.Orders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Order, len(os))
	for n, o := range os {
		out[n] = Order{ID: o.ID, Symbol: ICICISymbol(o.Contract), Exchange: "NFO", Side: o.Side, Tag: o.Remark,
			Open: !o.Done(), Price: o.Price, Pending: o.Pending}
	}
	return out, nil
}

// Marketable is a LIMIT price MarketPad through ltp, on the tick: above it to
// buy, below it to sell (at least a tick).
func Marketable(side string, ltp, tick float64) float64 {
	if tick <= 0 {
		tick = 0.05
	}
	var n float64
	if side == "BUY" {
		n = math.Ceil(ltp*(1+MarketPad)/tick - 1e-9)
	} else {
		n = math.Max(1, math.Floor(ltp*(1-MarketPad)/tick+1e-9))
	}
	return math.Round(n*tick*100) / 100
}

func (i *ICICI) Place(ctx context.Context, o OrderReq) (string, error) {
	c, err := ParseICICISymbol(o.Symbol)
	if err != nil {
		return "", err
	}
	price := o.Price
	if o.Type == "MARKET" {
		if i.LTP == nil {
			return "", fmt.Errorf("ICICI takes no market orders and there's no LTP for %s: give a limit", iciciName(c))
		}
		ltp, err := i.LTP(ctx, c)
		if err != nil {
			return "", fmt.Errorf("the LTP for ICICI's limit: %w", err)
		}
		if ltp <= 0 {
			return "", fmt.Errorf("no LTP for %s", iciciName(c))
		}
		price = Marketable(o.Side, ltp, 0.05)
	}
	return i.C.PlaceOrder(ctx, breeze.OrderRequest{Contract: c, Side: o.Side, Qty: o.Qty, Price: price, Remark: Tag})
}

func (i *ICICI) Cancel(ctx context.Context, id string) error { return i.C.CancelOrder(ctx, id) }

func (i *ICICI) Order(ctx context.Context, id string) (OrderState, error) {
	o, err := i.C.Order(ctx, id)
	if err != nil {
		return OrderState{}, err
	}
	return OrderState{Done: o.Done(), Filled: o.Filled(), Average: o.Average, Status: strings.ToLower(o.Status)}, nil
}
