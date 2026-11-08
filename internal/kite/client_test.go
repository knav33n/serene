package kite

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseInstruments(t *testing.T) {
	csv := "instrument_token,exchange_token,tradingsymbol,name,last_price,expiry,strike,tick_size,lot_size,instrument_type,segment,exchange\n" +
		"146796039,573422,CRUDEOIL26NOVFUT,\"CRUDEOIL\",0,2026-11-19,0,1,1,FUT,MCX-FUT,MCX\n" +
		"273417,1068,MCXGOLDEX,\"MCXGOLDEX\",0,,0,0,1,EQ,INDICES,MCX\n"
	got, err := ParseInstruments([]byte(csv))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows", len(got))
	}
	f := got[0]
	if f.Token != 146796039 || f.ExchangeToken != "573422" || f.Name != "CRUDEOIL" || f.Type != "FUT" ||
		f.Expiry.String() != "2026-11-19" {
		t.Errorf("future parsed as %+v", f)
	}
	if !got[1].Expiry.IsZero() {
		t.Errorf("index has expiry %s", got[1].Expiry)
	}
}

func TestCandles(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token key:tok" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		query = r.URL.RawQuery
		w.Write([]byte(`{"status":"success","data":{"candles":[["2026-11-06T09:00:00+0530",8665,8700.5,8600,8459,50108,7486]]}}`))
	}))
	defer srv.Close()
	c := New("key", "tok")
	c.BaseURL, c.Interval = srv.URL, 0
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	cs, err := c.Candles(context.Background(), 1, "day", from, from.Add(time.Hour), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].High != 8700.5 || cs[0].Volume != 50108 || cs[0].OI != 7486 {
		t.Fatalf("got %+v", cs)
	}
	if want := time.Date(2026, 11, 6, 3, 30, 0, 0, time.UTC); !cs[0].Time.Equal(want) {
		t.Errorf("time %s, want %s", cs[0].Time, want)
	}
	if want := "continuous=1&from=2026-11-01+05%3A30%3A00&oi=1&to=2026-11-01+06%3A30%3A00"; query != want {
		t.Errorf("query %s, want %s", query, want)
	}
	c.Token = "bad"
	if _, err := c.Candles(context.Background(), 1, "day", from, from, false); err != ErrAuth {
		t.Errorf("bad token gave %v", err)
	}
}
