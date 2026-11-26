package live

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"serene/internal/kite"
	"serene/internal/market"
)

type stbtFake struct {
	orders  map[string]*kite.Order
	n       int
	log     []string
	held    map[string]int64
	lostAck bool // the place reaches Kite but the call fails
}

func newSTBTFake() *stbtFake {
	return &stbtFake{orders: map[string]*kite.Order{}, held: map[string]int64{}}
}

func (f *stbtFake) Place(_ context.Context, o kite.OrderRequest) (string, error) {
	f.n++
	id := fmt.Sprint(f.n)
	st := "OPEN"
	if o.Type == "SL" {
		st = "TRIGGER PENDING"
	}
	f.orders[id] = &kite.Order{ID: id, Status: st, Symbol: o.Symbol, Side: o.Side, Type: o.Type, Qty: o.Qty, Price: o.Price, Trigger: o.Trigger, Tag: o.Tag, Product: o.Product}
	f.log = append(f.log, fmt.Sprintf("place %s %s %s %s @%.2f/%.2f", id, o.Symbol, o.Side, o.Type, o.Price, o.Trigger))
	if f.lostAck {
		f.lostAck = false
		return "", errors.New("timeout")
	}
	return id, nil
}

func (f *stbtFake) Modify(_ context.Context, id string, o kite.OrderRequest) error {
	x := f.orders[id]
	x.Type, x.Price, x.Trigger = o.Type, o.Price, o.Trigger
	f.log = append(f.log, fmt.Sprintf("modify %s %s @%.2f/%.2f", id, o.Type, o.Price, o.Trigger))
	return nil
}

func (f *stbtFake) Cancel(_ context.Context, id string) error {
	x := f.orders[id]
	if x.Status == "COMPLETE" {
		return errors.New("already complete")
	}
	x.Status = "CANCELLED"
	f.log = append(f.log, "cancel "+id)
	return nil
}

func (f *stbtFake) Order(_ context.Context, id string) (kite.Order, error) {
	x, ok := f.orders[id]
	if !ok {
		return kite.Order{}, errors.New("no such order")
	}
	return *x, nil
}

func (f *stbtFake) Orders(context.Context) ([]kite.Order, error) {
	var out []kite.Order
	for _, o := range f.orders {
		out = append(out, *o)
	}
	return out, nil
}

func (f *stbtFake) Position(_ context.Context, sym string) (int64, error) { return f.held[sym], nil }

// fill completes the open order on sym at px.
func (f *stbtFake) fill(t *testing.T, sym string, px float64) {
	t.Helper()
	for _, o := range f.orders {
		if o.Symbol == sym && o.Status != "COMPLETE" && o.Status != "CANCELLED" {
			o.Status, o.Average, o.Filled = "COMPLETE", px, o.Qty
			if o.Side == "SELL" {
				f.held[sym] -= o.Qty
			} else {
				f.held[sym] += o.Qty
			}
			return
		}
	}
	t.Fatalf("no working order on %s", sym)
}

func (f *stbtFake) working(sym string) *kite.Order {
	for _, o := range f.orders {
		if o.Symbol == sym && o.Status != "COMPLETE" && o.Status != "CANCELLED" {
			return o
		}
	}
	return nil
}

type stbtMkt struct {
	ltp map[string]float64
	low float64
}

func (m *stbtMkt) LTP(context.Context, []string) (map[string]float64, error) { return m.ltp, nil }
func (m *stbtMkt) LowSince(context.Context, int64, time.Time) (float64, bool, error) {
	return m.low, m.low > 0, nil
}

var (
	mon = market.Date{Year: 2026, Month: time.November, Day: 30}
	tue = market.Date{Year: 2026, Month: time.December, Day: 1}
)

func at(d market.Date, h, m int) time.Time { return d.At(h, m).Add(20 * time.Second) }

func niftyPick() *STBTPick {
	return &STBTPick{Expiry: tue, Spot: 26010, Lot: 65, Legs: []STBTLeg{
		{Right: "CE", Symbol: "NIFTY26DEC26000CE", Strike: 26000, Ref: 100},
		{Right: "PE", Symbol: "NIFTY26DEC26000PE", Strike: 26000, Ref: 80},
	}}
}

