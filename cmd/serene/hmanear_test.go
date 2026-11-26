package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"serene/internal/market"
	"serene/internal/paper"
)

// fakeNear is a broker whose buys fill at 100 and limit sells at 102.05
// once sellFills is set.
type fakeNear struct {
	name      string
	cap       float64
	lot       int64
	placed    []string // side type qty price
	held      int64
	sellFills bool
	orders    map[string][2]any // id → side, qty
}

func (f *fakeNear) Name() string                         { return f.name }
func (f *fakeNear) Cap(context.Context) (float64, error) { return f.cap, nil }
func (f *fakeNear) Ticker(_ context.Context, x hmaIndex, _ *paperDayState, r market.Right) (string, int64, error) {
	return f.name + ":" + x.Name + string(r), f.lot, nil
}
func (f *fakeNear) Place(_ context.Context, _, side, typ string, qty int64, price float64) (string, error) {
	id := fmt.Sprint(len(f.placed))
	f.placed = append(f.placed, fmt.Sprintf("%s %s %d %.2f", side, typ, qty, price))
	if f.orders == nil {
		f.orders = map[string][2]any{}
	}
	f.orders[id] = [2]any{side, qty}
	if side == "BUY" {
		f.held += qty
	}
	return id, nil
}
func (f *fakeNear) Order(_ context.Context, _, id string) (nearOrder, error) {
	o := f.orders[id]
	qty := o[1].(int64)
	if o[0] == "BUY" {
		return nearOrder{Done: true, Filled: qty, Avg: 100, Status: "traded"}, nil
	}
	if !f.sellFills {
		return nearOrder{Status: "pending"}, nil
	}
	return nearOrder{Done: true, Filled: qty, Avg: 102.05, Status: "traded"}, nil
}
func (f *fakeNear) Held(context.Context, string) (int64, error) { return f.held, nil }

func TestNearTargetPrice(t *testing.T) {
	for in, want := range map[float64]float64{100: 102, 269.8: 275.2, 120.95: 123.4, 101.07: 103.1} {
		if got := nearTargetPrice(in); got != want {
			t.Errorf("target of %.2f: %.2f, want %.2f", in, got, want)
		}
	}
}

func TestNearSizing(t *testing.T) {
	// Fyers: all of ₹5L at ₹100 × 65 → 75 lots (98% of 5L / 6500).
	if got := nearLots(500000, 100, 65); got != 75 {
		t.Errorf("lots %d", got)
	}
	// Zerodha's ₹1L cap at ₹100 × 65 → 15 lots; too little cash → 0.
	if got := nearLots(min(nearZerodhaCap, 250000), 100, 65); got != 15 {
		t.Errorf("lots %d", got)
	}
	if got := nearLots(5000, 100, 65); got != 0 {
		t.Errorf("lots %d", got)
	}
	// 75 lots of 65 = 4875: NIFTY's 1755 freeze is 27 lots.
	if got := fmt.Sprint(nearChunks(4875, 65, 1755)); got != "[1755 1755 1365]" {
		t.Errorf("chunks %s", got)
	}
	// SENSEX lot 20, freeze 1000.
	if got := fmt.Sprint(nearChunks(2040, 20, 1000)); got != "[1000 1000 40]" {
		t.Errorf("chunks %s", got)
	}
}

