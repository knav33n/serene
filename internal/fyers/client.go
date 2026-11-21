// Package fyers is a client for the Fyers API v3 market data endpoints:
// candles for listed instruments, and the expired F&O history service.
package fyers

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"serene/internal/auth"
	"serene/internal/market"
)

// Fatal errors stop a download, because retrying can't help.
var (
	ErrAuth = errors.New("fyers rejected the token: run serene auth")
	// ErrRateLimited means Fyers counted a rate-limit breach. Retrying risks a
	// block for the rest of the day, so the job stops instead.
	ErrRateLimited = errors.New("fyers rate limit exceeded: stopping before the account is blocked for the day")
	ErrServer      = errors.New("fyers server error")
	// ErrNoData is Fyers' code -50 "Invalid input", which it returns for a
	// range before a series starts and for a symbol it doesn't know, such as a
	// strike that never listed, and code -300 "Invalid symbol provided", which
	// it returns for a delisted or renamed stock's spot (NSE:ALBK-EQ).
	ErrNoData = errors.New("fyers has no such data")
	// ErrBroken is Fyers' code -99 "Something went wrong", which it returns
	// every time for some ranges of some expired contracts: retrying the same
	// request can't help, though a narrower range may.
	ErrBroken = errors.New("fyers fails on this range")
)

// Client calls the Fyers data APIs through a shared limiter.
type Client struct {
	AppID     string
	Token     auth.Secret
	DataURL   string // default https://api-t1.fyers.in/data
	PublicURL string // symbol master files; default https://public.fyers.in
	Limiter   *Limiter
	Backoff   time.Duration
	HTTP      *http.Client
	// OnRetry, if set, hears each error that is about to be retried.
	OnRetry func(path string, try int, err error)
}

// New returns a client. The limiter is shared by every goroutine using it.
func New(appID string, token auth.Secret, lim *Limiter) *Client {
	return &Client{AppID: appID, Token: token, DataURL: "https://api-t1.fyers.in/data",
		PublicURL: "https://public.fyers.in", Limiter: lim, Backoff: 5 * time.Second,
		HTTP: &http.Client{Timeout: 90 * time.Second, Transport: transport()}}
}

// transport pings idle HTTP/2 connections so a dead one is dropped within
// seconds. Without it, when the machine's address changes (as the mobile link
// does), every request queues on the old connection until the kernel gives up
// on it, about 13 minutes on 2026-11-20.
func transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.HTTP2 = &http.HTTP2Config{SendPingTimeout: 15 * time.Second, PingTimeout: 10 * time.Second}
	return t
}

// attempts is how many times a request meets a network or server error before
// giving up.
const attempts = 5

