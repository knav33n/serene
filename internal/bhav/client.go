// Package bhav downloads and stores NSE's daily cash market bhavcopy: OHLC,
// volume and turnover for every security traded that day.
//
// NSE has published it in two formats. The legacy file cmDDMONYYYYbhav.csv
// runs from the first trading day, 1994-11-03, to 2024-07-05. The UDiFF file
// BhavCopy_NSE_CM_0_0_0_YYYYMMDD_F_0000.csv runs from 2024-07-08. Both come
// zipped and need no login. Raw zips are kept, and the Parquet store is
// rebuilt from them, so a corrected parser never needs another download.
package bhav

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"serene/internal/market"
)

// ErrNotPublished means no file exists for a date: a weekend, a holiday, or
// today's file before it has landed.
var ErrNotPublished = errors.New("no file published")

// FirstDay is NSE's first cash market session.
var FirstDay = market.Date{Year: 1994, Month: time.November, Day: 3}

// UDiFFFrom is the first day of the UDiFF format.
var UDiFFFrom = market.Date{Year: 2024, Month: time.July, Day: 8}

const base = "https://nsearchives.nseindia.com/content"

// URL is where a day's file is published.
func URL(d market.Date) string {
	if d.Before(UDiFFFrom) {
		mon := strings.ToUpper(d.Month.String()[:3])
		return fmt.Sprintf("%s/historical/EQUITIES/%d/%s/cm%02d%s%dbhav.csv.zip", base, d.Year, mon, d.Day, mon, d.Year)
	}
	return fmt.Sprintf("%s/cm/BhavCopy_NSE_CM_0_0_0_%04d%02d%02d_F_0000.csv.zip", base, d.Year, int(d.Month), d.Day)
}

// Client fetches one file at a time, evenly paced.
type Client struct {
	Interval time.Duration
	Backoff  time.Duration // first retry delay
	Tries    int
	HTTP     *http.Client
	URL      func(market.Date) string

	last     time.Time
	Requests int
}

// New returns a client with default pacing.
func New() *Client {
	return &Client{Interval: 500 * time.Millisecond, Backoff: 5 * time.Second, Tries: 4,
		HTTP: &http.Client{Timeout: 60 * time.Second}, URL: URL}
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

var zipMagic = []byte("PK\x03\x04")

// once makes one request. retry is true for failures that may pass.
func (c *Client) once(ctx context.Context, url string) (body []byte, retry bool, err error) {
	if err := c.pace(ctx); err != nil {
		return nil, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", "serene/0.1")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, true, err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, true, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, false, ErrNotPublished
	// The archive answers bursts with 403 as well as 429.
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden || resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("HTTP %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, false, fmt.Errorf("HTTP %d", resp.StatusCode)
	// Some early dates, such as 1995-09-06, are served as empty files.
	case len(body) == 0:
		return nil, false, ErrNotPublished
	case !bytes.HasPrefix(body, zipMagic):
		return nil, false, errors.New("response is not a zip file")
	}
	return body, false, nil
}

// Fetch returns a day's zipped file.
func (c *Client) Fetch(ctx context.Context, d market.Date) ([]byte, error) {
	url := c.URL(d)
	delay := c.Backoff
	var last error
	for try := range c.Tries {
		if try > 0 {
			if err := sleep(ctx, delay); err != nil {
				return nil, err
			}
			delay *= 2
		}
		body, retry, err := c.once(ctx, url)
		if err == nil || !retry {
			return body, err
		}
		last = err
	}
	return nil, fmt.Errorf("%s: gave up after %d tries: %w", url, c.Tries, last)
}
