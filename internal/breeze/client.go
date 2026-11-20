// Package breeze is ICICI Direct's Breeze API: the account and order calls
// serene manual trades with. Every call but customerdetails is signed: an
// X-Checksum of sha256(timestamp + body + secret), with the session token
// customerdetails hands back for the day's apisession.
//
// Breeze takes no market orders, quantities are in units, and its order
// details carry no filled quantity: it's the quantity less what's pending
// and what was cancelled.
package breeze

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"serene/internal/market"
)

// ErrAuth means the session was refused.
var ErrAuth = errors.New("breeze rejected the session: log in again with serene auth")

// Client is one day's Breeze session.
type Client struct {
	AppKey  string
	Secret  string
	Session string // customerdetails' session_token (base64 user:key)
	BaseURL string // default https://api.icicidirect.com/breezeapi/api/v1
	HTTP    *http.Client
	Now     func() time.Time
}

const defaultBase = "https://api.icicidirect.com/breezeapi/api/v1"

// Login turns the day's apisession into a signed client.
func Login(ctx context.Context, appKey, secret, apisession string) (*Client, error) {
	c := &Client{AppKey: appKey, Secret: secret, BaseURL: defaultBase, HTTP: &http.Client{Timeout: 20 * time.Second}}
	return c, c.login(ctx, apisession)
}

func (c *Client) login(ctx context.Context, apisession string) error {
	body, _ := json.Marshal(map[string]string{"SessionToken": apisession, "AppKey": c.AppKey})
	var d struct {
		SessionToken string `json:"session_token"`
	}
	if err := c.send(ctx, http.MethodGet, "/customerdetails", body, false, &d); err != nil {
		return err
	}
	if d.SessionToken == "" {
		return ErrAuth
	}
	c.Session = d.SessionToken
	return nil
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// call sends a signed request with body (nil: {}) and decodes Success into out.
func (c *Client) call(ctx context.Context, method, path string, body any, out any) error {
	raw := []byte("{}")
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return err
		}
	}
	return c.send(ctx, method, path, raw, true, out)
}

// Checksum is Breeze's X-Checksum for a request.
func Checksum(timestamp string, body []byte, secret string) string {
	sum := sha256.Sum256([]byte(timestamp + string(body) + secret))
	return "token " + hex.EncodeToString(sum[:])
}

// noData is Breeze's error for an empty list.
func noData(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "no data found") || strings.Contains(m, "no record")
}