func stbtStep(t *testing.T, f *stbtFake, m *stbtMkt, st *STBTState, now time.Time, pick *STBTPick) []STBTEvent {
	t.Helper()
	evs, err := StepSTBT(context.Background(), f, m, st, NiftySTBT, DefaultSTBTConfig, now, pick)
	if err != nil {
		t.Fatalf("%s: %v (log %v)", now.Format("01-02 15:04"), err, f.log)
	}
	return evs
}

func TestSTBTEntryDay(t *testing.T) {
	if !EntryDay(mon, tue) || EntryDay(tue, tue) {
		t.Fatal("1DTE")
	}
	st := &STBTState{}
	if NiftySTBT.WantsPick(st, at(mon, 12, 59), tue) || !NiftySTBT.WantsPick(st, at(mon, 13, 0), tue) || NiftySTBT.WantsPick(st, at(mon, 13, 30), tue) {
		t.Fatal("entry window")
	}
}

func TestSTBTLifecycle(t *testing.T) {
	f, m := newSTBTFake(), &stbtMkt{ltp: map[string]float64{"NIFTY26DEC26000CE": 100, "NIFTY26DEC26000PE": 80}}
	st := &STBTState{}
	ce, pe := "NIFTY26DEC26000CE", "NIFTY26DEC26000PE"

	// arm: SL SELL at −5%
	evs := stbtStep(t, f, m, st, at(mon, 13, 0), niftyPick())
	if !st.Active || len(evs) != 1 || evs[0].Kind != "armed" || !st.DoneFor(mon) {
		t.Fatalf("arm %+v", evs)
	}
	if o := f.working(ce); o == nil || o.Type != "SL" || o.Side != "SELL" || o.Trigger != 95 || o.Product != "NRML" || o.Qty != 65 {
		t.Fatalf("ce trigger %+v", o)
	}
	if o := f.working(pe); o.Trigger != 76 {
		t.Fatalf("pe trigger %+v", o)
	}

	// CE sells at 95: SL BUY at 133
	f.fill(t, ce, 95)
	evs = stbtStep(t, f, m, st, at(mon, 13, 10), nil)
	if evs[0].Kind != "sold" || st.Legs[0].Stop != 133 {
		t.Fatalf("sold %+v stop %v", evs, st.Legs[0].Stop)
	}
	if o := f.working(ce); o.Side != "BUY" || o.Type != "SL" || o.Trigger != 133 {
		t.Fatalf("stop %+v", o)
	}

	// trail: low 70 is 26% under 95 → one step: 95 × 1.30 = 123.50
	m.low = 70
	evs = stbtStep(t, f, m, st, at(mon, 14, 0), nil)
	if len(evs) != 1 || evs[0].Kind != "trail" || st.Legs[0].Stop != 123.5 || f.working(ce).Trigger != 123.5 {
		t.Fatalf("trail %+v %v", evs, st.Legs[0].Stop)
	}

	// CE stopped at 124: re-entry armed at 95 (one re-entry)
	f.fill(t, ce, 124)
	m.ltp[ce] = 124
	evs = stbtStep(t, f, m, st, at(mon, 14, 30), nil)
	if evs[0].Kind != "stop" || st.Legs[0].State != LegRecost || st.Legs[0].Trig != 95 {
		t.Fatalf("stop %+v %+v", evs, st.Legs[0])
	}
	if o := f.working(ce); o.Side != "SELL" || o.Trigger != 95 {
		t.Fatalf("re-entry %+v", o)
	}

	// next morning: DAY orders lapsed; positions agree, so everything is re-armed.
	// CE gaps under its re-entry level: sold at once with a LIMIT
	for _, o := range f.orders {
		if o.Status == "TRIGGER PENDING" {
			o.Status = "CANCELLED"
		}
	}
	m.ltp[ce], m.ltp[pe] = 90, 85
	stbtStep(t, f, m, st, at(tue, 9, 15), nil)
	if o := f.working(ce); o == nil || o.Type != "LIMIT" || o.Side != "SELL" || o.Price != 87.3 {
		t.Fatalf("gap re-entry %+v (log %v)", o, f.log)
	}
	if o := f.working(pe); o.Type != "SL" || o.Trigger != 76 {
		t.Fatalf("pe re-armed %+v", o)
	}
	f.fill(t, ce, 89.5)
	m.low = 0
	evs = stbtStep(t, f, m, st, at(tue, 9, 16), nil)
	if st.Legs[0].ReUsed != 1 || st.Legs[0].State != LegOpen || !strings.Contains(evs[0].Text, "re-entry 1 of 1") {
		t.Fatalf("re-entry fill %+v %+v", evs, st.Legs[0])
	}

	// exit at 15:15: PE trigger cancelled, CE bought back with a LIMIT
	m.ltp[ce] = 60
	stbtStep(t, f, m, st, at(tue, 15, 15), nil)
	if st.Legs[1].State != LegDone {
		t.Fatalf("pe %+v", st.Legs[1])
	}
	if o := f.working(ce); o.Side != "BUY" || o.Type != "LIMIT" || o.Price != 61.8 {
		t.Fatalf("exit %+v", o)
	}
	if !st.Busy() {
		t.Fatal("exit working, not busy")
	}
	f.fill(t, ce, 60.5)
	evs = stbtStep(t, f, m, st, at(tue, 15, 15), nil)
	last := evs[len(evs)-1]
	if st.Active || last.Kind != "closed" || len(st.Legs[0].Trades) != 2 || st.Legs[0].Trades[1].Reason != "exit" {
		t.Fatalf("closed %+v %+v", evs, st.Legs[0].Trades)
	}
	if f.held[ce] != 0 || f.held[pe] != 0 {
		t.Fatalf("held %v", f.held)
	}
}

