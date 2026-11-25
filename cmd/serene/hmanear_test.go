package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"serene/internal/fyers"
	"serene/internal/market"
	"serene/internal/paper"
)

type fakeNearFyers struct {
	placed []fyers.OrderRequest
	status map[string]int
	held   int64
}

func (f *fakeNearFyers) PlaceOrder(_ context.Context, o fyers.OrderRequest) (string, error) {
	f.placed = append(f.placed, o)
	if o.Side == "BUY" {
		f.held += o.Qty
	}
	return o.Side, nil
}
func (f *fakeNearFyers) Order(_ context.Context, id string) (fyers.Order, error) {
	px := 100.0
	if id == "SELL" {
		px = 102.05
	}
	return fyers.Order{ID: id, Status: f.status[id], TradedPrice: px}, nil
}
func (f *fakeNearFyers) Positions(context.Context) ([]fyers.Position, error) {
	return []fyers.Position{{Symbol: "NSE:CE", NetQty: f.held, ProductType: "INTRADAY"}}, nil
}

func TestNearTargetPrice(t *testing.T) {
	for in, want := range map[float64]float64{100: 102, 269.8: 275.2, 120.95: 123.4, 101.07: 103.1} {
		if got := nearTargetPrice(in); got != want {
			t.Errorf("target of %.2f: %.2f, want %.2f", in, got, want)
		}
	}
}

func TestHMANearTrade(t *testing.T) {
	d := market.Date{Year: 2026, Month: time.November, Day: 26}
	st := &paperDayState{Day: d, Lot: 65}
	st.Legs = append(st.Legs, struct {
		Right  market.Right `json:"right"`
		Token  int64        `json:"token"`
		Symbol string       `json:"symbol"`
	}{market.Call, 1, "NIFTYCE"})
	ce := market.Option(market.NFO, "NIFTY", d, market.Rupees(25700), market.Call, 65)
	near := paper.Near{Cross: paper.Cross{Instrument: ce, Time: d.At(9, 25), End: d.At(9, 35), Up: true}, Time: d.At(10, 3)}
	clock := d.At(10, 4).Add(20 * time.Second)
	price := 101.0
	var msgs []string
	f := &fakeNearFyers{status: map[string]int{"BUY": 2, "SELL": 6}}
	dep := nearDeps{
		broker: f,
		ticker: func(context.Context, market.Right) (string, int64, error) { return "NSE:CE", 65, nil },
		price:  func(context.Context, string) (float64, error) { return price, nil },
		notify: func(_ context.Context, _ *nearTrade, m string) { msgs = append(msgs, m) },
		sleep:  func(context.Context, time.Duration) error { clock = clock.Add(3 * time.Second); return nil },
		now:    func() time.Time { return clock },
	}
	dir := t.TempDir()
	run := func() {
		t.Helper()
		if err := hmaNearTrade(context.Background(), dep, st, []paper.Near{near}, dir); err != nil {
			t.Fatal(err)
		}
	}
	// Buys one lot at market, then watches: 101 < the 102 target, no sell.
	run()
	if len(f.placed) != 1 || f.placed[0].Side != "BUY" || f.placed[0].Type != "MARKET" || f.placed[0].Qty != 65 || f.placed[0].Product != "INTRADAY" {
		t.Fatalf("placed %+v", f.placed)
	}
	// The target is reached: one LIMIT sell at 102, which rests.
	price, clock = 102.3, d.At(10, 5)
	run()
	if len(f.placed) != 2 || f.placed[1].Side != "SELL" || f.placed[1].Type != "LIMIT" || f.placed[1].Price != 102 {
		t.Fatalf("placed %+v", f.placed)
	}
	// It fills; later runs and later near alerts trade no more today.
	f.status["SELL"] = 2
	run()
	near.Time, clock = d.At(11, 0), d.At(11, 1)
	run()
	if len(f.placed) != 2 {
		t.Errorf("traded twice in a day: %+v", f.placed)
	}
	if len(msgs) != 2 || !strings.Contains(msgs[1], "102.05") {
		t.Errorf("messages %q", msgs)
	}
	// A stale alert (after downtime) isn't bought.
	f2 := &fakeNearFyers{status: map[string]int{"BUY": 2}}
	dep.broker, clock = f2, d.At(11, 30)
	if err := hmaNearTrade(context.Background(), dep, st, []paper.Near{near}, t.TempDir()); err != nil || len(f2.placed) != 0 {
		t.Errorf("stale alert bought: %+v %v", f2.placed, err)
	}
}
