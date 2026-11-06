package upstox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
