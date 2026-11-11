package ingest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"serene/internal/data"
	"serene/internal/market"
	"serene/internal/upstox"
)

// Two weekly expiries with lot sizes that differ, like 2024's 25 and today's 65.
const (
	olderExpiry = "2026-07-28"
	newerExpiry = "2026-08-04"
)

type fake struct {
	mu        sync.Mutex
	calls     map[string]int
	failAfter int // contract candle calls before returning 401
	candles   int
	busy      int // 429s to send first
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.EscapedPath()
	f.calls[path]++
	if f.busy > 0 {
		f.busy--
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	switch {
	case strings.HasSuffix(path, "/expiries"):
		fmt.Fprintf(w, `{"status":"success","data":["%s","%s"]}`, olderExpiry, newerExpiry)
	case strings.HasSuffix(path, "/option/contract"):
		exp := r.URL.Query().Get("expiry_date")
		lot := 65
		if exp == olderExpiry {
			lot = 75
		}
		var items []string
		for _, s := range []struct {
			id     int
			strike float64
			right  string
		}{{1, 24000, "CE"}, {2, 24000, "PE"}, {3, 30000, "CE"}, {4, 24050, "CE"}} {
			items = append(items, fmt.Sprintf(`{"instrument_key":"NSE_FO|%d|%s","trading_symbol":"S%d","instrument_type":%q,"strike_price":%f,"lot_size":%d,"weekly":true}`,
				s.id, exp, s.id, s.right, s.strike, lot))
		}
		fmt.Fprintf(w, `{"status":"success","data":[%s]}`, strings.Join(items, ","))
	case strings.HasPrefix(path, "/v3/historical-candle/"):
		if !strings.Contains(path, "/minutes/1/2026-07-31/2026-07-01") && !strings.Contains(path, "/minutes/1/2026-08-31/2026-08-01") {
			fmt.Fprint(w, `{"status":"success","data":{"candles":[]}}`)
			return
		}
		day := "2026-07-28"
		if strings.Contains(path, "2026-08") {
			day = "2026-08-04"
		}
		fmt.Fprintf(w, `{"status":"success","data":{"candles":[["%sT09:16:00+05:30",24010.5,24020,24000,24012,0,0],["%sT09:15:00+05:30",24000,24011,23990,24010.5,0,0]]}}`, day, day)
	case strings.Contains(path, "/historical-candle/"):
		f.candles++
		if f.failAfter > 0 && f.candles > f.failAfter {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"status":"error","errors":[{"errorCode":"UDAPI100050","message":"Invalid token"}]}`)
			return
		}
		day := olderExpiry
		if strings.Contains(path, "2026-08-04/1minute") || strings.Contains(path, "04-08") {
			day = newerExpiry
		}
		if strings.Contains(path, "%7C4%7C") { // listed, but never traded
			fmt.Fprint(w, `{"status":"success","data":{"candles":[]}}`)
			return
		}
		// Includes a closing-auction bar, and a pre-open bar to drop.
		fmt.Fprintf(w, `{"status":"success","data":{"candles":[["%sT15:35:00+05:30",1.5,2,1,1.5,10,20],["%sT10:00:00+05:30",100.05,101,99.95,100.5,500,9000],["%sT08:00:00+05:30",1,1,1,1,0,0]]}}`, day, day, day)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func setup(t *testing.T, f *fake) (*upstox.Client, *data.Store, Config) {
	t.Helper()
	f.calls = map[string]int{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	store, err := data.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	c := upstox.New("tok")
	c.BaseURL, c.Interval, c.Backoff = srv.URL, 0, time.Millisecond
	from, _ := market.ParseDate("2024-01-01")
	to, _ := market.ParseDate("2026-12-31")
	return c, store, Config{Underlying: "NIFTY", Dir: dir, BandPct: 5, From: from, To: to, Log: func(string, ...any) {}}
}

func TestDownload(t *testing.T) {
	ctx := context.Background()
	f := &fake{}
	c, store, cfg := setup(t, f)
	if err := Run(ctx, cfg, store, c); err != nil {
		t.Fatal(err)
	}
	d, _ := market.ParseDate(olderExpiry)
	bars, err := store.DayBars(ctx, "NIFTY", d)
	if err != nil {
		t.Fatal(err)
	}
	// Index: 2. Options on 2026-07-28: CE and PE, 09:15-session bars only.
	byKey := map[string][]market.Bar{}
	for _, b := range bars {
		byKey[b.Key] = append(byKey[b.Key], b)
	}
	ce := "NFO:NIFTY:" + olderExpiry + ":24000:CE"
	if len(byKey["NSE:NIFTY"]) != 2 || len(byKey[ce]) != 1 {
		t.Fatalf("bars %v", byKey)
	}
	got := byKey[ce][0]
	if got.Open != 10005 || got.High != 10100 || got.Low != 9995 || got.Close != 10050 || got.Volume != 500 || got.OI != 9000 {
		t.Errorf("candle not preserved: %+v", got)
	}
	if !got.Time.Equal(d.At(10, 0)) {
		t.Errorf("time %s", got.Time)
	}

	cs, err := store.Contracts(ctx, "NIFTY")
	if err != nil {
		t.Fatal(err)
	}
	lots := map[string]int64{}
	for _, c := range cs {
		lots[c.Expiry.String()] = c.LotSize
		if c.Strike == market.Rupees(30000) {
			t.Error("a strike outside the band was fetched")
		}
	}
	if lots[olderExpiry] != 75 || lots[newerExpiry] != 65 {
		t.Errorf("lot sizes must come from each contract: %v", lots)
	}

	// The closing auction bar is kept only after the session change.
	d2, _ := market.ParseDate(newerExpiry)
	later, _ := store.DayBars(ctx, "NIFTY", d2)
	found := false
	for _, b := range later {
		found = found || b.Time.Equal(d2.At(15, 35))
	}
	if !found {
		t.Error("15:35 bar on a 15:40 close day was lost")
	}

	cov, err := Report(ctx, cfg.Dir, "NIFTY", store)
	if err != nil {
		t.Fatal(err)
	}
	if len(cov.Expiries) != 2 {
		t.Fatalf("expiries %+v", cov.Expiries)
	}
	e := cov.Expiries[0]
	if e.Listed != 4 || e.Selected != 3 || e.Empty != 1 || e.Expiry != olderExpiry {
		t.Errorf("coverage %+v", e)
	}
	if e.Dropped == 0 {
		t.Error("dropped candles not counted")
	}
	if len(cov.Short) != 2 || len(cov.NoIndex) != 0 {
		t.Errorf("short %v none %v", cov.Short, cov.NoIndex)
	}
}

func TestResumeSkipsFinishedWork(t *testing.T) {
	ctx := context.Background()
	f := &fake{}
	c, store, cfg := setup(t, f)
	if err := Run(ctx, cfg, store, c); err != nil {
		t.Fatal(err)
	}
	first := f.candles
	if err := Run(ctx, cfg, store, c); err != nil {
		t.Fatal(err)
	}
	if f.candles != first {
		t.Errorf("finished expiries were fetched again: %d then %d", first, f.candles)
	}
	for path, n := range f.calls {
		if strings.Contains(path, "/v3/") && n > 1 {
			t.Errorf("finished month fetched twice: %s", path)
		}
	}
}

func TestExpiredTokenStopsAndResumes(t *testing.T) {
	ctx := context.Background()
	f := &fake{failAfter: 4}
	c, store, cfg := setup(t, f)
	err := Run(ctx, cfg, store, c)
	if err == nil || !strings.Contains(err.Error(), "run serene auth") {
		t.Fatalf("got %v", err)
	}
	cov, _ := Report(ctx, cfg.Dir, "NIFTY", store)
	if len(cov.Expiries) != 1 || cov.Expiries[0].Expiry != newerExpiry {
		t.Fatalf("newest expiry should finish first: %+v", cov.Expiries)
	}
	f.failAfter = 0
	if err := Run(ctx, cfg, store, c); err != nil {
		t.Fatal(err)
	}
	cov, _ = Report(ctx, cfg.Dir, "NIFTY", store)
	if len(cov.Expiries) != 2 {
		t.Fatalf("resume incomplete: %+v", cov.Expiries)
	}
}

func TestRateLimitIsRetried(t *testing.T) {
	f := &fake{busy: 3}
	c, store, cfg := setup(t, f)
	if err := Run(context.Background(), cfg, store, c); err != nil {
		t.Fatal(err)
	}
}

func TestCoverageFlagsMissingExpiries(t *testing.T) {
	ctx := context.Background()
	f := &fake{}
	c, store, cfg := setup(t, f)
	if err := Run(ctx, cfg, store, c); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(manifestDir(cfg)+"/2026-06-02.json", ExpiryRecord{Expiry: "2026-06-02"}); err != nil {
		t.Fatal(err)
	}
	cov, err := Report(ctx, cfg.Dir, "NIFTY", store)
	if err != nil {
		t.Fatal(err)
	}
	if len(cov.Gaps) != 1 || !strings.Contains(cov.Gaps[0], "2026-06-02") {
		t.Errorf("gaps %v", cov.Gaps)
	}
}

func TestCoverageFlagsEmptyExpiries(t *testing.T) {
	ctx := context.Background()
	f := &fake{}
	c, store, cfg := setup(t, f)
	if err := Run(ctx, cfg, store, c); err != nil {
		t.Fatal(err)
	}
	rec := ExpiryRecord{Expiry: "2026-06-02", Contracts: []ContractRecord{{Key: "a"}, {Key: "b"}}}
	if err := writeJSON(manifestDir(cfg)+"/2026-06-02.json", rec); err != nil {
		t.Fatal(err)
	}
	cov, err := Report(ctx, cfg.Dir, "NIFTY", store)
	if err != nil {
		t.Fatal(err)
	}
	if len(cov.Empty) != 1 || cov.Empty[0] != "2026-06-02" {
		t.Errorf("empty %v", cov.Empty)
	}
}
