package fyers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"serene/internal/auth"
)

// Trader is the Fyers API v3 account and order calls: funds, positions, the
// tradebook and orderbook, placing and cancelling orders.
type Trader struct {
	AppID   string
	Token   auth.Secret
	BaseURL string // default https://api-t1.fyers.in/api/v3
	HTTP    *http.Client
}

// NewTrader returns a Trader on the day's login.
func NewTrader(appID string, token auth.Secret) *Trader {
	return &Trader{AppID: appID, Token: token, BaseURL: "https://api-t1.fyers.in/api/v3",
		HTTP: &http.Client{Timeout: 20 * time.Second}}
}

// call sends one request and decodes the reply into out, which must embed
// envelope's fields s, code and message.
func (t *Trader) call(ctx context.Context, method, path string, q url.Values, body, out any) error {
	u := t.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", t.AppID+":"+t.Token.Reveal())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("fyers %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("fyers %s: %w", path, err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("fyers %s: HTTP %d", path, resp.StatusCode)
	}
	if env.S != "ok" {
		switch env.Code {
		case -8, -15, -16, -17:
			return ErrAuth
		}
		msg := env.Message
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("fyers %s: %s (code %d)", path, msg, env.Code)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("fyers %s: %w", path, err)
		}
	}
	return nil
}

// Funds is the account's Available Balance.
func (t *Trader) Funds(ctx context.Context) (float64, error) {
	var r struct {
		FundLimit []struct {
			ID           int     `json:"id"`
			Title        string  `json:"title"`
			EquityAmount float64 `json:"equityAmount"`
		} `json:"fund_limit"`
	}
	if err := t.call(ctx, http.MethodGet, "/funds", nil, nil, &r); err != nil {
		return 0, err
	}
	for _, f := range r.FundLimit {
		if strings.EqualFold(f.Title, "Available Balance") || f.ID == 10 {
			return f.EquityAmount, nil
		}
	}
	return 0, fmt.Errorf("fyers funds: no Available Balance")
}

// Position is one of netPositions.
type Position struct {
	Symbol      string  `json:"symbol"` // e.g. NSE:NIFTY25N2525400CE
	NetQty      int64   `json:"netQty"`
	NetAvg      float64 `json:"netAvg"`
	BuyQty      int64   `json:"buyQty"`
	BuyAvg      float64 `json:"buyAvg"`
	SellQty     int64   `json:"sellQty"`
	SellAvg     float64 `json:"sellAvg"`
	CFBuyQty    int64   `json:"cfBuyQty"`
	CFSellQty   int64   `json:"cfSellQty"`
	LTP         float64 `json:"ltp"`
	PL          float64 `json:"pl"`
	ProductType string  `json:"productType"`
	Segment     int     `json:"segment"`  // 11 equity derivatives, 20 commodity
	Exchange    int     `json:"exchange"` // 10 NSE, 11 MCX, 12 BSE
}

// Positions are today's net positions.
func (t *Trader) Positions(ctx context.Context) ([]Position, error) {
	var r struct {
		NetPositions []Position `json:"netPositions"`
	}
	if err := t.call(ctx, http.MethodGet, "/positions", nil, nil, &r); err != nil {
		return nil, err
	}
	return r.NetPositions, nil
}

// Trade is one tradebook fill.
type Trade struct {
	OrderNumber   string  `json:"orderNumber"`
	Symbol        string  `json:"symbol"`
	TradedQty     int64   `json:"tradedQty"`
	TradePrice    float64 `json:"tradePrice"`
	Side          int     `json:"side"` // 1 buy, -1 sell
	ProductType   string  `json:"productType"`
	OrderDateTime string  `json:"orderDateTime"` // 07-Aug-2020 13:51:12, IST
	Segment       int     `json:"segment"`
	Exchange      int     `json:"exchange"`
}

// Trades is today's tradebook.
func (t *Trader) Trades(ctx context.Context) ([]Trade, error) {
	var r struct {
		TradeBook []Trade `json:"tradeBook"`
	}
	if err := t.call(ctx, http.MethodGet, "/tradebook", nil, nil, &r); err != nil {
		return nil, err
	}
	return r.TradeBook, nil
}

