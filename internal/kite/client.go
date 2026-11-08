// Package kite is a client for the Zerodha Kite Connect market data APIs.
package kite

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"calm/internal/auth"
	"calm/internal/market"
)

// ErrAuth stops a download, because retrying can't help.
var ErrAuth = errors.New("kite rejected the token: run calm auth")

// Client calls Kite one request at a time, evenly paced. Kite allows 3
// historical requests a second.
type Client struct {
	APIKey   string
	Token    auth.Secret
	BaseURL  string
	Interval time.Duration
	Backoff  time.Duration
	HTTP     *http.Client

	last     time.Time
	Requests int
}

// DefaultInterval keeps under Kite's historical limit of 3 a second.
const DefaultInterval = 400 * time.Millisecond

// New returns a client with default pacing.
func New(apiKey string, token auth.Secret) *Client {
	return &Client{APIKey: apiKey, Token: token, BaseURL: "https://api.kite.trade", Interval: DefaultInterval,
		Backoff: 5 * time.Second, HTTP: &http.Client{Timeout: 90 * time.Second}}
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// get fetches a path, retrying rate limits and server errors with backoff.
func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	backoff := c.Backoff
	for try := 0; ; try++ {
		if wait := c.Interval - time.Since(c.last); wait > 0 {
			if err := sleep(ctx, wait); err != nil {
				return nil, err
			}
		}
		c.last = time.Now()
		c.Requests++
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Kite-Version", "3")
		req.Header.Set("Authorization", "token "+c.APIKey+":"+c.Token.Reveal())
		resp, err := c.HTTP.Do(req)
		var body []byte
		if err == nil {
			body, err = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		retry := err != nil
		if err == nil {
			switch {
			case resp.StatusCode == http.StatusOK:
				return body, nil
			case resp.StatusCode == http.StatusForbidden:
				return nil, ErrAuth
			case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
				retry = true
				err = fmt.Errorf("HTTP %d %s", resp.StatusCode, message(body))
			default:
				return nil, fmt.Errorf("HTTP %d %s", resp.StatusCode, message(body))
			}
		}
		if !retry || try >= 5 {
			return nil, err
		}
		if err := sleep(ctx, backoff); err != nil {
			return nil, err
		}
		backoff *= 2
	}
}

func message(body []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		return e.Message
	}
	return string(body[:min(len(body), 200)])
}

// Instrument is one row of Kite's instrument list.
type Instrument struct {
	Token         int64
	ExchangeToken string
	Symbol        string
	Name          string
	Expiry        market.Date // zero when none
	Strike        float64
	TickSize      float64
	LotSize       int64
	Type          string // FUT, CE, PE, EQ
	Segment       string
	Exchange      string
}

// Instruments lists an exchange's tradable instruments as of today.
func (c *Client) Instruments(ctx context.Context, exchange string) ([]Instrument, error) {
	body, err := c.get(ctx, "/instruments/"+url.PathEscape(exchange))
	if err != nil {
		return nil, err
	}
	return ParseInstruments(body)
}

// ParseInstruments reads Kite's instrument CSV.
func ParseInstruments(body []byte) ([]Instrument, error) {
	r := csv.NewReader(bytes.NewReader(body))
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("instrument list: %w", err)
	}
	if len(rows) == 0 {
		return nil, errors.New("instrument list is empty")
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	for _, h := range []string{"instrument_token", "exchange_token", "tradingsymbol", "name", "expiry", "strike",
		"tick_size", "lot_size", "instrument_type", "segment", "exchange"} {
		if _, ok := col[h]; !ok {
			return nil, fmt.Errorf("instrument list has no %s column", h)
		}
	}
	out := make([]Instrument, 0, len(rows)-1)
	for _, row := range rows[1:] {
		var in Instrument
		if in.Token, err = strconv.ParseInt(row[col["instrument_token"]], 10, 64); err != nil {
			return nil, err
		}
		in.ExchangeToken = row[col["exchange_token"]]
		in.Symbol, in.Name = row[col["tradingsymbol"]], row[col["name"]]
		if e := row[col["expiry"]]; e != "" {
			if in.Expiry, err = market.ParseDate(e); err != nil {
				return nil, err
			}
		}
		in.Strike, _ = strconv.ParseFloat(row[col["strike"]], 64)
		in.TickSize, _ = strconv.ParseFloat(row[col["tick_size"]], 64)
		in.LotSize, _ = strconv.ParseInt(row[col["lot_size"]], 10, 64)
		in.Type, in.Segment, in.Exchange = row[col["instrument_type"]], row[col["segment"]], row[col["exchange"]]
		out = append(out, in)
	}
	return out, nil
}

// Candle is one candle in rupees.
type Candle struct {
	Time                   time.Time
	Open, High, Low, Close float64
	Volume, OI             int64
}

// Kite's limits on one request's span, by interval.
const (
	MinuteSpan = 60 * 24 * time.Hour
	DaySpan    = 2000 * 24 * time.Hour
)

// Candles returns candles oldest first. Interval is "minute" or "day";
// continuous stitches a future's expired contracts, which Kite allows for
// daily candles only.
func (c *Client) Candles(ctx context.Context, token int64, interval string, from, to time.Time, continuous bool) ([]Candle, error) {
	q := url.Values{"from": {from.In(market.IST).Format("2006-01-02 15:04:05")},
		"to": {to.In(market.IST).Format("2006-01-02 15:04:05")}, "oi": {"1"}}
	if continuous {
		q.Set("continuous", "1")
	}
	body, err := c.get(ctx, fmt.Sprintf("/instruments/historical/%d/%s?%s", token, interval, q.Encode()))
	if err != nil {
		return nil, err
	}
	return parseCandles(body)
}

func parseCandles(body []byte) ([]Candle, error) {
	var env struct {
		Data struct {
			Candles [][]any `json:"candles"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("unreadable candles: %w", err)
	}
	out := make([]Candle, 0, len(env.Data.Candles))
	for _, row := range env.Data.Candles {
		if len(row) < 6 {
			return nil, fmt.Errorf("candle has %d fields", len(row))
		}
		ts, _ := row[0].(string)
		t, err := time.Parse("2006-01-02T15:04:05-0700", ts)
		if err != nil {
			return nil, fmt.Errorf("candle time %q: %w", ts, err)
		}
		v := make([]float64, 6)
		for i := 1; i < len(row) && i <= 6; i++ {
			n, ok := row[i].(float64)
			if !ok {
				return nil, fmt.Errorf("candle at %s: field %d is not a number", ts, i)
			}
			v[i-1] = n
		}
		out = append(out, Candle{t, v[0], v[1], v[2], v[3], int64(v[4]), int64(v[5])})
	}
	return out, nil
}
