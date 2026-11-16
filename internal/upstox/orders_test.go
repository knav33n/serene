package upstox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOrderRequests(t *testing.T) {
	var body map[string]any
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = nil
		json.Unmarshal(b, &body)
		paths = append(paths, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v3/order/place":
			w.Write([]byte(`{"status":"success","data":{"order_ids":["2611170001"]},"metadata":{"latency":30}}`))
		case "/v3/order/modify", "/v3/order/cancel":
			w.Write([]byte(`{"status":"success","data":{"order_id":"2611170001"}}`))
		case "/v2/order/details":
			w.Write([]byte(`{"status":"success","data":{"order_id":"2611170001","status":"trigger pending","filled_quantity":0,"average_price":0,"tag":"hmr1117u02"}}`))
		case "/v2/user/get-funds-and-margin":
			w.Write([]byte(`{"status":"success","data":{"equity":{"available_margin":76543.21,"used_margin":0}}}`))
		case "/v2/portfolio/short-term-positions":
			w.Write([]byte(`{"status":"success","data":[{"instrument_token":"NSE_FO|43919","product":"I","quantity":130}]}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"status":"error","errors":[{"errorCode":"UDAPI1154","message":"static IP not whitelisted"}]}`))
		}
	}))
	defer srv.Close()
	c := New("tok")
	c.BaseURL = srv.URL
	ctx := context.Background()
	id, err := c.PlaceOrder(ctx, OrderRequest{Instrument: "NSE_FO|43919", Side: "BUY", Type: "LIMIT", Product: "I",
		Qty: 130, Price: 103.3, Tag: "hmr1117u01"})
	if err != nil || id != "2611170001" {
		t.Fatal(id, err)
	}
	want := map[string]any{"quantity": 130.0, "product": "I", "validity": "DAY", "price": 103.3, "tag": "hmr1117u01",
		"instrument_token": "NSE_FO|43919", "order_type": "LIMIT", "transaction_type": "BUY", "disclosed_quantity": 0.0,
		"trigger_price": 0.0, "is_amo": false}
	if b, w := mustJSON(body), mustJSON(want); b != w {
		t.Fatalf("place body\n%s\nwant\n%s", b, w)
	}
	if paths[0] != "POST /v3/order/place Bearer tok" {
		t.Fatal(paths[0])
	}
	if err := c.ModifyOrder(ctx, "2611170001", OrderRequest{Type: "SL", Qty: 130, Price: 92, Trigger: 100}); err != nil {
		t.Fatal(err)
	}
	want = map[string]any{"order_id": "2611170001", "quantity": 130.0, "validity": "DAY", "price": 92.0, "order_type": "SL", "trigger_price": 100.0}
	if b, w := mustJSON(body), mustJSON(want); b != w {
		t.Fatalf("modify body\n%s\nwant\n%s", b, w)
	}
	o, err := c.OrderStatus(ctx, "2611170001")
	if err != nil || o.Status != "trigger pending" || o.Tag != "hmr1117u02" {
		t.Fatalf("%+v %v", o, err)
	}
	if f, err := c.EquityFunds(ctx); err != nil || f != 76543.21 {
		t.Fatal(f, err)
	}
	if ps, err := c.Positions(ctx); err != nil || len(ps) != 1 || ps[0].Qty != 130 {
		t.Fatal(ps, err)
	}
	if err := c.CancelOrder(ctx, "2611170001"); err != nil {
		t.Fatal(err)
	}
	if paths[len(paths)-1] != "DELETE /v3/order/cancel?order_id=2611170001 Bearer tok" {
		t.Fatal(paths[len(paths)-1])
	}
	if _, _, err := c.StaticIPs(ctx); err == nil || err.Error() != "upstox GET: HTTP 400 UDAPI1154 static IP not whitelisted" {
		t.Fatal(err)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
