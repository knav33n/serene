package live

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"serene/internal/indicator"
	"serene/internal/market"
	"serene/internal/paper"
)

var (
	prevDay = market.Date{Year: 2026, Month: time.November, Day: 16}
	day     = market.Date{Year: 2026, Month: time.November, Day: 17}
	ce      = market.Option(market.NFO, "NIFTY", day, market.Rupees(25900), market.Call, 65)
	pe      = market.Option(market.NFO, "NIFTY", day, market.Rupees(25900), market.Put, 65)
)

type bar struct{ o, l, c float64 }

func mk(d market.Date, hh, mm int, bs []bar) []market.Bar {
	t0 := d.At(hh, mm)
	out := make([]market.Bar, len(bs))
	for i, b := range bs {
		t := t0.Add(time.Duration(i) * time.Minute)
		out[i] = market.Bar{Timeframe: market.Minute1, Time: t, End: t.Add(time.Minute),
			Open: market.Rupees(b.o), Low: market.Rupees(b.l), Close: market.Rupees(b.c),
			High: market.Rupees(max(b.o, b.c) + 1)}
	}
	return out
}

func flat(p float64, n int) []bar {
	out := make([]bar, n)
	for i := range out {
		out[i] = bar{p, p - 1, p}
	}
	return out
}

// rally: a swing low of 95 at 09:17, then a cross up; armed after its last bar.
func rally() []bar {
	return []bar{
		{100, 99, 99}, {99, 97, 97}, {97, 95, 96}, {96, 96, 97}, {97, 96.5, 98},
		{98, 97.5, 104}, {104, 103, 108}, {108, 107, 112}, {112, 111, 114}, {114, 113, 115},
	}
}

func legBars(today []bar) []market.Bar {
	return append(mk(prevDay, 14, 50, flat(100, 40)), mk(day, 9, 15, today)...)
}

// fake is a broker whose orders fill only when the test says.
type fake struct {
	funds     market.Paise
	orders    map[string]*OrderState
	reqs      map[string]OrderReq
	log       []string
	held      map[string]int64
	failPlace bool // the request fails but the order is placed
	lostPlace bool // the request fails and nothing is placed
	n         int
}

func newFake(funds float64) *fake {
	return &fake{funds: market.Rupees(funds), orders: map[string]*OrderState{}, reqs: map[string]OrderReq{}, held: map[string]int64{}}
}

func (f *fake) Name() string                                { return "zerodha" }
func (f *fake) Funds(context.Context) (market.Paise, error) { return f.funds, nil }
func (f *fake) Place(_ context.Context, o OrderReq) (string, error) {
	if f.lostPlace {
		f.lostPlace = false
		return "", errors.New("timeout")
	}
	f.n++
	id := fmt.Sprint(f.n)
	f.orders[id] = &OrderState{ID: id, Tag: o.Tag, Status: Open}
	f.reqs[id] = o
	f.log = append(f.log, fmt.Sprintf("place %s %s %s %d @%s/%s", id, o.Side, o.Type, o.Qty, o.Price, o.Trigger))
	if f.failPlace {
		f.failPlace = false
		return "", errors.New("timeout")
	}
	return id, nil
}
func (f *fake) Modify(_ context.Context, id string, o OrderReq) error {
	if f.orders[id].Status != Open {
		return errors.New("not open")
	}
	f.reqs[id] = o
	f.log = append(f.log, fmt.Sprintf("modify %s %d @%s/%s", id, o.Qty, o.Price, o.Trigger))
	return nil
}
func (f *fake) Cancel(_ context.Context, id string) error {
	o := f.orders[id]
	if o.Status != Open {
		return errors.New("not open")
	}
	o.Status = Cancelled
	f.log = append(f.log, "cancel "+id)
	return nil
}
func (f *fake) Order(_ context.Context, id string) (OrderState, error) {
	o, ok := f.orders[id]
	if !ok {
		return OrderState{}, errors.New("no such order")
	}
	return *o, nil
}
func (f *fake) ByTag(_ context.Context, tag string) (OrderState, bool, error) {
	for _, o := range f.orders {
		if o.Tag == tag {
			return *o, true, nil
		}
	}
	return OrderState{}, false, nil
}
func (f *fake) Position(_ context.Context, in market.Instrument) (int64, error) {
	return f.held[in.Key()], nil
}

