package kite

import (
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
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-Kite-Version", "3")
	req.Header.Set("Authorization", "token "+c.APIKey+":"+c.Token.Reveal())
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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

// Position is a net position.
type Position struct {
	Symbol   string `json:"tradingsymbol"`
	Exchange string `json:"exchange"`
	Product  string `json:"product"`
	Qty      int64  `json:"quantity"`
}

// Positions lists the account's net positions.
func (c *Client) Positions(ctx context.Context) ([]Position, error) {
	var d struct {
		Net []Position `json:"net"`
	}
	err := c.send(ctx, http.MethodGet, "/portfolio/positions", nil, &d)
	return d.Net, err
}
