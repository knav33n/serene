// Package nse downloads and stores NSE's daily all-index file.
//
// One file, ind_close_all_DDMMYYYY.csv, holds every Nifty index for a day, with
// OHLC, volume, P/E, P/B and yield. It is published around 23:20 IST and needs no
// login. Raw files are kept, and the Parquet store is rebuilt from them, so a
// corrected alias or parser never needs another download.
package nse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"calm/internal/market"
)

// ErrNotPublished means no file exists for a date: a weekend, a holiday, or
// tonight's file before it has landed.
var ErrNotPublished = errors.New("no file published")

// Mirrors serve the same file. The NSE archive comes first because it answers a
// missing file with a real 404, where niftyindices sends 200 and an HTML page.
// The archive answers HEAD with 503, so only GET is used.
var Mirrors = []string{
	"https://archives.nseindia.com/content/indices",
	"https://www.niftyindices.com/Daily_Snapshot",
}

// Client fetches one file at a time, evenly paced.
type Client struct {
	Bases    []string
	Interval time.Duration
	Backoff  time.Duration // first retry delay
	Tries    int
	HTTP     *http.Client

	last     time.Time
	Requests int
}

// New returns a client with default pacing.
func New() *Client {
	return &Client{Bases: Mirrors, Interval: 500 * time.Millisecond, Backoff: 5 * time.Second, Tries: 4,
		HTTP: &http.Client{Timeout: 30 * time.Second}}
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

// fileName is the published name for a date.
func fileName(d market.Date) string {
	return fmt.Sprintf("ind_close_all_%02d%02d%04d.csv", d.Day, int(d.Month), d.Year)
}

// once makes one request. retry is true for failures that may pass.
func (c *Client) once(ctx context.Context, url string) (body []byte, retry bool, err error) {
	if err := c.pace(ctx); err != nil {
		return nil, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", "calm/0.1")
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
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("HTTP %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, false, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	switch text := bytes.TrimSpace(bytes.TrimPrefix(body, utf8BOM)); {
	case bytes.HasPrefix(text, []byte("Index Name")):
		return body, false, nil
	case bytes.HasPrefix(text, []byte("<")):
		return nil, false, ErrNotPublished // niftyindices' page for a missing file
	}
	return nil, false, errors.New("response is not the index file")
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
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
	return nil, fmt.Errorf("gave up after %d tries: %w", c.Tries, last)
}

// Fetch returns a day's file, trying each mirror. It returns ErrNotPublished
// only if every mirror tried says so. A mirror that fails hides nothing, as the
// next one may have the file. With firstOnly, one mirror answers, which keeps
// probing weekends for rare special sessions cheap.
func (c *Client) Fetch(ctx context.Context, d market.Date, firstOnly bool) ([]byte, error) {
	bases := c.Bases
	if firstOnly {
		bases = bases[:1]
	}
	var failure error
	for _, base := range bases {
		body, err := c.get(ctx, base+"/"+fileName(d))
		switch {
		case err == nil:
			return body, nil
		case errors.Is(err, ErrNotPublished):
		case ctx.Err() != nil:
			return nil, ctx.Err()
		default:
			failure = fmt.Errorf("%s: %w", base, err)
		}
	}
	if failure != nil {
		return nil, failure
	}
	return nil, ErrNotPublished
}
