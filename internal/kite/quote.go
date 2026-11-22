package kite

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Quote is one instrument's market snapshot.
type Quote struct {
	Last          float64  `json:"last_price"`
	Volume        int64    `json:"volume"` // traded today, in units
	OI            float64  `json:"oi"`
	Timestamp     kiteTime `json:"timestamp"`
	LastTradeTime kiteTime `json:"last_trade_time"`
	Depth         struct {
		Buy  []Level `json:"buy"`
		Sell []Level `json:"sell"`
	} `json:"depth"`
}

// Level is one depth level.
type Level struct {
	Price    float64 `json:"price"`
	Quantity int64   `json:"quantity"`
}

// Bid and Ask are the best prices, 0 when the side is empty.
func (q Quote) Bid() float64 {
	if len(q.Depth.Buy) > 0 && q.Depth.Buy[0].Quantity > 0 {
		return q.Depth.Buy[0].Price
	}
	return 0
}

func (q Quote) Ask() float64 {
	if len(q.Depth.Sell) > 0 && q.Depth.Sell[0].Quantity > 0 {
		return q.Depth.Sell[0].Price
	}
	return 0
}

// kiteTime reads Kite's "2006-01-02 15:04:05" IST times.
type kiteTime struct{ time.Time }

var ist = time.FixedZone("IST", 5*3600+1800)

func (t *kiteTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	v, err := time.ParseInLocation(time.DateTime, s, ist)
	if err != nil {
		return err
	}
	t.Time = v
	return nil
}

// Quotes fetches full quotes for keys such as "NFO:NIFTY26DEC24000PE",
// up to 500 a request.
func (c *Client) Quotes(ctx context.Context, keys []string) (map[string]Quote, error) {
	out := map[string]Quote{}
	for len(keys) > 0 {
		n := min(len(keys), 500)
		q := url.Values{}
		for _, k := range keys[:n] {
			q.Add("i", k)
		}
		body, err := c.get(ctx, "/quote?"+q.Encode())
		if err != nil {
			return nil, err
		}
		var env struct {
			Data map[string]Quote `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, fmt.Errorf("kite quote: %w", err)
		}
		for k, v := range env.Data {
			out[k] = v
		}
		keys = keys[n:]
	}
	return out, nil
}

// BasketMargin is the margin Kite would block for orders placed together,
// net of the account's open positions.
func (c *Client) BasketMargin(ctx context.Context, orders []OrderRequest) (float64, error) {
	type item struct {
		Exchange string  `json:"exchange"`
		Symbol   string  `json:"tradingsymbol"`
		Side     string  `json:"transaction_type"`
		Variety  string  `json:"variety"`
		Product  string  `json:"product"`
		Type     string  `json:"order_type"`
		Qty      int64   `json:"quantity"`
		Price    float64 `json:"price"`
		Trigger  float64 `json:"trigger_price"`
	}
	var items []item
	for _, o := range orders {
		items = append(items, item{o.Exchange, o.Symbol, o.Side, "regular", o.Product, o.Type, o.Qty, o.Price, o.Trigger})
	}
	body, err := json.Marshal(items)
	if err != nil {
		return 0, err
	}
	var d struct {
		Final struct {
			Total float64 `json:"total"`
		} `json:"final"`
	}
	if err := c.do(ctx, http.MethodPost, "/margins/basket?consider_positions=true&mode=compact", bytes.NewReader(body), "application/json", &d); err != nil {
		return 0, err
	}
	return d.Final.Total, nil
}
