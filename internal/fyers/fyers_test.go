package fyers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"serene/internal/market"
)

func d(y int, m time.Month, day int) market.Date { return market.Date{Year: y, Month: m, Day: day} }

func TestParseContract(t *testing.T) {
	nifty, sensex, sbin := Indices["NIFTY"], Indices["SENSEX"], Lookup("SBIN")
	cases := []struct {
		ticker string
		u      Underlying
		expiry market.Date
		want   string
	}{
		{"NSE:NIFTY25MAR23000CE", nifty, d(2025, 3, 27), "NFO:NIFTY:2025-03-27:23000:CE"},
		{"NSE:NIFTY2010811000CE", nifty, d(2020, 1, 8), "NFO:NIFTY:2020-01-08:11000:CE"},
		{"NSE:NIFTY20O0811000PE", nifty, d(2020, 10, 8), "NFO:NIFTY:2020-10-08:11000:PE"},
		{"NSE:NIFTY20D1025000CE", nifty, d(2020, 12, 10), "NFO:NIFTY:2020-12-10:25000:CE"},
		{"BSE:SENSEX2381161000CE", sensex, d(2023, 8, 11), "BFO:SENSEX:2023-08-11:61000:CE"},
		{"BSE:SENSEX23AUG60400CE", sensex, d(2023, 8, 25), "BFO:SENSEX:2023-08-25:60400:CE"},
		{"NSE:SBIN25MAR320PE", sbin, d(2025, 3, 27), "NFO:SBIN:2025-03-27:320:PE"},
		{"NSE:SBIN25MAR322.5CE", sbin, d(2025, 3, 27), "NFO:SBIN:2025-03-27:322.50:CE"},
		{"NSE:NIFTY25MARFUT", nifty, d(2025, 3, 27), "NFO:NIFTY:2025-03-27:FUT"},
	}
	for _, c := range cases {
		inst, err := ParseContract(c.ticker, c.u, c.expiry)
		if err != nil {
			t.Errorf("%s: %v", c.ticker, err)
			continue
		}
		if got := inst.Key(); got != c.want {
			t.Errorf("%s: got %s, want %s", c.ticker, got, c.want)
		}
		if back, err := market.ParseKey(inst.Key()); err != nil || back.Key() != c.want {
			t.Errorf("%s: key does not round-trip: %v", c.ticker, err)
		}
	}
	bad := []struct {
		ticker string
		u      Underlying
		expiry market.Date
	}{
		{"NSE:NIFTY25MAR23000CE", nifty, d(2025, 4, 24)},      // wrong month
		{"NSE:NIFTY2510923000CE", nifty, d(2025, 1, 16)},      // wrong day
		{"NSE:NIFTYNXT5025MAR23000CE", nifty, d(2025, 3, 27)}, // another index
		{"BSE:NIFTY25MAR23000CE", nifty, d(2025, 3, 27)},      // wrong exchange
		{"NSE:NIFTY25MAR23000XX", nifty, d(2025, 3, 27)},      // no right
	}
	for _, c := range bad {
		if inst, err := ParseContract(c.ticker, c.u, c.expiry); err == nil {
			t.Errorf("%s on %s: parsed as %s", c.ticker, c.expiry, inst.Key())
		}
	}
}

type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time { return f.t }
func (f *fakeClock) sleep(_ context.Context, d time.Duration) error {
	f.t = f.t.Add(d)
	return nil
}

func TestLimiterStaysUnderEveryCap(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 16, 10, 0, 0, 0, market.IST)}
	lim, err := NewLimiter(Limits{Name: "t", PerSecond: 3, PerMinute: 10, PerDay: 25}, "")
	if err != nil {
		t.Fatal(err)
	}
	lim.now, lim.sleep = clock.now, clock.sleep
	var sent []time.Time
	for range 25 {
		if err := lim.Wait(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, clock.t)
	}
	for i := range sent {
		inSec, inMin := 0, 0
		for _, s := range sent[:i+1] {
			if sent[i].Sub(s) < time.Second {
				inSec++
			}
			if sent[i].Sub(s) < time.Minute {
				inMin++
			}
		}
		if inSec > 3 || inMin > 10 {
			t.Fatalf("request %d: %d in a second, %d in a minute", i, inSec, inMin)
		}
	}
	if err := lim.Wait(context.Background(), false); !errors.Is(err, ErrBudget) {
		t.Fatalf("26th request today: %v", err)
	}
	clock.t = clock.t.Add(24 * time.Hour)
	if err := lim.Wait(context.Background(), false); err != nil {
		t.Fatalf("next day: %v", err)
	}
}

func TestLimiterBudgetSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	limits := Limits{Name: "t", PerSecond: 100, PerMinute: 1000, PerDay: 1000, ExpiredIntraday: 2}
	for range 2 {
		lim, err := NewLimiter(limits, path)
		if err != nil {
			t.Fatal(err)
		}
		if err := lim.Wait(context.Background(), true); err != nil {
			t.Fatal(err)
		}
	}
	lim, err := NewLimiter(limits, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := lim.Wait(context.Background(), true); !errors.Is(err, ErrBudget) {
		t.Fatalf("third expired request across runs: %v", err)
	}
	if err := lim.Wait(context.Background(), false); err != nil {
		t.Fatalf("ordinary request: %v", err)
	}
	if u := lim.Usage(); u.Requests != 3 || u.ExpiredIntraday != 2 {
		t.Fatalf("usage %+v", u)
	}
}

func serve(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	lim, err := NewLimiter(Limits{Name: "t", PerSecond: 1000, PerMinute: 100000, PerDay: 100000}, "")
	if err != nil {
		t.Fatal(err)
	}
	c := New("APP-100", "tok", lim)
	c.DataURL, c.PublicURL, c.Backoff = s.URL, s.URL, time.Millisecond
	return c
}

func TestHistorySplitsWindowsAndSendsAuth(t *testing.T) {
	var ranges []string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "APP-100:tok" || r.URL.Path != "/history" {
			t.Errorf("auth %q path %q", r.Header.Get("Authorization"), r.URL.Path)
		}
		q := r.URL.Query()
		ranges = append(ranges, q.Get("range_from")+".."+q.Get("range_to"))
		from, _ := market.ParseDate(q.Get("range_from"))
		ts := from.At(9, 15).Unix()
		fmt.Fprintf(w, `{"s":"ok","code":200,"candles":[[%d,100.5,101,100,100.75,10,7]]}`, ts)
	})
	cs, err := c.History(context.Background(), "NSE:NIFTY50-INDEX", Minute, d(2024, 1, 1), d(2024, 7, 1), true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2024-01-01..2024-04-09", "2024-04-10..2024-07-01"}
	if strings.Join(ranges, " ") != strings.Join(want, " ") {
		t.Fatalf("windows %v", ranges)
	}
	if len(cs) != 2 || cs[0].Close != 100.75 || cs[0].OI != 7 || !cs[0].Time.Equal(d(2024, 1, 1).At(9, 15)) {
		t.Fatalf("candles %+v", cs)
	}
}

func TestExpiredHistoryReadsColumnsByName(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/history/fno/expired/historical-data" || r.URL.Query().Get("include_oi") != "1" {
			t.Errorf("path %s query %s", r.URL.Path, r.URL.RawQuery)
		}
		fmt.Fprintf(w, `{"s":"ok","symbol":"x","columns":["timestamp","open","high","low","close","volume","open_interest"],
			"candles":[[%d,10,11,9,10.5,100,5000]]}`, d(2025, 3, 20).At(9, 15).Unix())
	})
	cs, err := c.ExpiredHistory(context.Background(), "NSE:NIFTY25MAR23000CE", Minute, d(2025, 3, 1), d(2025, 3, 27))
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].OI != 5000 || cs[0].Volume != 100 {
		t.Fatalf("candles %+v", cs)
	}
	if _, err := c.ExpiredHistory(context.Background(), "x", Minute, d(2025, 1, 1), d(2025, 6, 1)); err == nil {
		t.Fatal("a window over 100 days was sent")
	}
}