// fill fills qty of order id at price, updating the position.
func (f *fake) fill(id string, qty int64, price float64) {
	o := f.orders[id]
	r := f.reqs[id]
	o.Average = market.Rupees(price)
	o.Filled += qty
	if o.Filled == r.Qty {
		o.Status = Complete
	}
	if r.Side == market.Buy {
		f.held[r.Instrument.Key()] += qty
	} else {
		f.held[r.Instrument.Key()] -= qty
	}
}

func (f *fake) last() string { return f.log[len(f.log)-1] }

func cfg() Config {
	// The fixtures cross before 09:30, so the 09:30 gate is off here.
	sc := paper.DefaultHMARetest(day)
	sc.FirstEntry, sc.FirstArm = 0, 0
	return Config{Strategy: sc, Capital: market.Rupees(100000), MaxQty: 1755, Buffer: 0.08}
}

func step(t *testing.T, f *fake, st *State, now time.Time, legs ...paper.Leg) []Event {
	t.Helper()
	ev, err := Step(context.Background(), f, st, legs, cfg(), now)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func after(bs []market.Bar) time.Time { return bs[len(bs)-1].End.Add(20 * time.Second) }

func limitOf(bs []market.Bar) market.Paise {
	es := indicator.HMA(bs, 34)
	return market.Rupees(es[len(es)-1]) / 5 * 5
}

func TestPlacesMovesAndSizesTheBuy(t *testing.T) {
	f := newFake(50000) // less than the ₹1L cap
	st := &State{}
	bs := legBars(rally())
	step(t, f, st, after(bs), paper.Leg{Instrument: ce, Bars: bs})
	lim := limitOf(bs)
	lots := int64(float64(market.Rupees(50000))*0.99) / (int64(lim) * 65)
	if want := fmt.Sprintf("place 1 BUY LIMIT %d @%s/0", lots*65, lim); f.last() != want {
		t.Fatalf("%s, want %s", f.last(), want)
	}
	bs2 := legBars(append(rally(), bar{115, 114, 116}))
	step(t, f, st, after(bs2), paper.Leg{Instrument: ce, Bars: bs2})
	if lim2 := limitOf(bs2); lim2 != lim && !strings.HasPrefix(f.last(), "modify 1 ") {
		t.Fatalf("limit %s → %s: %s", lim, lim2, f.last())
	}
	// A close back below the slow HMA disarms: the buy is cancelled.
	bs3 := legBars(append(rally(), bar{115, 114, 116}, bar{116, 90, 90}, bar{90, 85, 85}, bar{85, 80, 80}))
	step(t, f, st, after(bs3), paper.Leg{Instrument: ce, Bars: bs3})
	if f.last() != "cancel 1" || st.Buy != nil || st.Phase != Flat {
		t.Fatalf("%s %+v", f.last(), st)
	}
}

func TestFillStopTrailAndStopOut(t *testing.T) {
	f := newFake(500000)
	st := &State{}
	bs := legBars(rally())
	step(t, f, st, after(bs), paper.Leg{Instrument: ce, Bars: bs})
	qty := f.reqs["1"].Qty
	f.fill("1", qty, 102)
	bs = legBars(append(rally(), bar{114, 101, 102}))
	ev := step(t, f, st, after(bs), paper.Leg{Instrument: ce, Bars: bs})
	if st.Phase != Long || st.Entry != market.Rupees(102) || st.Stop != market.Rupees(95) || st.Qty != qty {
		t.Fatalf("%+v", st)
	}
	if want := fmt.Sprintf("place 2 SELL SL %d @87.40/95", qty); f.last() != want {
		t.Fatalf("%s, want %s", f.last(), want)
	}
	if !strings.Contains(ev[0].Text, "BOUGHT") {
		t.Fatalf("%v", ev)
	}
	// A higher swing low of 100 confirms two bars after it: the SL rises.
	after3 := []bar{{102, 101, 106}, {106, 100.5, 103}, {103, 100, 104}, {104, 102, 108}, {108, 106, 109}}
	bs = legBars(append(append(rally(), bar{114, 101, 102}), after3...))
	step(t, f, st, after(bs), paper.Leg{Instrument: ce, Bars: bs})
	if want := fmt.Sprintf("modify 2 %d @92/100", qty); f.last() != want {
		t.Fatalf("%s, want %s", f.last(), want)
	}
	// A minute on, the bars from before the raise, one with a low of 100, are
	// not a stop-out at the raised stop.
	n := len(f.log)
	bs = legBars(append(append(append(rally(), bar{114, 101, 102}), after3...), bar{109, 108, 110}))
	step(t, f, st, after(bs), paper.Leg{Instrument: ce, Bars: bs})
	if st.Phase != Long || len(f.log) != n {
		t.Fatalf("%+v %v", st, f.log[n:])
	}
	f.fill("2", qty, 99.5)
	ev = step(t, f, st, after(bs).Add(time.Minute), paper.Leg{Instrument: ce, Bars: bs})
	if st.Phase != Done || st.Exit != market.Rupees(99.5) || st.Reason != "trailed stop" || ev[len(ev)-1].Closed == nil {
		t.Fatalf("%+v %v", st, ev)
	}
}

func TestTimeExitSellsAtALimitAndReprices(t *testing.T) {
	f := newFake(500000)
	st := &State{}
	bs := legBars(rally())
	step(t, f, st, after(bs), paper.Leg{Instrument: ce, Bars: bs})
	f.fill("1", f.reqs["1"].Qty, 102)
	bs = legBars(append(rally(), bar{114, 101, 102}))
	step(t, f, st, after(bs), paper.Leg{Instrument: ce, Bars: bs})
	qty := st.Qty
	step(t, f, st, day.At(15, 15).Add(20*time.Second), paper.Leg{Instrument: ce, Bars: bs})
	if st.Phase != Exiting || f.orders["2"].Status != Cancelled {
		t.Fatalf("%+v %v", st, f.log)
	}
	if want := fmt.Sprintf("place 3 SELL LIMIT %d @93.80/0", qty); f.last() != want {
		t.Fatalf("%s, want %s", f.last(), want)
	}
	f.fill("3", qty, 101)
	ev := step(t, f, st, day.At(15, 16).Add(20*time.Second), paper.Leg{Instrument: ce, Bars: bs})
	if st.Phase != Done || st.Exit != market.Rupees(101) || st.Reason != "15:15" || ev[0].Closed == nil {
		t.Fatalf("%+v %v", st, ev)
	}
	if f.held[ce.Key()] != 0 {
		t.Fatalf("held %d", f.held[ce.Key()])
	}
}

func TestPartialFillKeepsWhatFilled(t *testing.T) {
	f := newFake(500000)
	st := &State{}
	bs := legBars(rally())
	step(t, f, st, after(bs), paper.Leg{Instrument: ce, Bars: bs})
	f.fill("1", 130, 102)
	bs = legBars(append(rally(), bar{114, 101, 102}))
	step(t, f, st, after(bs), paper.Leg{Instrument: ce, Bars: bs})
	if st.Phase != Long || st.Qty != 130 || f.orders["1"].Status != Cancelled || f.reqs["2"].Qty != 130 {
		t.Fatalf("%+v %v", st, f.log)
	}
}

func TestRejectedBuyEndsTheDay(t *testing.T) {
	f := newFake(500000)
	st := &State{}
	bs := legBars(rally())
	step(t, f, st, after(bs), paper.Leg{Instrument: ce, Bars: bs})
	f.orders["1"].Status, f.orders["1"].Message = Rejected, "margin"
	ev := step(t, f, st, after(bs).Add(time.Minute), paper.Leg{Instrument: ce, Bars: bs})
	if st.Phase != Done || len(f.log) != 1 || !strings.Contains(ev[0].Text, "rejected") {
		t.Fatalf("%+v %v %v", st, f.log, ev)
	}
}

func TestFailedPlaceIsFoundByTag(t *testing.T) {
	f := newFake(500000)
	f.failPlace = true
	st := &State{}
	bs := legBars(rally())
	if _, err := Step(context.Background(), f, st, []paper.Leg{{Instrument: ce, Bars: bs}}, cfg(), after(bs)); err == nil {
		t.Fatal("no error")
	}
	if st.Buy == nil || st.Buy.ID != "" {
		t.Fatalf("%+v", st.Buy)
	}
	step(t, f, st, after(bs), paper.Leg{Instrument: ce, Bars: bs})
	if st.Buy.ID != "1" || len(f.orders) != 1 {
		t.Fatalf("%+v %v", st.Buy, f.log)
	}
}

func TestLostPlaceIsPlacedAgain(t *testing.T) {
	f := newFake(500000)
	f.lostPlace = true
	st := &State{}
	bs := legBars(rally())
	Step(context.Background(), f, st, []paper.Leg{{Instrument: ce, Bars: bs}}, cfg(), after(bs))
	step(t, f, st, after(bs), paper.Leg{Instrument: ce, Bars: bs})
	step(t, f, st, after(bs), paper.Leg{Instrument: ce, Bars: bs})
	if len(f.orders) != 1 || st.Buy == nil || st.Buy.ID != "1" {
		t.Fatalf("%+v %v", st.Buy, f.log)
	}
}

func TestPositionGoneNeverSellsShort(t *testing.T) {
	f := newFake(500000)
	st := &State{}
	bs := legBars(rally())
	step(t, f, st, after(bs), paper.Leg{Instrument: ce, Bars: bs})
	f.fill("1", f.reqs["1"].Qty, 102)
	bs = legBars(append(rally(), bar{114, 101, 102}))
	step(t, f, st, after(bs), paper.Leg{Instrument: ce, Bars: bs})
	f.held[ce.Key()] = 0 // squared off by the broker
	step(t, f, st, day.At(15, 15).Add(20*time.Second), paper.Leg{Instrument: ce, Bars: bs})
	if st.Phase != Long {
		t.Fatalf("one missing read ends the day: %+v", st)
	}
	n := len(f.log)
	ev := step(t, f, st, day.At(15, 16).Add(20*time.Second), paper.Leg{Instrument: ce, Bars: bs})
	if st.Phase != Done || !strings.Contains(ev[len(ev)-1].Text, "no longer holds") {
		t.Fatalf("%+v %v", st, ev)
	}
	for _, l := range f.log[n:] {
		if strings.HasPrefix(l, "place") {
			t.Fatalf("placed %s", l)
		}
	}
}

func TestNoEntryAfterThreePM(t *testing.T) {
	f := newFake(500000)
	st := &State{}
	bs := legBars(rally())
	step(t, f, st, after(bs), paper.Leg{Instrument: ce, Bars: bs})
	step(t, f, st, day.At(15, 0).Add(20*time.Second), paper.Leg{Instrument: ce, Bars: bs})
	if st.Phase != Done || f.last() != "cancel 1" {
		t.Fatalf("%+v %v", st, f.log)
	}
}

func TestNotEnoughFundsPlacesNothing(t *testing.T) {
	f := newFake(1000)
	st := &State{}
	bs := legBars(rally())
	ev := step(t, f, st, after(bs), paper.Leg{Instrument: ce, Bars: bs})
	if len(f.log) != 0 || len(ev) != 1 || !strings.Contains(ev[0].Text, "not enough") {
		t.Fatalf("%v %v", f.log, ev)
	}
	if ev := step(t, f, st, after(bs), paper.Leg{Instrument: ce, Bars: bs}); len(ev) != 0 {
		t.Fatalf("posted twice: %v", ev)
	}
}

func TestOneLegAtATime(t *testing.T) {
	f := newFake(500000)
	st := &State{}
	bs := legBars(rally())
	step(t, f, st, after(bs), paper.Leg{Instrument: ce, Bars: bs}, paper.Leg{Instrument: pe, Bars: bs})
	if len(f.orders) != 1 {
		t.Fatalf("%v", f.log)
	}
}