// Order is one of the orderbook's orders.
type Order struct {
	ID          string  `json:"id"`
	Symbol      string  `json:"symbol"`
	Qty         int64   `json:"qty"`
	FilledQty   int64   `json:"filledQty"`
	Remaining   int64   `json:"remainingQuantity"`
	Status      int     `json:"status"` // 1 cancelled, 2 traded, 4 transit, 5 rejected, 6 pending, 7 expired
	Side        int     `json:"side"`
	LimitPrice  float64 `json:"limitPrice"`
	TradedPrice float64 `json:"tradedPrice"`
	ProductType string  `json:"productType"`
	OrderTag    string  `json:"orderTag"` // "1:serenemanual"; "…:Untagged" with none
	Message     string  `json:"message"`
	Segment     int     `json:"segment"`
	Exchange    int     `json:"exchange"`
}

// Done is whether the order has stopped working.
func (o Order) Done() bool {
	switch o.Status {
	case 1, 2, 5, 7:
		return true
	}
	return false
}

// Tag is the order's tag without Fyers' "1:" prefix; empty when untagged.
func (o Order) Tag() string {
	_, tag, ok := strings.Cut(o.OrderTag, ":")
	if !ok {
		tag = o.OrderTag
	}
	if tag == "Untagged" {
		return ""
	}
	return tag
}

// StatusText names the order's status.
func (o Order) StatusText() string {
	s := map[int]string{1: "cancelled", 2: "traded", 4: "transit", 5: "rejected", 6: "pending", 7: "expired"}[o.Status]
	return strings.TrimSpace(s + " " + o.Message)
}

// Orders is today's orderbook.
func (t *Trader) Orders(ctx context.Context) ([]Order, error) {
	var r struct {
		OrderBook []Order `json:"orderBook"`
	}
	if err := t.call(ctx, http.MethodGet, "/orders", nil, nil, &r); err != nil {
		return nil, err
	}
	return r.OrderBook, nil
}

// Order is one order by its Fyers id.
func (t *Trader) Order(ctx context.Context, id string) (Order, error) {
	var r struct {
		OrderBook []Order `json:"orderBook"`
	}
	if err := t.call(ctx, http.MethodGet, "/orders", url.Values{"id": {id}}, nil, &r); err != nil {
		return Order{}, err
	}
	for _, o := range r.OrderBook {
		if o.ID == id {
			return o, nil
		}
	}
	return Order{}, fmt.Errorf("fyers has no order %s", id)
}

// OrderRequest is an order to place.
type OrderRequest struct {
	Symbol  string
	Side    string // BUY, SELL
	Type    string // LIMIT, MARKET
	Product string // INTRADAY, MARGIN, CNC
	Qty     int64
	Price   float64 // LIMIT only
	Tag     string  // 1–30 letters or digits
}

// PlaceOrder places a regular DAY order and returns its id.
func (t *Trader) PlaceOrder(ctx context.Context, o OrderRequest) (string, error) {
	side := map[string]int{"BUY": 1, "SELL": -1}[o.Side]
	typ := map[string]int{"LIMIT": 1, "MARKET": 2}[o.Type]
	if side == 0 || typ == 0 || o.Qty <= 0 {
		return "", fmt.Errorf("fyers: bad order %+v", o)
	}
	price := 0.0
	if typ == 1 {
		price = o.Price
	}
	body := map[string]any{"symbol": o.Symbol, "qty": o.Qty, "type": typ, "side": side, "productType": o.Product,
		"limitPrice": price, "stopPrice": 0, "validity": "DAY", "disclosedQty": 0, "offlineOrder": false,
		"stopLoss": 0, "takeProfit": 0}
	if o.Tag != "" {
		body["orderTag"] = o.Tag
	}
	var r struct {
		ID string `json:"id"`
	}
	if err := t.call(ctx, http.MethodPost, "/orders/sync", nil, body, &r); err != nil {
		return "", err
	}
	if r.ID == "" {
		return "", fmt.Errorf("fyers placed an order without an id")
	}
	return r.ID, nil
}

// CancelOrder cancels a pending order.
func (t *Trader) CancelOrder(ctx context.Context, id string) error {
	return t.call(ctx, http.MethodDelete, "/orders/sync", nil, map[string]string{"id": id}, nil)
}
