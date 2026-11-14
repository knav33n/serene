package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"serene/internal/data"
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
	if err := Backfill(ctx, cfg, store, c, u, d(2025, 1, 1), d(2025, 1, 31)); err == nil ||
		!strings.Contains(err.Error(), "no lot size") {
		t.Fatalf("backfill without a lot size: %v", err)
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