func TestNoDataIsEmpty(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"s":"no_data","code":200,"candles":[]}`)
	})
	cs, err := c.ExpiredHistory(context.Background(), "x", Minute, d(2025, 3, 1), d(2025, 3, 27))
	if err != nil || len(cs) != 0 {
		t.Fatalf("%v %v", cs, err)
	}
}

func TestFatalErrorsAreNotRetried(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   error
	}{
		"rate limit status": {429, `{"s":"error","code":-429,"message":"limit"}`, ErrRateLimited},
		"rate limit code":   {200, `{"s":"error","code":-429,"message":"limit"}`, ErrRateLimited},
		"expired token":     {200, `{"s":"error","code":-8,"message":"expired"}`, ErrAuth},
		"unauthorised":      {401, `{"s":"error","code":-16,"message":"no"}`, ErrAuth},
		"delisted spot":     {422, `{"s":"error","code":-300,"message":"Invalid symbol provided"}`, ErrNoData},
	} {
		n := 0
		c := serve(t, func(w http.ResponseWriter, r *http.Request) {
			n++
			w.WriteHeader(tc.status)
			fmt.Fprint(w, tc.body)
		})
		_, err := c.History(context.Background(), "x", Minute, d(2025, 1, 1), d(2025, 1, 2), false)
		if !errors.Is(err, tc.want) || n != 1 {
			t.Errorf("%s: err %v after %d calls", name, err, n)
		}
	}
}

func TestServerErrorsAreRetried(t *testing.T) {
	n := 0
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if n < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"s":"ok","code":200,"candles":[]}`)
	})
	if _, err := c.History(context.Background(), "x", Minute, d(2025, 1, 1), d(2025, 1, 2), false); err != nil || n != 3 {
		t.Fatalf("err %v calls %d", err, n)
	}
}

func TestExpiryDatesAcceptsDateFormats(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"s":"ok","code":200,"data":{"expiry_dates":{"futures":["2024-11-28"],
			"options":["2024-11-07","14-11-2024",%d]}}}`, d(2024, 11, 21).At(15, 30).Unix())
	})
	ex, err := c.ExpiryDates(context.Background(), "NSE:NIFTY50-INDEX", d(2024, 11, 1), d(2024, 11, 30))
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Futures) != 1 || len(ex.Options) != 3 || ex.Options[1] != d(2024, 11, 14) || ex.Options[2] != d(2024, 11, 21) {
		t.Fatalf("%+v", ex)
	}
}

func TestParseMaster(t *testing.T) {
	row := `101125032735012,NIFTY 27 Mar 25 23000 CE,14,75,0.05,,0915-1530|1815-1915:,2025-03-20,1743069600,NSE:NIFTY25MAR23000CE,10,11,35012,NIFTY,26000,23000.0,CE,101000000026000,None,0,0.0`
	rows, err := parseMaster(strings.NewReader(row + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Underlying != "NIFTY" || rows[0].LotSize != 75 || rows[0].Right != "CE" ||
		rows[0].Strike != 23000 || rows[0].Expiry != d(2025, 3, 27) {
		t.Fatalf("%+v", rows)
	}
}

func TestMonthlyTickerRoundTrips(t *testing.T) {
	u := Lookup("M&M")
	e := d(2019, 1, 31)
	for k, want := range map[float64]string{97.5: "NSE:M&M19JAN97.5CE", 700: "NSE:M&M19JAN700CE"} {
		got := MonthlyTicker(u, e, k, "CE")
		if got != want {
			t.Errorf("%g: %s, want %s", k, got, want)
		}
		inst, err := ParseContract(got, u, e)
		if err != nil || inst.Strike.Rupees() != k || inst.Underlying != "M_M" {
			t.Errorf("%s parsed to %+v %v", got, inst, err)
		}
	}
	if got := MonthlyTicker(Indices["BANKEX"], d(2023, 7, 28), 0, "FUT"); got != "BSE:BANKEX23JULFUT" {
		t.Errorf("future %s", got)
	}
}

func TestInvalidInputIsNoData(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprint(w, `{"s":"error","code":-50,"message":"Invalid input"}`)
	})
	_, err := c.History(context.Background(), "NSE:NIFTY50-INDEX", Minute, d(2017, 7, 3), d(2017, 7, 8), false)
	if !errors.Is(err, ErrNoData) {
		t.Fatalf("got %v", err)
	}
}
