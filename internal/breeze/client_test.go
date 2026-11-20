package breeze

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"serene/internal/market"
)

func fake(t *testing.T, h http.HandlerFunc) *Client {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{AppKey: "key", Secret: "sec", Session: "U0VTUw==", BaseURL: srv.URL, HTTP: srv.Client(),
		Now: func() time.Time { return time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC) }}
}

func TestSignedPlaceOrder(t *testing.T) {
	var got map[string]string
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ts := r.Header.Get("X-Timestamp")
		sum := sha256.Sum256([]byte(ts + string(b) + "sec"))
		if ts != "2026-11-20T04:00:00.000Z" || r.Header.Get("X-Checksum") != "token "+hex.EncodeToString(sum[:]) ||
			r.Header.Get("X-AppKey") != "key" || r.Header.Get("X-SessionToken") != "U0VTUw==" {
			t.Errorf("headers %v", r.Header)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/order" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.Unmarshal(b, &got)
		w.Write([]byte(`{"Success":{"order_id":"2026112000001","message":"Successfully Placed the order"},"Status":200,"Error":null}`))
	})
	id, err := c.PlaceOrder(context.Background(), OrderRequest{Contract: Contract{Stock: "NIFTY",
		Expiry: market.Date{Year: 2026, Month: 11, Day: 24}, Strike: 25400, Right: "CE"}, Side: "BUY", Qty: 75, Price: 100.05, Remark: "serenemanual"})
	if err != nil || id != "2026112000001" {
		t.Fatal(id, err)
	}
	want := map[string]string{"stock_code": "NIFTY", "exchange_code": "NFO", "product": "options", "action": "buy", "order_type": "limit",
		"quantity": "75", "price": "100.05", "expiry_date": "2026-11-24T06:00:00.000Z", "right": "call", "strike_price": "25400",
		"validity": "day", "user_remark": "serenemanual"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestOrderFilledAndDone(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Success":[{"order_id":"7","stock_code":"NIFTY","action":"Buy","status":"Partially Executed And Cancelled",
			"quantity":"150","pending_quantity":"0","cancelled_quantity":"75","price":"100","average_price":"99.95",
			"expiry_date":"24-Nov-2026","strike_price":"25400","right":"Call","user_remark":"serenemanual"}],"Status":200,"Error":null}`))
	})
	o, err := c.Order(context.Background(), "7")
	if err != nil || !o.Done() || o.Filled() != 75 || o.Average != 99.95 || o.Right != "CE" || o.Side != "BUY" {
		t.Fatal(o, err)
	}
	if (Order{Status: "Ordered", Qty: 75, Pending: 75}).Done() || (Order{Status: "Rejected", Qty: 75}).Filled() != 0 {
		t.Fatal("open or rejected")
	}
}

func TestNoDataIsEmpty(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Success":null,"Status":500,"Error":"No Data Found"}`))
	})
	ps, err := c.Positions(context.Background())
	if err != nil || len(ps) != 0 {
		t.Fatal(ps, err)
	}
}

func TestPositionsAndFunds(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/funds":
			w.Write([]byte(`{"Success":{"allocated_fno":"500000.00","block_by_trade_fno":120000},"Status":200,"Error":null}`))
		default:
			w.Write([]byte(`{"Success":[{"segment":"fno","product_type":"Options","exchange_code":"NFO","stock_code":"NIFTY",
				"expiry_date":"24-Nov-2026","strike_price":"25400","right":"Put","action":"Buy","quantity":"75","average_price":"80",
				"ltp":"82.5","pnl":null},{"segment":"equity","stock_code":"INFY"}],"Status":200,"Error":null}`))
		}
	})
	f, err := c.Funds(context.Background())
	if err != nil || f != 380000 {
		t.Fatal(f, err)
	}
	ps, err := c.Positions(context.Background())
	if err != nil || len(ps) != 1 || ps[0].Right != "PE" || ps[0].Qty != 75 || ps[0].PnL != 187.5 || ps[0].Product != "options" {
		t.Fatal(ps, err)
	}
}
