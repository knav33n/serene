package upstox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"calm/internal/market"
)

func serve(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	c := New("tok")
	c.BaseURL, c.Interval, c.Backoff = s.URL, 0, time.Millisecond
	return c
}

func TestPlanErrorIsFatal(t *testing.T) {
	n := 0
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"status":"error","errors":[{"errorCode":"UDAPI1149","message":"Plus only"}]}`)
	})
	if _, err := c.Expiries(context.Background(), "k"); !errors.Is(err, ErrPlan) || n != 1 {
		t.Fatalf("err %v calls %d", err, n)
	}
}

func TestRetriesThenGivesUp(t *testing.T) {
	n := 0
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(http.StatusBadGateway)
	})
	if _, err := c.Expiries(context.Background(), "k"); err == nil || n != 7 {
		t.Fatalf("err %v calls %d", err, n)
	}
}

func TestPacing(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":[]}`)
	})
	c.Interval = 30 * time.Millisecond
	start := time.Now()
	for range 4 {
		if _, err := c.Expiries(context.Background(), "k"); err != nil {
			t.Fatal(err)
		}
	}
	if got := time.Since(start); got < 90*time.Millisecond {
		t.Errorf("4 requests took %s", got)
	}
}

func TestSendsTokenAndEscapesKey(t *testing.T) {
	var auth, path string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.EscapedPath()
		fmt.Fprint(w, `{"status":"success","data":{"candles":[["2026-10-27T15:29:00+05:30",1.5,2,1,1.75,"100",7]]}}`)
	})
	d, _ := market.ParseDate("2026-10-27")
	got, err := c.ExpiredCandles(context.Background(), "NSE_FO|51352|27-10-2026", d.AddDays(-1), d)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok" || path != "/v2/expired-instruments/historical-candle/NSE_FO%7C51352%7C27-10-2026/1minute/2026-10-27/2026-10-26" {
		t.Errorf("auth %q path %q", auth, path)
	}
	if len(got) != 1 || got[0].Close != 1.75 || got[0].Volume != 100 || got[0].OI != 7 {
		t.Errorf("candle %+v", got)
	}
}

// candleServer serves one candle per requested day range, failing with status
// for ranges longer than maxDays.
func candleServer(t *testing.T, maxDays int, status int) (*Client, *[]string) {
	t.Helper()
	var seen []string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		to, from := parts[len(parts)-2], parts[len(parts)-1]
		seen = append(seen, from+" "+to)
		a, _ := market.ParseDate(from)
		b, _ := market.ParseDate(to)
		if int(b.At(0, 0).Sub(a.At(0, 0)).Hours()/24) > maxDays {
			w.WriteHeader(status)
			return
		}
		fmt.Fprintf(w, `{"status":"success","data":{"candles":[["%sT09:15:00+05:30",1,1,1,1,1,1],["%sT09:15:00+05:30",2,2,2,2,2,2]]}}`, to, from)
	})
	return c, &seen
}

func day(s string) market.Date {
	d, err := market.ParseDate(s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestOversizedCandleWindowIsSplit(t *testing.T) {
	c, seen := candleServer(t, 25, http.StatusInternalServerError)
	cs, err := c.ExpiredCandles(context.Background(), "k", day("2025-01-01"), day("2025-04-10"))
	if err != nil {
		t.Fatal(err)
	}
	// Each piece gives its first and last day, so the pieces must meet exactly.
	for i := 1; i < len(cs); i += 2 {
		if i+1 < len(cs) && cs[i].Time.AddDate(0, 0, -1).Format("2006-01-02") != cs[i+1].Time.Format("2006-01-02") {
			t.Fatalf("gap or overlap between %v and %v", cs[i].Time, cs[i+1].Time)
		}
	}
	if got := cs[0].Time.Format("2006-01-02"); got != "2025-04-10" {
		t.Fatalf("newest %s", got)
	}
	if got := cs[len(cs)-1].Time.Format("2006-01-02"); got != "2025-01-01" {
		t.Fatalf("oldest %s", got)
	}
	// The full window is tried twice, then split, rather than backed off seven times.
	if (*seen)[0] != (*seen)[1] || (*seen)[2] == (*seen)[0] {
		t.Fatalf("requests %v", (*seen)[:3])
	}
}

func TestServerErrorOnOneDayStops(t *testing.T) {
	c, seen := candleServer(t, -1, http.StatusInternalServerError)
	_, err := c.ExpiredCandles(context.Background(), "k", day("2025-01-01"), day("2025-01-01"))
	if !errors.Is(err, ErrServer) || len(*seen) != candleTries {
		t.Fatalf("err %v calls %d", err, len(*seen))
	}
	// A wider window must fail too, not return the days that worked.
	c, _ = candleServer(t, -1, http.StatusInternalServerError)
	if cs, err := c.ExpiredCandles(context.Background(), "k", day("2025-01-01"), day("2025-01-08")); err == nil || cs != nil {
		t.Fatalf("candles %v err %v", cs, err)
	}
}

func TestRateLimitedCandlesAreNotSplit(t *testing.T) {
	c, seen := candleServer(t, 0, http.StatusTooManyRequests)
	_, err := c.ExpiredCandles(context.Background(), "k", day("2025-01-01"), day("2025-01-08"))
	if err == nil || len(*seen) != attempts {
		t.Fatalf("err %v calls %d", err, len(*seen))
	}
	for _, s := range *seen {
		if s != (*seen)[0] {
			t.Fatalf("window changed: %v", *seen)
		}
	}
}
