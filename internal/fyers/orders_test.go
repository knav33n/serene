package fyers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func trader(t *testing.T, h http.HandlerFunc) *Trader {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tr := NewTrader("APP-100", "tok")
	tr.BaseURL = srv.URL
	return tr
}

func TestPlaceOrderBody(t *testing.T) {
	var got map[string]any
	tr := trader(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/orders/sync" || r.Header.Get("Authorization") != "APP-100:tok" {
			t.Errorf("%s %s auth %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Write([]byte(`{"s":"ok","code":1101,"message":"Order submitted","id":"808"}`))
	})
	id, err := tr.PlaceOrder(context.Background(), OrderRequest{Symbol: "NSE:NIFTY26N2425400CE", Side: "SELL", Type: "LIMIT",
		Product: "INTRADAY", Qty: 75, Price: 104.4, Tag: "serenemanual"})
	if err != nil || id != "808" {
		t.Fatal(id, err)
	}
	want := map[string]any{"symbol": "NSE:NIFTY26N2425400CE", "qty": 75.0, "type": 1.0, "side": -1.0, "productType": "INTRADAY",
		"limitPrice": 104.4, "validity": "DAY", "offlineOrder": false, "orderTag": "serenemanual"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestMarketOrderHasNoPrice(t *testing.T) {
	var got map[string]any
	tr := trader(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Write([]byte(`{"s":"ok","id":"9"}`))
	})
	if _, err := tr.PlaceOrder(context.Background(), OrderRequest{Symbol: "X", Side: "BUY", Type: "MARKET", Product: "INTRADAY", Qty: 75, Price: 50}); err != nil {
		t.Fatal(err)
	}
	if got["type"] != 2.0 || got["side"] != 1.0 || got["limitPrice"] != 0.0 {
		t.Fatal(got)
	}
}

func TestOrderStatusAndTag(t *testing.T) {
	tr := trader(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("id") != "808" {
			t.Errorf("query %q", r.URL.RawQuery)
		}
		w.Write([]byte(`{"s":"ok","orderBook":[{"id":"808","status":2,"filledQty":75,"tradedPrice":100.5,"orderTag":"1:serenemanual","side":1}]}`))
	})
	o, err := tr.Order(context.Background(), "808")
	if err != nil || !o.Done() || o.FilledQty != 75 || o.Tag() != "serenemanual" {
		t.Fatal(o, err)
	}
	if (Order{OrderTag: "2:Untagged"}).Tag() != "" || (Order{Status: 6}).Done() {
		t.Fatal("untagged or pending")
	}
}

func TestFundsAndAuth(t *testing.T) {
	tr := trader(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/funds" {
			w.Write([]byte(`{"s":"ok","fund_limit":[{"id":1,"title":"Total Balance","equityAmount":900},{"id":10,"title":"Available Balance","equityAmount":512340.5}]}`))
			return
		}
		w.WriteHeader(401)
		w.Write([]byte(`{"s":"error","code":-16,"message":"token expired"}`))
	})
	f, err := tr.Funds(context.Background())
	if err != nil || f != 512340.5 {
		t.Fatal(f, err)
	}
	if _, err := tr.Positions(context.Background()); !errors.Is(err, ErrAuth) {
		t.Fatal(err)
	}
}