func (c *Client) send(ctx context.Context, method, path string, body []byte, signed bool, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if signed {
		ts := c.now().UTC().Format("2006-01-02T15:04:05") + ".000Z"
		req.Header.Set("X-Checksum", Checksum(ts, body, c.Secret))
		req.Header.Set("X-Timestamp", ts)
		req.Header.Set("X-AppKey", c.AppKey)
		req.Header.Set("X-SessionToken", c.Session)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("breeze %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("breeze %s: %w", path, err)
	}
	var env struct {
		Success json.RawMessage `json:"Success"`
		Status  int             `json:"Status"`
		Error   any             `json:"Error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("breeze %s: HTTP %d", path, resp.StatusCode)
	}
	if env.Error != nil && fmt.Sprint(env.Error) != "" {
		msg := fmt.Sprint(env.Error)
		if noData(msg) {
			return nil // out stays empty
		}
		if resp.StatusCode == http.StatusUnauthorized || strings.Contains(strings.ToLower(msg), "session") {
			return fmt.Errorf("%w (%s)", ErrAuth, msg)
		}
		return fmt.Errorf("breeze %s: %s", path, msg)
	}
	if resp.StatusCode != http.StatusOK || (env.Status != 0 && env.Status != 200) {
		return fmt.Errorf("breeze %s: HTTP %d", path, resp.StatusCode)
	}
	if out == nil || len(env.Success) == 0 || string(env.Success) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Success, out); err != nil {
		return fmt.Errorf("breeze %s: %w", path, err)
	}
	return nil
}

// Num reads a number Breeze sends as a number, a string or null.
type Num float64

func (n *Num) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	if err != nil {
		return fmt.Errorf("breeze number %q: %w", s, err)
	}
	*n = Num(v)
	return nil
}

// list decodes a Success that may be one object or a list of them.
type list[T any] []T

func (l *list[T]) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '{' {
		var one T
		if err := json.Unmarshal(b, &one); err != nil {
			return err
		}
		*l = []T{one}
		return nil
	}
	var many []T
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*l = many
	return nil
}

// Funds is the F&O money free to trade: allocated to F&O less what trades block.
func (c *Client) Funds(ctx context.Context) (float64, error) {
	var f struct {
		AllocatedFNO Num `json:"allocated_fno"`
		BlockedFNO   Num `json:"block_by_trade_fno"`
	}
	if err := c.call(ctx, http.MethodGet, "/funds", nil, &f); err != nil {
		return 0, err
	}
	return float64(f.AllocatedFNO - f.BlockedFNO), nil
}

// Contract names an option the way Breeze does.
type Contract struct {
	Stock  string // NIFTY
	Expiry market.Date
	Strike float64
	Right  string // CE, PE
}

// ParseExpiry reads Breeze's expiry dates: 27-Feb-2025, 2025-02-27 or 2025-02-27T06:00:00.000Z.
func ParseExpiry(s string) (market.Date, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"02-Jan-2006", "2006-01-02", "2006-01-02T15:04:05.000Z", "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return market.Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}, nil
		}
	}
	if t, err := time.Parse("02-JAN-2006", strings.ToUpper(s)); err == nil {
		return market.Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}, nil
	}
	return market.Date{}, fmt.Errorf("breeze expiry %q", s)
}

// right turns Breeze's Call / Put into CE / PE.
func right(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "call", "ce":
		return "CE"
	case "put", "pe":
		return "PE"
	}
	return ""
}

func contractOf(stock, expiry string, strike Num, r string) (Contract, bool) {
	e, err := ParseExpiry(expiry)
	rr := right(r)
	if err != nil || rr == "" {
		return Contract{}, false
	}
	return Contract{Stock: strings.TrimSpace(stock), Expiry: e, Strike: float64(strike), Right: rr}, true
}

// Position is an F&O position from portfoliopositions.
type Position struct {
	Contract
	Option   bool
	Exchange string
	Product  string // options, futures
	Qty      int64  // negative short
	Avg      float64
	LTP      float64
	PnL      float64
}

// Positions are the account's open F&O option positions.
func (c *Client) Positions(ctx context.Context) ([]Position, error) {
	var rows list[struct {
		Segment  string `json:"segment"`
		Product  string `json:"product_type"`
		Exchange string `json:"exchange_code"`
		Stock    string `json:"stock_code"`
		Expiry   string `json:"expiry_date"`
		Strike   Num    `json:"strike_price"`
		Right    string `json:"right"`
		Action   string `json:"action"`
		Qty      Num    `json:"quantity"`
		Avg      Num    `json:"average_price"`
		LTP      Num    `json:"ltp"`
		PnL      *Num   `json:"pnl"`
	}]
	if err := c.call(ctx, http.MethodGet, "/portfoliopositions", nil, &rows); err != nil {
		return nil, err
	}
	var out []Position
	for _, r := range rows {
		ct, ok := contractOf(r.Stock, r.Expiry, r.Strike, r.Right)
		if !ok || !strings.EqualFold(r.Segment, "fno") {
			continue
		}
		qty := int64(r.Qty)
		if strings.EqualFold(r.Action, "sell") && qty > 0 {
			qty = -qty
		}
		p := Position{Contract: ct, Option: true, Exchange: r.Exchange, Product: strings.ToLower(r.Product), Qty: qty,
			Avg: float64(r.Avg), LTP: float64(r.LTP)}
		if r.PnL != nil {
			p.PnL = float64(*r.PnL)
		} else {
			p.PnL = (p.LTP - p.Avg) * float64(qty)
		}
		out = append(out, p)
	}
	return out, nil
}

// day is today's from_date and to_date, as Breeze takes them.
func (c *Client) day() (string, string) {
	d := c.now().In(market.IST).Format("2006-01-02")
	return d + "T00:00:00.000Z", d + "T23:59:59.000Z"
}

// Trade is a row of the trade list: an order's fills today.
type Trade struct {
	Contract
	OrderID string
	Side    string // BUY, SELL
	Qty     int64
	Price   float64
	Product string
	Charges float64 // brokerage and taxes
}

// Trades are today's NFO trades.
func (c *Client) Trades(ctx context.Context) ([]Trade, error) {
	from, to := c.day()
	var rows list[struct {
		OrderID   string `json:"order_id"`
		Action    string `json:"action"`
		Qty       Num    `json:"quantity"`
		Avg       Num    `json:"average_cost"`
		Brokerage Num    `json:"brokerage_amount"`
		Taxes     Num    `json:"total_taxes"`
		Product   string `json:"product_type"`
		Stock     string `json:"stock_code"`
		Expiry    string `json:"expiry_date"`
		Strike    Num    `json:"strike_price"`
		Right     string `json:"right"`
	}]
	if err := c.call(ctx, http.MethodGet, "/trades", map[string]string{"from_date": from, "to_date": to, "exchange_code": "NFO"}, &rows); err != nil {
		return nil, err
	}
	var out []Trade
	for _, r := range rows {
		ct, ok := contractOf(r.Stock, r.Expiry, r.Strike, r.Right)
		if !ok {
			continue
		}
		out = append(out, Trade{Contract: ct, OrderID: r.OrderID, Side: strings.ToUpper(r.Action), Qty: int64(r.Qty),
			Price: float64(r.Avg), Product: strings.ToLower(r.Product), Charges: float64(r.Brokerage + r.Taxes)})
	}
	return out, nil
}

// Order is an order's detail.
type Order struct {
	Contract
	ID        string
	Side      string // BUY, SELL
	Status    string
	Qty       int64
	Pending   int64
	Cancelled int64
	Price     float64
	Average   float64
	Remark    string
}

// Done is whether the order has stopped working.
func (o Order) Done() bool {
	switch strings.ToLower(o.Status) {
	case "executed", "rejected", "expired", "cancelled", "partially executed and expired", "partially executed and cancelled":
		return true
	}
	return false
}

// Filled is what executed: Breeze gives the quantity, what's pending and what was cancelled.
func (o Order) Filled() int64 {
	if strings.EqualFold(o.Status, "rejected") {
		return 0
	}
	return max(0, o.Qty-o.Pending-o.Cancelled)
}

type orderRow struct {
	ID        string `json:"order_id"`
	Stock     string `json:"stock_code"`
	Action    string `json:"action"`
	Status    string `json:"status"`
	Qty       Num    `json:"quantity"`
	Pending   Num    `json:"pending_quantity"`
	Cancelled Num    `json:"cancelled_quantity"`
	Price     Num    `json:"price"`
	Average   Num    `json:"average_price"`
	Expiry    string `json:"expiry_date"`
	Strike    Num    `json:"strike_price"`
	Right     string `json:"right"`
	Remark    string `json:"user_remark"`
}

func (r orderRow) order() Order {
	ct, _ := contractOf(r.Stock, r.Expiry, r.Strike, r.Right)
	return Order{Contract: ct, ID: r.ID, Side: strings.ToUpper(r.Action), Status: r.Status, Qty: int64(r.Qty),
		Pending: int64(r.Pending), Cancelled: int64(r.Cancelled), Price: float64(r.Price), Average: float64(r.Average),
		Remark: r.Remark}
}

// Orders are today's NFO orders.
func (c *Client) Orders(ctx context.Context) ([]Order, error) {
	from, to := c.day()
	var rows list[orderRow]
	if err := c.call(ctx, http.MethodGet, "/order", map[string]string{"exchange_code": "NFO", "from_date": from, "to_date": to}, &rows); err != nil {
		return nil, err
	}
	out := make([]Order, len(rows))
	for i, r := range rows {
		out[i] = r.order()
	}
	return out, nil
}

// Order is one NFO order by id.
func (c *Client) Order(ctx context.Context, id string) (Order, error) {
	var rows list[orderRow]
	if err := c.call(ctx, http.MethodGet, "/order", map[string]string{"exchange_code": "NFO", "order_id": id}, &rows); err != nil {
		return Order{}, err
	}
	for _, r := range rows {
		if r.ID == id || len(rows) == 1 {
			return r.order(), nil
		}
	}
	return Order{}, fmt.Errorf("breeze has no order %s", id)
}

// OrderRequest is a LIMIT order on an NFO option.
type OrderRequest struct {
	Contract
	Side   string // BUY, SELL
	Qty    int64  // units
	Price  float64
	Remark string // letters and digits only
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// PlaceOrder places a DAY LIMIT order on an option ("options" product) and returns its id.
func (c *Client) PlaceOrder(ctx context.Context, o OrderRequest) (string, error) {
	r := map[string]string{"CE": "call", "PE": "put"}[o.Right]
	action := map[string]string{"BUY": "buy", "SELL": "sell"}[o.Side]
	if r == "" || action == "" || o.Qty <= 0 || o.Price <= 0 {
		return "", fmt.Errorf("breeze: bad order %+v", o)
	}
	body := map[string]string{"stock_code": o.Stock, "exchange_code": "NFO", "product": "options", "action": action,
		"order_type": "limit", "stoploss": "", "quantity": strconv.FormatInt(o.Qty, 10), "price": num(o.Price),
		"validity": "day", "disclosed_quantity": "0", "expiry_date": o.Expiry.String() + "T06:00:00.000Z",
		"right": r, "strike_price": num(o.Strike)}
	if o.Remark != "" {
		body["user_remark"] = o.Remark
	}
	var d struct {
		OrderID string `json:"order_id"`
	}
	if err := c.call(ctx, http.MethodPost, "/order", body, &d); err != nil {
		return "", err
	}
	if d.OrderID == "" {
		return "", errors.New("breeze placed an order without an id")
	}
	return d.OrderID, nil
}

// CancelOrder cancels a working NFO order.
func (c *Client) CancelOrder(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodDelete, "/order", map[string]string{"order_id": id, "exchange_code": "NFO"}, nil)
}