func TestHMANearTrade(t *testing.T) {
	d := market.Date{Year: 2026, Month: time.November, Day: 26}
	st := &paperDayState{Day: d, Index: "NIFTY", Lot: 65}
	st.Legs = append(st.Legs, struct {
		Right  market.Right `json:"right"`
		Token  int64        `json:"token"`
		Symbol string       `json:"symbol"`
	}{market.Call, 1, "NIFTYCE"}, struct {
		Right  market.Right `json:"right"`
		Token  int64        `json:"token"`
		Symbol string       `json:"symbol"`
	}{market.Put, 2, "NIFTYPE"})
	ce := market.Option(market.NFO, "NIFTY", d, market.Rupees(25700), market.Call, 65)
	pe := market.Option(market.NFO, "NIFTY", d, market.Rupees(25700), market.Put, 65)
	near := func(in market.Instrument, cross, at time.Time) paper.Near {
		return paper.Near{Cross: paper.Cross{Instrument: in, Time: cross, End: cross.Add(10 * time.Minute), Up: true}, Time: at}
	}
	nifty := hmaIndices[0]
	clock := d.At(10, 4).Add(20 * time.Second)
	price := 100.0
	var msgs []string
	fy := &fakeNear{name: "fyers", cap: 500000, lot: 65}
	zd := &fakeNear{name: "zerodha", cap: nearZerodhaCap, lot: 65}
	dep := nearDeps{
		brokers: []nearBroker{fy, zd},
		prices: func(_ context.Context, keys []string) (map[string]float64, error) {
			out := map[string]float64{}
			for _, k := range keys {
				out[k] = price
			}
			return out, nil
		},
		notify: func(_ context.Context, _ *nearTrade, m string) { msgs = append(msgs, m) },
		sleep:  func(context.Context, time.Duration) error { clock = clock.Add(3 * time.Second); return nil },
		now:    func() time.Time { return clock },
	}
	dir := t.TempDir()
	nears := []paper.Near{near(ce, d.At(9, 25), d.At(10, 3))}
	run := func() {
		t.Helper()
		if err := hmaNearTrade(context.Background(), dep, []nearRun{{idx: nifty, st: st, nears: nears}}, dir); err != nil {
			t.Fatal(err)
		}
	}
	// Both brokers buy at market: Fyers 75 lots in 3 orders, Zerodha 15 lots.
	run()
	if got := strings.Join(fy.placed, ","); got != "BUY MARKET 1755 0.00,BUY MARKET 1755 0.00,BUY MARKET 1365 0.00" {
		t.Fatalf("fyers placed %s", got)
	}
	if got := strings.Join(zd.placed, ","); got != "BUY MARKET 975 0.00" {
		t.Fatalf("zerodha placed %s", got)
	}
	// The same alert next minute isn't bought again.
	clock = d.At(10, 5)
	run()
	if len(fy.placed) != 3 || len(zd.placed) != 1 {
		t.Fatalf("bought twice: %v %v", fy.placed, zd.placed)
	}
	// The target is reached: LIMIT sells at 102, split at the freeze; they rest.
	price, clock = 102.3, d.At(10, 6)
	run()
	if got := strings.Join(zd.placed[1:], ","); got != "SELL LIMIT 975 102.00" {
		t.Fatalf("zerodha sells %s", got)
	}
	if len(fy.placed) != 6 {
		t.Fatalf("fyers sells %v", fy.placed[3:])
	}
	// They fill; a second alert (the PE) opens trade 2, a third is ignored.
	fy.sellFills, zd.sellFills = true, true
	fy.held, zd.held = 0, 0
	price = 100
	nears = append(nears, near(pe, d.At(10, 15), d.At(10, 30)), near(pe, d.At(11, 5), d.At(11, 20)))
	clock = d.At(10, 31)
	run()
	if len(zd.placed) != 3 || !strings.HasPrefix(zd.placed[2], "BUY") {
		t.Fatalf("second trade: %v", zd.placed)
	}
	clock = d.At(11, 21)
	run()
	if len(zd.placed) != 3 {
		t.Fatalf("a third trade today: %v", zd.placed)
	}
	var hit int
	for _, m := range msgs {
		if strings.Contains(m, "target hit") {
			hit++
		}
	}
	if hit != 2 {
		t.Errorf("want a target-hit message per broker, got %q", msgs)
	}
	// SENSEX has its own two a day; a stale alert (after downtime) isn't bought.
	sx := &fakeNear{name: "zerodha", cap: nearZerodhaCap, lot: 20}
	dep.brokers, clock = []nearBroker{sx}, d.At(11, 30)
	sst := &paperDayState{Day: d, Index: "SENSEX", Lot: 20, Legs: st.Legs}
	sce := market.Option(market.BFO, "SENSEX", d, market.Rupees(85000), market.Call, 20)
	stale := []paper.Near{near(sce, d.At(10, 0), d.At(11, 20))}
	if err := hmaNearTrade(context.Background(), dep, []nearRun{{idx: hmaIndices[1], st: sst, nears: stale}}, dir); err != nil || len(sx.placed) != 0 {
		t.Errorf("stale alert bought: %v %v", sx.placed, err)
	}
	fresh := []paper.Near{near(sce, d.At(11, 10), d.At(11, 28))}
	if err := hmaNearTrade(context.Background(), dep, []nearRun{{idx: hmaIndices[1], st: sst, nears: fresh}}, dir); err != nil || len(sx.placed) != 1 {
		t.Errorf("SENSEX trade: %v %v", sx.placed, err)
	}
}

func TestHMANearTradeNoFunds(t *testing.T) {
	d := market.Date{Year: 2026, Month: time.November, Day: 26}
	st := &paperDayState{Day: d, Index: "NIFTY", Lot: 65}
	ce := market.Option(market.NFO, "NIFTY", d, market.Rupees(25700), market.Call, 65)
	n := paper.Near{Cross: paper.Cross{Instrument: ce, Time: d.At(9, 25), End: d.At(9, 35), Up: true}, Time: d.At(10, 3)}
	clock := d.At(10, 4)
	f := &fakeNear{name: "fyers", cap: 3000, lot: 65}
	var msgs []string
	dep := nearDeps{brokers: []nearBroker{f},
		prices: func(_ context.Context, keys []string) (map[string]float64, error) {
			return map[string]float64{keys[0]: 100}, nil
		},
		notify: func(_ context.Context, _ *nearTrade, m string) { msgs = append(msgs, m) },
		sleep:  func(context.Context, time.Duration) error { clock = clock.Add(3 * time.Second); return nil },
		now:    func() time.Time { return clock }}
	if err := hmaNearTrade(context.Background(), dep, []nearRun{{idx: hmaIndices[0], st: st, nears: []paper.Near{n}}}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(f.placed) != 0 || len(msgs) != 1 || !strings.Contains(msgs[0], "buys no lot") || !strings.Contains(msgs[0], "1 of 2") {
		t.Errorf("placed %v, messages %q", f.placed, msgs)
	}
}
