package live

import (
	"context"
	"fmt"
	"strings"

	"serene/internal/kite"
	"serene/internal/market"
	"serene/internal/upstox"
)

// Zerodha trades through Kite. Symbols maps instrument keys to Kite trading
// symbols. Exchange and Product default to NFO and MIS (intraday options);
// MCX futures held overnight use MCX and NRML.
type Zerodha struct {
	C                 *kite.Client
	Symbols           map[string]string
	Exchange, Product string
}

func (z *Zerodha) exchange() string {
	if z.Exchange == "" {
		return "NFO"
	}
	return z.Exchange
}

func (z *Zerodha) product() string {
	if z.Product == "" {
		return "MIS"
	}
	return z.Product
}

func (z *Zerodha) Name() string { return "zerodha" }

func (z *Zerodha) Funds(ctx context.Context) (market.Paise, error) {
	f, err := z.C.EquityFunds(ctx)
	return market.Rupees(f), err
}

func (z *Zerodha) req(o OrderReq) (kite.OrderRequest, error) {
	sym, ok := z.Symbols[o.Instrument.Key()]
	if !ok {
		return kite.OrderRequest{}, fmt.Errorf("no Kite symbol for %s", o.Instrument.Key())
	}
	return kite.OrderRequest{Exchange: z.exchange(), Symbol: sym, Side: sideName(o.Side), Type: o.Type, Product: z.product(),
		Qty: o.Qty, Price: o.Price.Rupees(), Trigger: o.Trigger.Rupees(), Tag: o.Tag}, nil
}

func (z *Zerodha) Place(ctx context.Context, o OrderReq) (string, error) {
	r, err := z.req(o)
	if err != nil {
		return "", err
	}
	return z.C.PlaceOrder(ctx, r)
}

func (z *Zerodha) Modify(ctx context.Context, id string, o OrderReq) error {
	r, err := z.req(o)
	if err != nil {
		return err
	}
	return z.C.ModifyOrder(ctx, id, r)
}

func (z *Zerodha) Cancel(ctx context.Context, id string) error { return z.C.CancelOrder(ctx, id) }

func kiteState(o kite.Order) OrderState {
	st := Open
	switch strings.ToUpper(o.Status) {
	case "COMPLETE":
		st = Complete
	case "CANCELLED":
		st = Cancelled
	case "REJECTED":
		st = Rejected
	}
	return OrderState{ID: o.ID, Tag: o.Tag, Status: st, Filled: o.Filled, Average: market.Rupees(o.Average), Message: o.StatusMessage}
}

func (z *Zerodha) Order(ctx context.Context, id string) (OrderState, error) {
	o, err := z.C.OrderStatus(ctx, id)
	return kiteState(o), err
}

func (z *Zerodha) ByTag(ctx context.Context, tag string) (OrderState, bool, error) {
	os, err := z.C.Orders(ctx)
	if err != nil {
		return OrderState{}, false, err
	}
	for _, o := range os {
		if o.Tag == tag {
			return kiteState(o), true, nil
		}
	}
	return OrderState{}, false, nil
}

func (z *Zerodha) Position(ctx context.Context, in market.Instrument) (int64, error) {
	ps, err := z.C.Positions(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, p := range ps {
		if p.Exchange == z.exchange() && p.Symbol == z.Symbols[in.Key()] && p.Product == z.product() {
			n += p.Qty
		}
	}
	return n, nil
}

// Upstox trades intraday (product I). Keys maps instrument keys to Upstox
// instrument tokens such as NSE_FO|43919.
type Upstox struct {
	C    *upstox.Client
	Keys map[string]string
}

func (u *Upstox) Name() string { return "upstox" }

func (u *Upstox) Funds(ctx context.Context) (market.Paise, error) {
	f, err := u.C.EquityFunds(ctx)
	return market.Rupees(f), err
}

func (u *Upstox) req(o OrderReq) (upstox.OrderRequest, error) {
	key, ok := u.Keys[o.Instrument.Key()]
	if !ok {
		return upstox.OrderRequest{}, fmt.Errorf("no Upstox key for %s", o.Instrument.Key())
	}
	return upstox.OrderRequest{Instrument: key, Side: sideName(o.Side), Type: o.Type, Product: "I",
		Qty: o.Qty, Price: o.Price.Rupees(), Trigger: o.Trigger.Rupees(), Tag: o.Tag}, nil
}

func (u *Upstox) Place(ctx context.Context, o OrderReq) (string, error) {
	r, err := u.req(o)
	if err != nil {
		return "", err
	}
	return u.C.PlaceOrder(ctx, r)
}

func (u *Upstox) Modify(ctx context.Context, id string, o OrderReq) error {
	r, err := u.req(o)
	if err != nil {
		return err
	}
	return u.C.ModifyOrder(ctx, id, r)
}

func (u *Upstox) Cancel(ctx context.Context, id string) error { return u.C.CancelOrder(ctx, id) }

func upstoxState(o upstox.Order) OrderState {
	st := Open
	switch strings.ToLower(o.Status) {
	case "complete":
		st = Complete
	case "cancelled", "cancelled after market order":
		st = Cancelled
	case "rejected":
		st = Rejected
	}
	return OrderState{ID: o.ID, Tag: o.Tag, Status: st, Filled: o.Filled, Average: market.Rupees(o.Average), Message: o.StatusMessage}
}

func (u *Upstox) Order(ctx context.Context, id string) (OrderState, error) {
	o, err := u.C.OrderStatus(ctx, id)
	return upstoxState(o), err
}

func (u *Upstox) ByTag(ctx context.Context, tag string) (OrderState, bool, error) {
	os, err := u.C.Orders(ctx)
	if err != nil {
		return OrderState{}, false, err
	}
	for _, o := range os {
		if o.Tag == tag {
			return upstoxState(o), true, nil
		}
	}
	return OrderState{}, false, nil
}

func (u *Upstox) Position(ctx context.Context, in market.Instrument) (int64, error) {
	ps, err := u.C.Positions(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, p := range ps {
		if p.Instrument == u.Keys[in.Key()] && p.Product == "I" {
			n += p.Qty
		}
	}
	return n, nil
}

func sideName(s market.Side) string {
	if s == market.Sell {
		return "SELL"
	}
	return "BUY"
}
