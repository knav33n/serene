package kite

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

// OrderRequest is a regular-variety order. Price is for LIMIT and SL,
// Trigger for SL and SL-M, in rupees.
type OrderRequest struct {
	Exchange, Symbol string
	Side             string // BUY, SELL
	Type             string // LIMIT, SL, SL-M, MARKET
	Product          string // MIS, NRML
	Qty              int64
	Price, Trigger   float64
	Tag              string // up to 20 alphanumeric characters
}

// Order is an order's latest state.
type Order struct {
	ID            string  `json:"order_id"`
	Status        string  `json:"status"`
	StatusMessage string  `json:"status_message"`
	Symbol        string  `json:"tradingsymbol"`
	Side          string  `json:"transaction_type"`
	Type          string  `json:"order_type"`
	Qty           int64   `json:"quantity"`
	Filled        int64   `json:"filled_quantity"`
	Pending       int64   `json:"pending_quantity"`
	Price         float64 `json:"price"`
	Trigger       float64 `json:"trigger_price"`
	Average       float64 `json:"average_price"`
	Tag           string  `json:"tag"`
	Exchange      string  `json:"exchange"`
	Product       string  `json:"product"`
	Variety       string  `json:"variety"`
}

func (o OrderRequest) form() url.Values {
	f := url.Values{"exchange": {o.Exchange}, "tradingsymbol": {o.Symbol}, "transaction_type": {o.Side},
		"order_type": {o.Type}, "quantity": {strconv.FormatInt(o.Qty, 10)}, "product": {o.Product}, "validity": {"DAY"}}
	if o.Type == "LIMIT" || o.Type == "SL" {
		f.Set("price", strconv.FormatFloat(o.Price, 'f', 2, 64))
	}
	if o.Type == "SL" || o.Type == "SL-M" {
		f.Set("trigger_price", strconv.FormatFloat(o.Trigger, 'f', 2, 64))
	}
	if o.Type == "MARKET" || o.Type == "SL-M" {
		f.Set("market_protection", "-1")
	}
	if o.Tag != "" {
		f.Set("tag", o.Tag)
	}
	return f
}

// send makes one order request, never retried: a timeout may still have
// reached Kite, so the caller reconciles by tag instead of resending.
func (c *Client) send(ctx context.Context, method, path string, form url.Values, out any) error {
	if form == nil {
		return c.do(ctx, method, path, nil, "", out)
	}
	return c.do(ctx, method, path, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", out)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, ctype string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-Kite-Version", "3")
	req.Header.Set("Authorization", "token "+c.APIKey+":"+c.Token.Reveal())
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
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
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: %s", ErrAuth, message(b))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kite %s %s: HTTP %d %s", method, path, resp.StatusCode, message(b))
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return fmt.Errorf("kite %s %s: %w", method, path, err)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

// PlaceOrder places a regular order and returns its id.
func (c *Client) PlaceOrder(ctx context.Context, o OrderRequest) (string, error) {
	var d struct {
		ID string `json:"order_id"`
	}
	if err := c.send(ctx, http.MethodPost, "/orders/regular", o.form(), &d); err != nil {
		return "", err
	}
	if d.ID == "" {
		return "", errors.New("kite returned no order id")
	}
	return d.ID, nil
}

// ModifyOrder changes an open order's type, quantity, price and trigger.
func (c *Client) ModifyOrder(ctx context.Context, id string, o OrderRequest) error {
	f := url.Values{"order_type": {o.Type}, "quantity": {strconv.FormatInt(o.Qty, 10)}, "validity": {"DAY"}}
	if o.Type == "LIMIT" || o.Type == "SL" {
		f.Set("price", strconv.FormatFloat(o.Price, 'f', 2, 64))
	}
	if o.Type == "SL" || o.Type == "SL-M" {
		f.Set("trigger_price", strconv.FormatFloat(o.Trigger, 'f', 2, 64))
	}
	return c.send(ctx, http.MethodPut, "/orders/regular/"+url.PathEscape(id), f, nil)
}

// CancelOrder cancels an open order.
func (c *Client) CancelOrder(ctx context.Context, id string) error {
	return c.send(ctx, http.MethodDelete, "/orders/regular/"+url.PathEscape(id), nil, nil)
}