// envelope holds the fields every Fyers response shares.
type envelope struct {
	S       string `json:"s"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// get fetches path?q into out, retrying network and server errors.
func (c *Client) get(ctx context.Context, path string, q url.Values, expiredIntraday bool, out any) error {
	delay := c.Backoff
	var last error
	for try := range attempts {
		if try > 0 {
			if err := sleep(ctx, delay); err != nil {
				return err
			}
			delay *= 2
		}
		retry, err := c.once(ctx, path, q, expiredIntraday, out)
		if err == nil || !retry {
			return err
		}
		last = err
		if c.OnRetry != nil && try+1 < attempts {
			c.OnRetry(path, try+1, err)
		}
	}
	return fmt.Errorf("gave up after %d tries: %w", attempts, last)
}

func (c *Client) once(ctx context.Context, path string, q url.Values, expiredIntraday bool, out any) (retry bool, err error) {
	if err := c.Limiter.Wait(ctx, expiredIntraday); err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.DataURL+path+"?"+q.Encode(), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", c.AppID+":"+c.Token.Reveal())
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err // the URL is safe, but keep errors short
		}
		return true, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return true, err
	}
	var env envelope
	jsonErr := json.Unmarshal(body, &env)
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || env.Code == -429 || env.Code == -360:
		return false, fmt.Errorf("%w (%s)", ErrRateLimited, env.Message)
	case resp.StatusCode == http.StatusUnauthorized || env.Code == -8 || env.Code == -15 || env.Code == -16 || env.Code == -17:
		return false, ErrAuth
	case resp.StatusCode >= 500:
		return true, fmt.Errorf("%w: HTTP %d %s", ErrServer, resp.StatusCode, env.Message)
	case jsonErr != nil:
		return true, fmt.Errorf("unreadable response (HTTP %d): %w", resp.StatusCode, jsonErr)
	case env.S == "no_data":
		return false, json.Unmarshal(body, out)
	case env.Code == -99:
		return false, fmt.Errorf("%w: HTTP %d code %d %s", ErrBroken, resp.StatusCode, env.Code, env.Message)
	case env.Code == -50 || env.Code == -300:
		return false, fmt.Errorf("%w: HTTP %d code %d %s", ErrNoData, resp.StatusCode, env.Code, env.Message)
	case resp.StatusCode != http.StatusOK || env.S != "ok":
		return false, fmt.Errorf("fyers %s: HTTP %d code %d %s", path, resp.StatusCode, env.Code, env.Message)
	}
	return false, json.Unmarshal(body, out)
}

// Candle is one candle in rupees.
type Candle struct {
	Time                   time.Time
	Open, High, Low, Close float64
	Volume, OI             int64
}

// MaxIntradayDays is the widest window Fyers serves for minute candles.
const MaxIntradayDays = 100

// MaxDailyDays is the widest window for daily candles.
const MaxDailyDays = 366

// Resolution is a candle size in Fyers' notation.
type Resolution string

const (
	Minute Resolution = "1"
	Day    Resolution = "1D"
)

func (r Resolution) maxDays() int {
	if r == Day {
		return MaxDailyDays
	}
	return MaxIntradayDays
}

type historyResp struct {
	Candles [][]float64 `json:"candles"`
}

// History returns candles for a listed instrument (an index, a stock or an
// unexpired contract) over [from, to], oldest first. Windows wider than Fyers
// allows are split.
func (c *Client) History(ctx context.Context, symbol string, res Resolution, from, to market.Date, oi bool) ([]Candle, error) {
	var out []Candle
	for _, w := range windows(from, to, res.maxDays()) {
		q := url.Values{"symbol": {symbol}, "resolution": {string(res)}, "date_format": {"1"},
			"range_from": {w[0].String()}, "range_to": {w[1].String()}, "cont_flag": {"1"}}
		if oi {
			q.Set("oi_flag", "1")
		}
		var r historyResp
		if err := c.get(ctx, "/history", q, false, &r); err != nil {
			return nil, fmt.Errorf("%s %s..%s: %w", symbol, w[0], w[1], err)
		}
		for _, row := range r.Candles {
			cd, err := candle(row, 6)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", symbol, err)
			}
			out = append(out, cd)
		}
	}
	return out, nil
}

func candle(row []float64, oiAt int) (Candle, error) {
	if len(row) < 6 {
		return Candle{}, fmt.Errorf("candle has %d fields", len(row))
	}
	cd := Candle{Time: time.Unix(int64(row[0]), 0).In(market.IST), Open: row[1], High: row[2],
		Low: row[3], Close: row[4], Volume: int64(row[5])}
	if oiAt >= 0 && oiAt < len(row) {
		cd.OI = int64(row[oiAt])
	}
	return cd, nil
}

// windows splits [from, to] into consecutive spans of at most n days.
func windows(from, to market.Date, n int) [][2]market.Date {
	var out [][2]market.Date
	for start := from; !to.Before(start); {
		end := start.AddDays(n - 1)
		if to.Before(end) {
			end = to
		}
		out = append(out, [2]market.Date{start, end})
		start = end.AddDays(1)
	}
	return out
}

// Expiries are an underlying's expired contract dates.
type Expiries struct {
	Futures, Options []market.Date
}

// ExpiryDates lists an underlying's expiries in [from, to]. The underlying is
// an index or stock symbol such as NSE:NIFTY50-INDEX or NSE:SBIN-EQ. Wider
// ranges are split into the 366 days Fyers allows.
func (c *Client) ExpiryDates(ctx context.Context, underlying string, from, to market.Date) (Expiries, error) {
	var out Expiries
	seen := map[[2]any]bool{}
	for _, w := range windows(from, to, MaxDailyDays) {
		var r struct {
			Data struct {
				ExpiryDates struct {
					Futures []json.RawMessage `json:"futures"`
					Options []json.RawMessage `json:"options"`
				} `json:"expiry_dates"`
			} `json:"data"`
		}
		q := url.Values{"symbol": {underlying}, "date_format": {"1"},
			"range_from": {w[0].String()}, "range_to": {w[1].String()}}
		if err := c.get(ctx, "/history/fno/expired/expiry-dates", q, false, &r); err != nil {
			return out, fmt.Errorf("%s expiries %s..%s: %w", underlying, w[0], w[1], err)
		}
		for kind, list := range map[string][]json.RawMessage{"fut": r.Data.ExpiryDates.Futures, "opt": r.Data.ExpiryDates.Options} {
			for _, raw := range list {
				d, err := parseDate(raw)
				if err != nil {
					return out, err
				}
				if seen[[2]any{kind, d}] {
					continue
				}
				seen[[2]any{kind, d}] = true
				if kind == "fut" {
					out.Futures = append(out.Futures, d)
				} else {
					out.Options = append(out.Options, d)
				}
			}
		}
	}
	return out, nil
}

// parseDate reads a date that Fyers may send as yyyy-mm-dd, dd-mm-yyyy or
// epoch seconds.
func parseDate(raw json.RawMessage) (market.Date, error) {
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return market.DateOf(time.Unix(n, 0)), nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return market.Date{}, fmt.Errorf("unreadable expiry %s", raw)
	}
	if d, err := market.ParseDate(s); err == nil {
		return d, nil
	}
	if t, err := time.ParseInLocation("02-01-2006", s, market.IST); err == nil {
		return market.DateOf(t), nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return market.DateOf(time.Unix(n, 0)), nil
	}
	return market.Date{}, fmt.Errorf("unreadable expiry %q", s)
}

// Contracts are the tickers of one expiry's expired contracts.
type Contracts struct {
	Futures, Options []string
}

// ExpiredContracts lists every contract of an underlying that expired on a date.
func (c *Client) ExpiredContracts(ctx context.Context, underlying string, expiry market.Date) (Contracts, error) {
	var r struct {
		Data struct {
			Contracts Contracts `json:"contracts"`
		} `json:"data"`
	}
	q := url.Values{"symbol": {underlying}, "expiry_date": {expiry.String()}}
	if err := c.get(ctx, "/history/fno/expired/underlying-symbols", q, false, &r); err != nil {
		return Contracts{}, fmt.Errorf("%s contracts for %s: %w", underlying, expiry, err)
	}
	return r.Data.Contracts, nil
}

// ExpiredHistory returns an expired contract's candles with open interest over
// one window of at most 100 days for minute candles, oldest first.
func (c *Client) ExpiredHistory(ctx context.Context, symbol string, res Resolution, from, to market.Date) ([]Candle, error) {
	if days := int(to.At(0, 0).Sub(from.At(0, 0)).Hours()/24) + 1; days > res.maxDays() {
		return nil, fmt.Errorf("window of %d days is wider than the %d Fyers allows", days, res.maxDays())
	}
	var r struct {
		Columns []string    `json:"columns"`
		Candles [][]float64 `json:"candles"`
	}
	q := url.Values{"symbol": {symbol}, "resolution": {string(res)}, "date_format": {"1"},
		"range_from": {from.String()}, "range_to": {to.String()}, "include_oi": {"1"}, "include_greeks": {"0"}}
	if err := c.get(ctx, "/history/fno/expired/historical-data", q, res != Day, &r); err != nil {
		return nil, fmt.Errorf("%s %s..%s: %w", symbol, from, to, err)
	}
	oiAt := -1
	for i, col := range r.Columns {
		if col == "open_interest" {
			oiAt = i
		}
	}
	if len(r.Columns) > 0 {
		for i, want := range []string{"timestamp", "open", "high", "low", "close", "volume"} {
			if i >= len(r.Columns) || r.Columns[i] != want {
				return nil, fmt.Errorf("%s: unexpected columns %v", symbol, r.Columns)
			}
		}
	}
	out := make([]Candle, 0, len(r.Candles))
	for _, row := range r.Candles {
		cd, err := candle(row, oiAt)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", symbol, err)
		}
		out = append(out, cd)
	}
	return out, nil
}

// MasterRow is one live contract from a symbol master file.
type MasterRow struct {
	Ticker     string
	Underlying string // e.g. NIFTY
	LotSize    int64
	Expiry     market.Date
	Strike     float64
	Right      string // CE, PE or XX for futures
}

// SymbolMaster reads a segment's live contracts, such as NSE_FO or BSE_FO.
// The file has no header; the columns used here are documented positions.
func (c *Client) SymbolMaster(ctx context.Context, segment string) ([]MasterRow, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.PublicURL+"/sym_details/"+url.PathEscape(segment)+".csv", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("symbol master %s: %w", segment, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("symbol master %s: HTTP %d", segment, resp.StatusCode)
	}
	return parseMaster(resp.Body)
}

// Symbol master columns: 0 token, 1 name, 2 instrument type, 3 lot size,
// 4 tick, 5 ISIN, 6 hours, 7 last update, 8 expiry (epoch), 9 ticker,
// 10 exchange, 11 segment, 12 scrip code, 13 underlying, 14 underlying token,
// 15 strike, 16 option type.
func parseMaster(r io.Reader) ([]MasterRow, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	var out []MasterRow
	for line := 1; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("symbol master line %d: %w", line, err)
		}
		if len(rec) < 17 {
			continue
		}
		lot, err1 := strconv.ParseInt(strings.TrimSpace(rec[3]), 10, 64)
		exp, err2 := strconv.ParseInt(strings.TrimSpace(rec[8]), 10, 64)
		strike, err3 := strconv.ParseFloat(strings.TrimSpace(rec[15]), 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue // a header or a malformed row
		}
		out = append(out, MasterRow{Ticker: rec[9], Underlying: strings.TrimSpace(rec[13]), LotSize: lot,
			Expiry: market.DateOf(time.Unix(exp, 0)), Strike: strike, Right: strings.TrimSpace(rec[16])})
	}
}
