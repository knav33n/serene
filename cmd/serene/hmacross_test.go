package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"serene/internal/kite"
	"serene/internal/manual"
	"serene/internal/market"
	"serene/internal/paper"
)

type fakeOptKite struct {
	ins    []kite.Instrument
	pos    []kite.Position
	placed []kite.OrderRequest
}

func (f *fakeOptKite) Instruments(context.Context, string) ([]kite.Instrument, error) {
	return f.ins, nil
}
func (f *fakeOptKite) Positions(context.Context) ([]kite.Position, error) { return f.pos, nil }
func (f *fakeOptKite) PlaceOrder(_ context.Context, o kite.OrderRequest) (string, error) {
	f.placed = append(f.placed, o)
	return "1", nil
}
func (f *fakeOptKite) OrderStatus(context.Context, string) (kite.Order, error) {
	return kite.Order{Status: "COMPLETE", Average: 101.5}, nil
}

func TestPlaceOpt(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, market.IST)
	exp := market.Date{Year: 2026, Month: time.November, Day: 25}
	old := market.Date{Year: 2026, Month: time.November, Day: 18}
	f := &fakeOptKite{
		ins: []kite.Instrument{
			{Symbol: "NIFTY25N2526000CE", Name: "NIFTY", Type: "CE", Expiry: exp, LotSize: 65},
			{Symbol: "NIFTY25N1826000CE", Name: "NIFTY", Type: "CE", Expiry: old, LotSize: 65},
		},
		pos: []kite.Position{
			{Symbol: "NIFTY25N2526000CE", Exchange: "NFO", Product: "MIS", Qty: 130},
			{Symbol: "NIFTY25N2526000CE", Exchange: "NFO", Product: "NRML", Qty: 650},
		},
	}
	ctx := context.Background()
	buy := manual.FutOrder{Strategy: paper.HMACrossName, Side: market.Buy, Symbol: "NIFTY25N2526000CE"}
	if _, err := placeOpt(ctx, f, buy, now, false); err != nil {
		t.Fatal(err)
	}
	sell := buy
	sell.Side = market.Sell
	if _, err := placeOpt(ctx, f, sell, now, false); err != nil {
		t.Fatal(err)
	}
	if len(f.placed) != 2 {
		t.Fatalf("placed %+v", f.placed)
	}
	if b := f.placed[0]; b.Side != "BUY" || b.Qty != 65 || b.Exchange != "NFO" || b.Product != "MIS" || b.Type != "MARKET" {
		t.Errorf("buy %+v", b)
	}
	if s := f.placed[1]; s.Side != "SELL" || s.Qty != 130 || s.Product != "MIS" {
		t.Errorf("sell %+v: want the 130 MIS held, not the NRML", s)
	}
	// Expired contract, unknown symbol, nothing held, dry run: no orders.
	for _, o := range []manual.FutOrder{
		{Strategy: paper.HMACrossName, Side: market.Buy, Symbol: "NIFTY25N1826000CE"},
		{Strategy: paper.HMACrossName, Side: market.Buy, Symbol: "BANKNIFTY"},
	} {
		if _, err := placeOpt(ctx, f, o, now, false); err == nil {
			t.Errorf("%+v: want refused", o)
		}
	}
	f.pos = nil
	if _, err := placeOpt(ctx, f, sell, now, false); err == nil || !strings.Contains(err.Error(), "no MIS") {
		t.Errorf("sell with nothing held: %v", err)
	}
	if msg, err := placeOpt(ctx, f, buy, now, true); err != nil || !strings.Contains(msg, "dry run") {
		t.Errorf("dry run: %q %v", msg, err)
	}
	if len(f.placed) != 2 {
		t.Errorf("refused or dry-run clicks placed orders: %+v", f.placed[2:])
	}
}