// Orders lists today's orders.
func (c *Client) Orders(ctx context.Context) ([]Order, error) {
	var out []Order
	err := c.send(ctx, http.MethodGet, "/orders", nil, &out)
	return out, err
}

// OrderStatus is an order's latest state, the last entry of its history.
func (c *Client) OrderStatus(ctx context.Context, id string) (Order, error) {
	var hist []Order
	if err := c.send(ctx, http.MethodGet, "/orders/"+url.PathEscape(id), nil, &hist); err != nil {
		return Order{}, err
	}
	if len(hist) == 0 {
		return Order{}, fmt.Errorf("kite has no history for order %s", id)
	}
	return hist[len(hist)-1], nil
}

// EquityFunds is the equity segment's net margin available, in rupees.
func (c *Client) EquityFunds(ctx context.Context) (float64, error) {
	var d struct {
		Net float64 `json:"net"`
	}
	if err := c.send(ctx, http.MethodGet, "/user/margins/equity", nil, &d); err != nil {
		return 0, err
	}
	return d.Net, nil
}

// Position is a net position: what's held now and what was traded today,
// with Kite's P&L on it before charges.
type Position struct {
	Symbol       string  `json:"tradingsymbol"`
	Exchange     string  `json:"exchange"`
	Product      string  `json:"product"`
	Qty          int64   `json:"quantity"`
	Average      float64 `json:"average_price"`
	Last         float64 `json:"last_price"`
	PnL          float64 `json:"pnl"`
	Overnight    int64   `json:"overnight_quantity"`
	DayBuyQty    int64   `json:"day_buy_quantity"`
	DayBuyPrice  float64 `json:"day_buy_price"`
	DaySellQty   int64   `json:"day_sell_quantity"`
	DaySellPrice float64 `json:"day_sell_price"`
}

// Positions lists the account's net positions.
func (c *Client) Positions(ctx context.Context) ([]Position, error) {
	var d struct {
		Net []Position `json:"net"`
	}
	err := c.send(ctx, http.MethodGet, "/portfolio/positions", nil, &d)
	return d.Net, err
}

// Trade is one fill in today's tradebook.
type Trade struct {
	OrderID      string  `json:"order_id"`
	Exchange     string  `json:"exchange"`
	Symbol       string  `json:"tradingsymbol"`
	Product      string  `json:"product"`
	Side         string  `json:"transaction_type"`
	Qty          int64   `json:"quantity"`
	Average      float64 `json:"average_price"`
	FillTime     string  `json:"fill_timestamp"`
	ExchangeTime string  `json:"exchange_timestamp"`
}

// Trades lists today's fills.
func (c *Client) Trades(ctx context.Context) ([]Trade, error) {
	var out []Trade
	err := c.send(ctx, http.MethodGet, "/trades", nil, &out)
	return out, err
}

// OrderCharges is Kite's virtual contract note for orders that filled: the
// total of brokerage, STT, exchange and SEBI fees, stamp duty and GST for
// each, in the order given.
func (c *Client) OrderCharges(ctx context.Context, orders []Order) ([]float64, error) {
	type row struct {
		ID       string  `json:"order_id"`
		Exchange string  `json:"exchange"`
		Symbol   string  `json:"tradingsymbol"`
		Side     string  `json:"transaction_type"`
		Variety  string  `json:"variety"`
		Product  string  `json:"product"`
		Type     string  `json:"order_type"`
		Qty      int64   `json:"quantity"`
		Average  float64 `json:"average_price"`
	}
	in := make([]row, len(orders))
	for i, o := range orders {
		v := o.Variety
		if v == "" {
			v = "regular"
		}
		in[i] = row{o.ID, o.Exchange, o.Symbol, o.Side, v, o.Product, o.Type, o.Filled, o.Average}
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out []struct {
		Charges struct {
			Total float64 `json:"total"`
		} `json:"charges"`
	}
	if err := c.do(ctx, http.MethodPost, "/charges/orders", bytes.NewReader(b), "application/json", &out); err != nil {
		return nil, err
	}
	if len(out) != len(orders) {
		return nil, fmt.Errorf("kite's contract note has %d rows for %d orders", len(out), len(orders))
	}
	totals := make([]float64, len(out))
	for i, r := range out {
		totals[i] = r.Charges.Total
	}
	return totals, nil
}
