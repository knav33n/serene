// Package upstox is a client for the Upstox market data APIs.
package upstox

import (
	"context"
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

// Fatal errors stop a download, because retrying can't help.
var (
	ErrAuth = errors.New("upstox rejected the token: run calm auth")
	ErrPlan = errors.New("this API needs the Upstox Plus plan")
)

// Client calls Upstox one request at a time, evenly paced. Upstox allows 2,000
// requests per 30 minutes and may suspend access beyond that, so the default
// pace stays inside it however long the job runs.
type Client struct {
	Token    auth.Secret
	BaseURL  string
	Interval time.Duration
	Backoff  time.Duration // first retry delay
	HTTP     *http.Client

	last     time.Time
	Requests int
}

// DefaultInterval is 1.1 requests per second, or 1,980 per 30 minutes.
const DefaultInterval = 910 * time.Millisecond

// New returns a client with default pacing.
func New(token auth.Secret) *Client {
	return &Client{Token: token, BaseURL: "https://api.upstox.com", Interval: DefaultInterval,
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

func (c *Client) pace(ctx context.Context) error {
	if wait := c.Interval - time.Since(c.last); wait > 0 {
		if err := sleep(ctx, wait); err != nil {
			return err
		}
	}
	c.last = time.Now()
	c.Requests++
	return nil
}

type envelope struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Code    string `json:"errorCode"`
		Message string `json:"message"`
	} `json:"errors"`
}

// get fetches a path into out, retrying rate limits, server errors and network
// failures with increasing delays.
func (c *Client) get(ctx context.Context, path string, out any) error {
	const attempts = 7
	delay := c.Backoff
	var last error
	for try := range attempts {
		if try > 0 {
			if err := sleep(ctx, delay); err != nil {
				return err
			}
			delay *= 2
		}
		retry, err := c.once(ctx, path, out)
		if err == nil || !retry {
			return err
		}
		last = err
	}
	return fmt.Errorf("gave up after %d tries: %w", attempts, last)
}

func (c *Client) once(ctx context.Context, path string, out any) (retry bool, err error) {
	if err := c.pace(ctx); err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token.Reveal())
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return true, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return true, err
	}
	var env envelope
	jsonErr := json.Unmarshal(body, &env)
	code, msg := "", ""
	if len(env.Errors) > 0 {
		code, msg = env.Errors[0].Code, env.Errors[0].Message
	}
	switch {
	case code == "UDAPI1149":
		return false, ErrPlan
	case resp.StatusCode == http.StatusUnauthorized:
		return false, ErrAuth
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return true, fmt.Errorf("HTTP %d %s", resp.StatusCode, msg)
	case resp.StatusCode != http.StatusOK:
		return false, fmt.Errorf("HTTP %d %s %s", resp.StatusCode, code, msg)
	case jsonErr != nil:
		return true, fmt.Errorf("unreadable response: %w", jsonErr)
	}
	return false, json.Unmarshal(env.Data, out)
}

// Expiries lists an index's expired option expiries.
func (c *Client) Expiries(ctx context.Context, indexKey string) ([]market.Date, error) {
	var raw []string
	q := url.Values{"instrument_key": {indexKey}}
	if err := c.get(ctx, "/v2/expired-instruments/expiries?"+q.Encode(), &raw); err != nil {
		return nil, err
	}
	out := make([]market.Date, 0, len(raw))
	for _, s := range raw {
		d, err := market.ParseDate(s)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// Contract is an expired option contract.
type Contract struct {
	Key     string  `json:"instrument_key"`
	Symbol  string  `json:"trading_symbol"`
	Type    string  `json:"instrument_type"`
	Strike  float64 `json:"strike_price"`
	LotSize int64   `json:"lot_size"`
	Weekly  bool    `json:"weekly"`
}

// OptionContracts lists an expiry's contracts.
func (c *Client) OptionContracts(ctx context.Context, indexKey string, expiry market.Date) ([]Contract, error) {
	var out []Contract
	q := url.Values{"instrument_key": {indexKey}, "expiry_date": {expiry.String()}}
	err := c.get(ctx, "/v2/expired-instruments/option/contract?"+q.Encode(), &out)
	return out, err
}

// Candle is one 1-minute candle in rupees.
type Candle struct {
	Time                   time.Time
	Open, High, Low, Close float64
	Volume, OI             int64
}

type candleData struct {
	Candles [][]any `json:"candles"`
}

func parseCandles(d candleData) ([]Candle, error) {
	out := make([]Candle, 0, len(d.Candles))
	for _, row := range d.Candles {
		if len(row) < 7 {
			return nil, fmt.Errorf("candle has %d fields", len(row))
		}
		ts, _ := row[0].(string)
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return nil, fmt.Errorf("candle time %q: %w", ts, err)
		}
		v := make([]float64, 6)
		for i := range v {
			if v[i], err = number(row[i+1]); err != nil {
				return nil, fmt.Errorf("candle at %s: %w", ts, err)
			}
		}
		out = append(out, Candle{t, v[0], v[1], v[2], v[3], int64(v[4]), int64(v[5])})
	}
	return out, nil
}

func number(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case string:
		return strconv.ParseFloat(n, 64)
	}
	return 0, fmt.Errorf("not a number: %v", v)
}

// ExpiredCandles returns an expired contract's 1-minute candles. Upstox serves
// a contract's whole life in one response.
func (c *Client) ExpiredCandles(ctx context.Context, expiredKey string, from, to market.Date) ([]Candle, error) {
	var d candleData
	path := fmt.Sprintf("/v2/expired-instruments/historical-candle/%s/1minute/%s/%s", url.PathEscape(expiredKey), to, from)
	if err := c.get(ctx, path, &d); err != nil {
		return nil, err
	}
	return parseCandles(d)
}

// IndexCandles returns 1-minute candles. Upstox limits a request to one month.
func (c *Client) IndexCandles(ctx context.Context, key string, from, to market.Date) ([]Candle, error) {
	var d candleData
	path := fmt.Sprintf("/v3/historical-candle/%s/minutes/1/%s/%s", url.PathEscape(key), to, from)
	if err := c.get(ctx, path, &d); err != nil {
		return nil, err
	}
	return parseCandles(d)
}