func TestHMACrossBarsDropsForming(t *testing.T) {
	d := market.Date{Year: 2026, Month: time.November, Day: 23}
	inst := market.Option(market.NFO, "NIFTY", d, market.Rupees(26000), market.Call, 65)
	var cs []kite.Candle
	for m := range 23 { // 09:15 .. 09:37, and Kite's forming 09:38
		cs = append(cs, kite.Candle{Time: d.At(9, 15).Add(time.Duration(m) * time.Minute), Open: 100, High: 101, Low: 99, Close: 100})
	}
	now := d.At(9, 38).Add(20 * time.Second)
	bars, _, err := hmaCrossBars(inst, cs, d, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != 2 || !bars[1].End.Equal(d.At(9, 35)) {
		t.Fatalf("want 09:15 and 09:25 bars only, got %d: %+v", len(bars), bars)
	}
}

func TestHMACrossButtonsDedicated(t *testing.T) {
	st := &paperDayState{Lot: 65}
	st.Legs = append(st.Legs,
		struct {
			Right  market.Right `json:"right"`
			Token  int64        `json:"token"`
			Symbol string       `json:"symbol"`
		}{market.Call, 1, "NIFTYCE"},
		struct {
			Right  market.Right `json:"right"`
			Token  int64        `json:"token"`
			Symbol string       `json:"symbol"`
		}{market.Put, 2, "NIFTYPE"})
	d := market.Date{Year: 2026, Month: time.November, Day: 23}
	pe := market.Option(market.NFO, "NIFTY", d, market.Rupees(26000), market.Put, 65)
	up, _ := json.Marshal(hmaCrossButtons(st, paper.Cross{Instrument: pe, Up: true}))
	down, _ := json.Marshal(hmaCrossButtons(st, paper.Cross{Instrument: pe, Up: false}))
	for _, s := range []string{string(up), string(down)} {
		if !strings.Contains(s, "hma_crossover|BUY|NIFTYCE") || !strings.Contains(s, "hma_crossover|BUY|NIFTYPE") {
			t.Errorf("want Buy CE and Buy PE on every cross, got %s", s)
		}
	}
	if strings.Count(string(up), `"type":"button"`) != 2 || strings.Contains(string(up), "SELL") {
		t.Errorf("up-cross: want just the two buys, got %s", up)
	}
	if strings.Count(string(down), `"type":"button"`) != 3 || !strings.Contains(string(down), "hma_crossover|SELL|NIFTYPE") ||
		strings.Contains(string(down), "SELL|NIFTYCE") {
		t.Errorf("down-cross on the PE: want the buys and Sell PE, got %s", down)
	}
}

func TestPlaceOptSensex(t *testing.T) {
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, market.IST)
	exp := market.Date{Year: 2026, Month: time.November, Day: 27}
	var asked string
	f := &fakeOptKite{
		ins: []kite.Instrument{{Symbol: "SENSEX26N2785000CE", Name: "SENSEX", Type: "CE", Expiry: exp, LotSize: 20}},
		pos: []kite.Position{{Symbol: "SENSEX26N2785000CE", Exchange: "BFO", Product: "MIS", Qty: 40}},
	}
	ctx := context.Background()
	buy := manual.FutOrder{Strategy: paper.HMACrossName, Side: market.Buy, Symbol: "SENSEX26N2785000CE"}
	if _, err := placeOpt(ctx, &askKite{f, &asked}, buy, now, false); err != nil {
		t.Fatal(err)
	}
	if asked != "BFO" {
		t.Errorf("instruments of %q, want BFO", asked)
	}
	sell := buy
	sell.Side = market.Sell
	if _, err := placeOpt(ctx, f, sell, now, false); err != nil {
		t.Fatal(err)
	}
	if b := f.placed[0]; b.Exchange != "BFO" || b.Qty != 20 || b.Side != "BUY" {
		t.Errorf("buy %+v", b)
	}
	if s := f.placed[1]; s.Exchange != "BFO" || s.Qty != 40 || s.Side != "SELL" {
		t.Errorf("sell %+v", s)
	}
}

type askKite struct {
	*fakeOptKite
	asked *string
}

func (a *askKite) Instruments(ctx context.Context, exch string) ([]kite.Instrument, error) {
	*a.asked = exch
	return a.fakeOptKite.Instruments(ctx, exch)
}

func TestHMACrossTextSensex(t *testing.T) {
	d := market.Date{Year: 2026, Month: time.November, Day: 26}
	ce := market.Option(market.BFO, "SENSEX", d, market.Rupees(85000), market.Call, 20)
	c := paper.Cross{Instrument: ce, Time: d.At(9, 25), End: d.At(9, 35), Up: true}
	if s := hmaCrossText(c, paper.DefaultHMACross(d)); !strings.Contains(s, "hma_crossover* SENSEX 85000") {
		t.Errorf("text %q", s)
	}
}
