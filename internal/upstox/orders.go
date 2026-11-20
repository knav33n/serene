package upstox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// HFTURL is the host for placing, modifying and cancelling orders.
const HFTURL = "https://api-hft.upstox.com"

// OrderRequest is an order. Price is for LIMIT and SL, Trigger for SL and
// SL-M, in rupees.
type OrderRequest struct {
	Instrument     string // e.g. NSE_FO|43919
	Side           string // BUY, SELL
	Type           string // LIMIT, SL, SL-M, MARKET
	Product        string // I (intraday), D
	Qty            int64
	Price, Trigger float64
	Tag            string // up to 40 characters
}

// Order is an order's latest state.
type Order struct {
	ID            string  `json:"order_id"`
	Status        string  `json:"status"`
	StatusMessage string  `json:"status_message"`
	Instrument    string  `json:"instrument_token"`
	Side          string  `json:"transaction_type"`
	Type          string  `json:"order_type"`
	Qty           int64   `json:"quantity"`
	Filled        int64   `json:"filled_quantity"`
	Pending       int64   `json:"pending_quantity"`
	Price         float64 `json:"price"`
	Trigger       float64 `json:"trigger_price"`
	Average       float64 `json:"average_price"`
	Tag           string  `json:"tag"`
	Symbol        string  `json:"trading_symbol"`
	Exchange      string  `json:"exchange"`
	Product       string  `json:"product"`
	Placed        string  `json:"order_timestamp"`
}

