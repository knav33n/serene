package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"serene/internal/data"
	"serene/internal/fobhav"
	"serene/internal/fyers"
	"serene/internal/market"
)

func d(y int, m time.Month, day int) market.Date { return market.Date{Year: y, Month: m, Day: day} }

// fakeFyers serves an index at 22000 and one expiry, 2025-01-09, whose
// contracts trade on the five weekdays before it.
func fakeFyers(t *testing.T, expiredCalls *atomic.Int32) *fyers.Client {
	t.Helper()
	expiry := d(2025, 1, 9)
	row := func(day market.Date, px float64) string {
		return fmt.Sprintf("[%d,%g,%g,%g,%g,100,0]", day.At(9, 15).Unix(), px, px, px, px)
	}
	h := http.NewServeMux()
	h.HandleFunc("/history", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		from, _ := market.ParseDate(q.Get("range_from"))
		to, _ := market.ParseDate(q.Get("range_to"))
		var rows []string
		for day := from; !to.Before(day); day = day.AddDays(1) {
			if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday && !day.Before(d(2024, 12, 1)) {
				rows = append(rows, row(day, 22000))
			}
		}
		fmt.Fprintf(w, `{"s":"ok","code":200,"candles":[%s]}`, strings.Join(rows, ","))
	})
	h.HandleFunc("/history/fno/expired/expiry-dates", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"s":"ok","code":200,"data":{"expiry_dates":{"futures":[],"options":["2025-01-09"]}}}`)
	})
	h.HandleFunc("/history/fno/expired/underlying-symbols", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("expiry_date") != expiry.String() {
			t.Errorf("contracts asked for %s", r.URL.Query().Get("expiry_date"))
		}
		fmt.Fprint(w, `{"s":"ok","code":200,"data":{"contracts":{"futures":[],"options":[
			"NSE:NIFTY2510922000CE","NSE:NIFTY2510922000PE","NSE:NIFTY2510930000CE","NSE:NIFTY25JAN22000CE","NSE:BANKNIFTY2510922000CE"]}}}`)
	})
	h.HandleFunc("/history/fno/expired/historical-data", func(w http.ResponseWriter, r *http.Request) {
		expiredCalls.Add(1)
		q := r.URL.Query()
		from, _ := market.ParseDate(q.Get("range_from"))
		to, _ := market.ParseDate(q.Get("range_to"))
		var rows []string
		for day := expiry.AddDays(-6); !expiry.Before(day); day = day.AddDays(1) {
			if !day.Before(from) && !to.Before(day) && day.Weekday() != time.Saturday && day.Weekday() != time.Sunday {
				rows = append(rows, strings.Replace(row(day, 150), ",0]", ",4200]", 1))
			}
		}
		fmt.Fprintf(w, `{"s":"ok","columns":["timestamp","open","high","low","close","volume","open_interest"],"candles":[%s]}`,
			strings.Join(rows, ","))
	})
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	lim, err := fyers.NewLimiter(fyers.Limits{Name: "t", PerSecond: 1000, PerMinute: 100000, PerDay: 100000}, "")
	if err != nil {
		t.Fatal(err)
	}
	c := fyers.New("APP-100", "tok", lim)
	c.DataURL, c.Backoff = s.URL, time.Millisecond
	return c
}

func TestIndexThenBackfill(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := data.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var expired atomic.Int32
	c := fakeFyers(t, &expired)
	cfg := Config{Dir: dir, BandPct: 5, Workers: 3, Log: t.Logf,
		Now: func() time.Time { return d(2025, 1, 20).At(18, 0) }}
	u := fyers.Indices["NIFTY"]

	if err := Backfill(ctx, cfg, store, c, u, d(2025, 1, 1), d(2025, 1, 31)); err == nil {
		t.Fatal("backfill ran without index data")
	}
	if _, err := Index(ctx, cfg, store, c, u, d(2024, 12, 1)); err != nil {
		t.Fatal(err)
	}
	// Without a lot size the expiry is skipped and left unfinished.
	if err := Backfill(ctx, cfg, store, c, u, d(2025, 1, 1), d(2025, 1, 31)); err != nil {
		t.Fatalf("backfill without a lot size: %v", err)
	}
	if ok, _ := readJSON(filepath.Join(dir, "fetch", "NIFTY", "2025-01-09.json"), &ExpiryRecord{}); ok {
		t.Fatal("an expiry without a lot size was marked finished")
	}
	if err := store.PutLots(ctx, "NIFTY", []data.Lot{{From: d(2024, 11, 20), Size: 75, Source: "manual"}}); err != nil {
		t.Fatal(err)
	}
	if err := Backfill(ctx, cfg, store, c, u, d(2025, 1, 1), d(2025, 1, 31)); err != nil {
		t.Fatal(err)
	}

	// The 30000 strike is outside the band and the monthly-style ticker names
	// the same contract as the weekly one, so two contracts are fetched.
	cs, err := store.Contracts(ctx, "NIFTY")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].LotSize != 75 || cs[0].Key() != "NFO:NIFTY:2025-01-09:22000:CE" {
		t.Fatalf("contracts %+v", cs)
	}
	bars, err := store.DayBars(ctx, "NIFTY", d(2025, 1, 8))
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]int64{}
	for _, b := range bars {
		keys[b.Key] = b.OI
	}
	if len(keys) != 3 || keys["NFO:NIFTY:2025-01-09:22000:PE"] != 4200 {
		t.Fatalf("bars on expiry eve: %v", keys)
	}
	if _, ok := keys["NSE:NIFTY"]; !ok {
		t.Fatal("no index bar")
	}
	exp, err := store.StoredExpiries(ctx, "NIFTY", data.ExpiryOption)
	if err != nil || len(exp) != 1 || exp[0] != d(2025, 1, 9) {
		t.Fatalf("expiries %v %v", exp, err)
	}
	daily, err := store.Daily(ctx, "NIFTY")
	if err != nil || len(daily) == 0 || daily[0].Close != market.Rupees(22000) {
		t.Fatalf("daily %v %v", len(daily), err)
	}

	var rec ExpiryRecord
	if ok, err := readJSON(filepath.Join(dir, "fetch", "NIFTY", "2025-01-09.json"), &rec); !ok || err != nil {
		t.Fatalf("manifest %v %v", ok, err)
	}
	if rec.Listed != 5 || len(rec.Unparsed) != 1 || len(rec.Duplicates) != 1 || len(rec.Contracts) != 2 {
		b, _ := json.Marshal(rec)
		t.Fatalf("manifest %s", b)
	}

	// A finished expiry is not fetched again.
	before := expired.Load()
	if err := Backfill(ctx, cfg, store, c, u, d(2025, 1, 1), d(2025, 1, 31)); err != nil {
		t.Fatal(err)
	}
	if expired.Load() != before {
		t.Fatalf("refetched a finished expiry: %d more requests", expired.Load()-before)
	}
}

func TestLotOn(t *testing.T) {
	lots := []data.Lot{{From: d(2019, 1, 1), Size: 75}, {From: d(2021, 7, 1), Size: 50}}
	for _, c := range []struct {
		e    market.Date
		want int64
		ok   bool
	}{{d(2018, 12, 27), 0, false}, {d(2019, 1, 3), 75, true}, {d(2021, 7, 1), 50, true}, {d(2024, 1, 4), 50, true}} {
		if got, ok := data.LotOn(lots, c.e); got != c.want || ok != c.ok {
			t.Errorf("%s: %d %v", c.e, got, ok)
		}
	}
}

// TestBackfillFromBhavcopy covers a stock Fyers no longer lists: expiries,
// strikes, prices and lots come from the bhavcopy, the contracts are fetched
// by constructed ticker, and a strike Fyers never carried just has no bars.
func TestBackfillFromBhavcopy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	expiry := d(2019, 1, 31)
	h := http.NewServeMux()
	h.HandleFunc("/history/fno/expired/expiry-dates", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprint(w, `{"s":"error","code":-50,"message":"Invalid input"}`)
	})
	var asked []string
	var mu sync.Mutex
	h.HandleFunc("/history/fno/expired/historical-data", func(w http.ResponseWriter, r *http.Request) {
		sym := r.URL.Query().Get("symbol")
		mu.Lock()
		asked = append(asked, sym)
		mu.Unlock()
		if sym == "NSE:M&M19JAN740CE" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"s":"error","code":-50,"message":"Invalid input"}`)
			return
		}
		ts := expiry.At(9, 15).Unix()
		fmt.Fprintf(w, `{"s":"ok","columns":["timestamp","open","high","low","close","volume","open_interest"],"candles":[[%d,10,10,10,10,5,100]]}`, ts)
	})
	s := httptest.NewServer(h)
	defer s.Close()
	lim, _ := fyers.NewLimiter(fyers.Limits{Name: "t", PerSecond: 1000, PerMinute: 100000, PerDay: 100000}, "")
	c := fyers.New("APP-100", "tok", lim)
	c.DataURL, c.Backoff = s.URL, time.Millisecond

	sym := fobhav.Symbol{Symbol: "M&M", Exchange: "NSE", First: "2018-12-01", Last: "2019-01-31",
		Closes: map[string]float64{"2019-01-10": 700, "2019-01-24": 710},
		Expiries: []fobhav.Expiry{{Expiry: "2019-01-31", Lot: 1000, LotSource: fobhav.SourceDerived,
			Calls: []float64{500, 700, 740}, Puts: []float64{700}, Future: true}}}
	b, _ := json.Marshal(sym)
	if err := os.MkdirAll(filepath.Join(dir, "bhav_fo", "derived"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bhav_fo", "derived", "M&M.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := data.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := Config{Dir: dir, BandPct: 5, Workers: 2, Log: t.Logf, Now: func() time.Time { return d(2026, 1, 1).At(12, 0) }}
	u := fyers.Lookup("M&M")
	if u.Name != "M_M" || u.Exch() != "M&M" {
		t.Fatalf("lookup %+v", u)
	}
	if err := Backfill(ctx, cfg, store, c, u, d(2018, 1, 1), d(2026, 1, 1)); err != nil {
		t.Fatal(err)
	}
	cs, err := store.Contracts(ctx, "M_M")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]int64{}
	for _, c := range cs {
		keys[c.Key()] = c.LotSize
	}
	// 500 is outside the band, 740 has no data; the future is kept.
	if len(keys) != 3 || keys["NFO:M_M:2019-01-31:700:CE"] != 1000 || keys["NFO:M_M:2019-01-31:700:PE"] != 1000 {
		t.Fatalf("contracts %v (asked %v)", keys, asked)
	}
	var rec ExpiryRecord
	if ok, err := readJSON(filepath.Join(dir, "fetch", "M_M", "2019-01-31.json"), &rec); !ok || err != nil {
		t.Fatalf("manifest %v %v", ok, err)
	}
	if rec.Source != "bhavcopy" || rec.Listed != 4 || len(rec.Contracts) != 4 {
		t.Fatalf("manifest %+v", rec)
	}
	lots, err := store.Lots(ctx, "M_M")
	if err != nil || len(lots) != 1 || lots[0].Source != fobhav.SourceDerived {
		t.Fatalf("lots %v %v", lots, err)
	}
}