func TestSTBTOvernightMismatchHalts(t *testing.T) {
	f, m := newSTBTFake(), &stbtMkt{ltp: map[string]float64{"NIFTY26DEC26000CE": 100, "NIFTY26DEC26000PE": 80}}
	st := &STBTState{}
	stbtStep(t, f, m, st, at(mon, 13, 0), niftyPick())
	f.fill(t, "NIFTY26DEC26000CE", 95)
	stbtStep(t, f, m, st, at(mon, 13, 1), nil)
	// the stop filled after the last check yesterday: Zerodha is flat, serene thinks short
	f.fill(t, "NIFTY26DEC26000CE", 133)
	_, err := StepSTBT(context.Background(), f, m, st, NiftySTBT, DefaultSTBTConfig, at(tue, 9, 15), nil)
	if err == nil || st.Halted == "" {
		t.Fatalf("no halt: %v", err)
	}
	if evs, err := StepSTBT(context.Background(), f, m, st, NiftySTBT, DefaultSTBTConfig, at(tue, 9, 16), nil); err != nil || evs != nil {
		t.Fatal("halted state still acts")
	}
}

func TestSTBTLostPlaceAndChase(t *testing.T) {
	f, m := newSTBTFake(), &stbtMkt{ltp: map[string]float64{"NIFTY26DEC26000CE": 100, "NIFTY26DEC26000PE": 80}}
	st := &STBTState{}
	f.lostAck = true
	_, err := StepSTBT(context.Background(), f, m, st, NiftySTBT, DefaultSTBTConfig, at(mon, 13, 0), niftyPick())
	if err == nil {
		t.Fatal("lost ack not reported")
	}
	n := len(f.orders)
	stbtStep(t, f, m, st, at(mon, 13, 1), nil) // finds the CE order by tag, places the PE
	if st.Legs[0].Order != "1" || len(f.orders) != n+1 {
		t.Fatalf("reconcile %+v %v", st.Legs[0], f.log)
	}
	// CE trigger hits but the SL limit doesn't fill: chased with LIMITs
	f.orders["1"].Status = "OPEN"
	m.ltp["NIFTY26DEC26000CE"] = 88
	stbtStep(t, f, m, st, at(mon, 13, 2), nil)
	if o := f.orders["1"]; o.Type != "LIMIT" || o.Price != 85.35 || !st.Busy() {
		t.Fatalf("chase %+v", o)
	}
}

func TestSTBTSkip(t *testing.T) {
	f, m := newSTBTFake(), &stbtMkt{}
	st := &STBTState{}
	p := niftyPick()
	p.Skip = "margin ₹3.1L > funds ₹2L"
	evs := stbtStep(t, f, m, st, at(mon, 13, 0), p)
	if st.Active || evs[0].Kind != "skip" || !st.DoneFor(mon) || len(f.orders) != 0 {
		t.Fatalf("skip %+v", evs)
	}
}
