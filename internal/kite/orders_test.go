package kite

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestOrderRequests(t *testing.T) {
	var got []string
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(b))
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+r.Header.Get("X-Kite-Version"))
		switch r.URL.Path {
		case "/orders/regular", "/orders/regular/7":
			w.Write([]byte(`{"status":"success","data":{"order_id":"7"}}`))
		case "/orders/7":
			w.Write([]byte(`{"status":"success","data":[{"order_id":"7","status":"OPEN"},{"order_id":"7","status":"COMPLETE","filled_quantity":65,"average_price":101.5,"tag":"hmr1117z01"}]}`))
		case "/user/margins/equity":
			w.Write([]byte(`{"status":"success","data":{"enabled":true,"net":54321.5,"available":{"live_balance":1}}}`))
		case "/portfolio/positions":
			w.Write([]byte(`{"status":"success","data":{"net":[{"tradingsymbol":"NIFTY25N1725900CE","exchange":"NFO","product":"MIS","quantity":65}],"day":[]}}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"status":"error","message":"Insufficient funds","error_type":"InputException"}`))
		}
	}))
	defer srv.Close()
	c := New("key", "tok")
	c.BaseURL = srv.URL
	ctx := context.Background()
	id, err := c.PlaceOrder(ctx, OrderRequest{Exchange: "NFO", Symbol: "NIFTY25N1725900CE", Side: "SELL", Type: "SL",
		Product: "MIS", Qty: 65, Price: 87.4, Trigger: 95, Tag: "hmr1117z02"})
	if err != nil || id != "7" {
		t.Fatal(id, err)
	}
	want := url.Values{"exchange": {"NFO"}, "tradingsymbol": {"NIFTY25N1725900CE"}, "transaction_type": {"SELL"},
		"order_type": {"SL"}, "quantity": {"65"}, "product": {"MIS"}, "validity": {"DAY"}, "price": {"87.40"},
		"trigger_price": {"95.00"}, "tag": {"hmr1117z02"}}
	if form.Encode() != want.Encode() {
		t.Fatalf("form %s", form.Encode())
	}
	if got[0] != "POST /orders/regular token key:tok 3" {
		t.Fatal(got[0])
	}
	if err := c.ModifyOrder(ctx, "7", OrderRequest{Type: "LIMIT", Qty: 130, Price: 102.05}); err != nil {
		t.Fatal(err)
	}
	if form.Encode() != (url.Values{"order_type": {"LIMIT"}, "quantity": {"130"}, "validity": {"DAY"}, "price": {"102.05"}}).Encode() {
		t.Fatalf("modify %s", form.Encode())
	}
	o, err := c.OrderStatus(ctx, "7")
	if err != nil || o.Status != "COMPLETE" || o.Filled != 65 || o.Average != 101.5 {
		t.Fatalf("%+v %v", o, err)
	}
	if f, err := c.EquityFunds(ctx); err != nil || f != 54321.5 {
		t.Fatal(f, err)
	}
	if ps, err := c.Positions(ctx); err != nil || len(ps) != 1 || ps[0].Qty != 65 {
		t.Fatal(ps, err)
	}
	if err := c.CancelOrder(ctx, "8"); err == nil || err.Error() != "kite DELETE /orders/regular/8: HTTP 400 Insufficient funds" {
		t.Fatal(err)
	}
}