// send makes one request, never retried: a timeout may still have placed the
// order, so the caller reconciles by tag instead of resending.
func (c *Client) send(ctx context.Context, method, rawURL string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token.Reveal())
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.Requests++
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var env envelope
	jsonErr := json.Unmarshal(b, &env)
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrAuth
	}
	if resp.StatusCode != http.StatusOK {
		if len(env.Errors) > 0 {
			return fmt.Errorf("upstox %s: HTTP %d %s %s", method, resp.StatusCode, env.Errors[0].Code, env.Errors[0].Message)
		}
		return fmt.Errorf("upstox %s: HTTP %d %s", method, resp.StatusCode, b[:min(len(b), 200)])
	}
	if jsonErr != nil {
		return fmt.Errorf("upstox %s: %w", method, jsonErr)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

func (c *Client) hft() string {
	if c.BaseURL != "https://api.upstox.com" {
		return c.BaseURL // tests point both at one server
	}
	return HFTURL
}

type orderBody struct {
	Quantity         int64   `json:"quantity"`
	Product          string  `json:"product,omitempty"`
	Validity         string  `json:"validity"`
	Price            float64 `json:"price"`
	Tag              string  `json:"tag,omitempty"`
	Instrument       string  `json:"instrument_token,omitempty"`
	OrderType        string  `json:"order_type"`
	Side             string  `json:"transaction_type,omitempty"`
	DisclosedQty     int64   `json:"disclosed_quantity"`
	Trigger          float64 `json:"trigger_price"`
	IsAMO            bool    `json:"is_amo"`
	MarketProtection *int    `json:"market_protection,omitempty"`
}

func (o OrderRequest) body() orderBody {
	b := orderBody{Quantity: o.Qty, Product: o.Product, Validity: "DAY", Tag: o.Tag, Instrument: o.Instrument,
		OrderType: o.Type, Side: o.Side}
	if o.Type == "LIMIT" || o.Type == "SL" {
		b.Price = o.Price
	}
	if o.Type == "SL" || o.Type == "SL-M" {
		b.Trigger = o.Trigger
	}
	if o.Type == "MARKET" || o.Type == "SL-M" {
		auto := -1
		b.MarketProtection = &auto
	}
	return b
}

// PlaceOrder places an order, unsliced, and returns its id.
func (c *Client) PlaceOrder(ctx context.Context, o OrderRequest) (string, error) {
	var d struct {
		IDs []string `json:"order_ids"`
	}
	if err := c.send(ctx, http.MethodPost, c.hft()+"/v3/order/place", o.body(), &d); err != nil {
		return "", err
	}
	if len(d.IDs) != 1 {
		return "", fmt.Errorf("upstox returned %d order ids", len(d.IDs))
	}
	return d.IDs[0], nil
}

// ModifyOrder changes an open order's type, quantity, price and trigger.
func (c *Client) ModifyOrder(ctx context.Context, id string, o OrderRequest) error {
	b := o.body()
	m := map[string]any{"order_id": id, "quantity": b.Quantity, "validity": b.Validity, "price": b.Price,
		"order_type": b.OrderType, "trigger_price": b.Trigger}
	if b.MarketProtection != nil {
		m["market_protection"] = *b.MarketProtection
	}
	return c.send(ctx, http.MethodPut, c.hft()+"/v3/order/modify", m, nil)
}

// CancelOrder cancels an open order.
func (c *Client) CancelOrder(ctx context.Context, id string) error {
	return c.send(ctx, http.MethodDelete, c.hft()+"/v3/order/cancel?"+url.Values{"order_id": {id}}.Encode(), nil, nil)
}

// OrderStatus is an order's latest state.
func (c *Client) OrderStatus(ctx context.Context, id string) (Order, error) {
	var o Order
	err := c.send(ctx, http.MethodGet, c.BaseURL+"/v2/order/details?"+url.Values{"order_id": {id}}.Encode(), nil, &o)
	return o, err
}

// Orders lists today's orders.
func (c *Client) Orders(ctx context.Context) ([]Order, error) {
	var out []Order
	err := c.send(ctx, http.MethodGet, c.BaseURL+"/v2/order/retrieve-all", nil, &out)
	return out, err
}

// EquityFunds is the margin available for trading, in rupees. Upstox's funds
// service is down from midnight to 05:30 IST.
func (c *Client) EquityFunds(ctx context.Context) (float64, error) {
	var d struct {
		Equity *struct {
			Available float64 `json:"available_margin"`
		} `json:"equity"`
	}
	if err := c.send(ctx, http.MethodGet, c.BaseURL+"/v2/user/get-funds-and-margin?segment=SEC", nil, &d); err != nil {
		return 0, err
	}
	if d.Equity == nil {
		return 0, errors.New("upstox funds: no equity segment")
	}
	return d.Equity.Available, nil
}

// StaticIPs returns the user's registered primary and secondary IPs.
func (c *Client) StaticIPs(ctx context.Context) (primary, secondary string, err error) {
	var d struct {
		Primary   string `json:"primary_ip"`
		Secondary string `json:"secondary_ip"`
	}
	err = c.send(ctx, http.MethodGet, c.BaseURL+"/v2/user/ip", nil, &d)
	return d.Primary, d.Secondary, err
}

// Position is a day position: what's held now and what was traded today,
// with Upstox's P&L on it before charges.
type Position struct {
	Instrument   string  `json:"instrument_token"`
	Product      string  `json:"product"`
	Qty          int64   `json:"quantity"`
	Symbol       string  `json:"trading_symbol"`
	Exchange     string  `json:"exchange"`
	Average      float64 `json:"average_price"`
	Last         float64 `json:"last_price"`
	PnL          float64 `json:"pnl"`
	Overnight    int64   `json:"overnight_quantity"`
	DayBuyQty    int64   `json:"day_buy_quantity"`
	DayBuyPrice  float64 `json:"day_buy_price"`
	DaySellQty   int64   `json:"day_sell_quantity"`
	DaySellPrice float64 `json:"day_sell_price"`
}

// Positions lists the account's day positions.
func (c *Client) Positions(ctx context.Context) ([]Position, error) {
	var out []Position
	err := c.send(ctx, http.MethodGet, c.BaseURL+"/v2/portfolio/short-term-positions", nil, &out)
	return out, err
}

// Trade is one fill in today's tradebook. ExchangeTime comes without a zone
// and, as seen on project_calm's trades, 5:30 ahead of IST.
type Trade struct {
	OrderID      string  `json:"order_id"`
	Exchange     string  `json:"exchange"`
	Symbol       string  `json:"trading_symbol"`
	Instrument   string  `json:"instrument_token"`
	Product      string  `json:"product"`
	Side         string  `json:"transaction_type"`
	Qty          int64   `json:"quantity"`
	Average      float64 `json:"average_price"`
	ExchangeTime string  `json:"exchange_timestamp"`
	OrderTime    string  `json:"order_timestamp"`
}

// Trades lists today's fills.
func (c *Client) Trades(ctx context.Context) ([]Trade, error) {
	var out []Trade
	err := c.send(ctx, http.MethodGet, c.BaseURL+"/v2/order/trades/get-trades-for-day", nil, &out)
	return out, err
}

// Charges is Upstox's total charges for one order: brokerage, STT, exchange
// and SEBI fees, stamp duty and GST.
func (c *Client) Charges(ctx context.Context, instrument, product, side string, qty int64, price float64) (float64, error) {
	q := url.Values{"instrument_token": {instrument}, "quantity": {strconv.FormatInt(qty, 10)}, "product": {product},
		"transaction_type": {side}, "price": {strconv.FormatFloat(price, 'f', 2, 64)}}
	var d struct {
		Charges struct {
			Total float64 `json:"total"`
		} `json:"charges"`
	}
	err := c.send(ctx, http.MethodGet, c.BaseURL+"/v2/charges/brokerage?"+q.Encode(), nil, &d)
	return d.Charges.Total, err
}

// LTP is each instrument's last traded price, by instrument key.
func (c *Client) LTP(ctx context.Context, keys ...string) (map[string]float64, error) {
	var d map[string]struct {
		Last  float64 `json:"last_price"`
		Token string  `json:"instrument_token"`
	}
	q := url.Values{"instrument_key": {strings.Join(keys, ",")}}
	if err := c.send(ctx, http.MethodGet, c.BaseURL+"/v2/market-quote/ltp?"+q.Encode(), nil, &d); err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, v := range d {
		out[v.Token] = v.Last
	}
	return out, nil
}
