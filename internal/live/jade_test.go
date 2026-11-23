package live

import (
	"context"
	"strings"
	"testing"
	"time"

	"serene/internal/market"
	"serene/internal/options"
)

var (
	jExp = market.Date{Year: 2027, Month: time.January, Day: 26}
	jNow = time.Date(2026, 12, 14, 15, 20, 30, 0, market.IST)
)

// jChain prices every 100-point strike off a flat 14% vol around fwd.
func jChain(fwd float64, now time.Time) JadeChain {
	ch := JadeChain{Expiry: jExp, Fwd: fwd, Now: now, Quotes: map[JadeStrike]JadeQuote{}}
	years := options.YearsToExpiry(now, jExp)
	for k := 20000; k <= 28000; k += 50 {
		for _, call := range []bool{true, false} {
			px := options.Price(options.Input{Forward: fwd, Strike: float64(k), Years: years, Rate: 0.065, Call: call}, 0.14)
			if px < 0.5 {
				continue
			}
			ch.Quotes[JadeStrike{k, call}] = JadeQuote{Last: px, Bid: px - 0.5, Ask: px + 0.5, Contracts: 500}
		}
	}
	return ch
}

func TestPickJadeCreditNearTarget(t *testing.T) {
	p, skip := DefaultJade.PickJade(jChain(24000, jNow))
	if skip != "" {
		t.Fatal(skip)
	}
	if p.Credit < 150 || p.Credit > 250 || p.Wing != p.Call+100 || p.Put >= 24000 || p.Call <= 24000 || p.Put%100 != 0 {
		t.Fatalf("pick %+v", p)
	}
	// A tighter target moves the shorts out.
	r := DefaultJade
	r.Target = 120
	q, skip := r.PickJade(jChain(24000, jNow))
	if skip != "" || q.Put >= p.Put || q.Call <= p.Call {
		t.Fatalf("target 120: %+v %s (200 was %+v)", q, skip, p)
	}
}

func TestPickJadeSkipsIlliquidAndLowCredit(t *testing.T) {
	ch := jChain(24000, jNow)
	for k, q := range ch.Quotes {
		q.Contracts = 10
		ch.Quotes[k] = q
	}
	if _, skip := DefaultJade.PickJade(ch); skip != "no liquid strikes" {
		t.Fatal(skip)
	}
	r := DefaultJade
	r.Width, r.Target = 300, 250 // a 300-point wing can't be covered at 0.10-0.40 delta here
	if _, skip := r.PickJade(jChain(24000, time.Date(2027, 1, 20, 15, 20, 0, 0, market.IST))); skip == "" {
		t.Fatal("expected a skip near expiry")
	}
}

func TestEntryExpiryAndMonthlies(t *testing.T) {
	d := func(y int, m time.Month, dd int) market.Date { return market.Date{Year: y, Month: m, Day: dd} }
	exps := []market.Date{d(2026, 12, 22), d(2026, 12, 29), d(2027, 1, 5), d(2027, 1, 26), d(2027, 2, 23)}
	m := JadeMonthlies(exps)
	if len(m) != 3 || m[0] != (market.Date{Year: 2026, Month: 12, Day: 29}) || m[1] != jExp {
		t.Fatal(m)
	}
	if e, ok := DefaultJade.EntryExpiry(market.Date{Year: 2026, Month: 12, Day: 14}, m); !ok || e != jExp {
		t.Fatal(e, ok)
	}
	if _, ok := DefaultJade.EntryExpiry(market.Date{Year: 2026, Month: 12, Day: 10}, m); ok {
		t.Fatal("47 days out is too early")
	}
	if _, ok := DefaultJade.EntryExpiry(market.Date{Year: 2026, Month: 12, Day: 18}, m); ok {
		t.Fatal("39 days out is too late")
	}
}

func jCfg() JadeConfig { return JadeConfig{Rules: DefaultJade, Buffer: 0.02, MaxMods: 3} }

func jSnap(now time.Time, pick *JadePick, quotes map[string]JadeQuote) JadeSnap {
	return JadeSnap{Now: now, Decide: true, Entry: pick, MarginOK: true, Quotes: quotes, Lot: 65, Tick: market.Rupees(0.05)}
}

func jStep(t *testing.T, f *fake, st *JadeState, s JadeSnap) []JadeEvent {
	t.Helper()
	evs, err := StepJade(context.Background(), f, st, s, jCfg())
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func jQuotes(put, call, wing float64) map[string]JadeQuote {
	q := func(px float64) JadeQuote { return JadeQuote{Last: px, Bid: px - 0.5, Ask: px + 0.5, Contracts: 500} }
	return map[string]JadeQuote{
		JadeKey(jExp, 23400, false): q(put),
		JadeKey(jExp, 25000, true):  q(call),
		JadeKey(jExp, 25100, true):  q(wing),
	}
}

var jPick = &JadePick{Expiry: jExp, Put: 23400, Call: 25000, Wing: 25100, PutPx: 160, CallPx: 80, WingPx: 60, Delta: 0.2, Credit: 180}

// enterAll runs a full entry, filling each leg at its mark.
func enterAll(t *testing.T, f *fake, st *JadeState) {
	t.Helper()
	qs := jQuotes(160, 80, 60)
	evs := jStep(t, f, st, jSnap(jNow, jPick, qs))
	if st.Phase != JadeEntering || len(evs) != 1 || !st.DoneFor(jExp) {
		t.Fatalf("decide: %+v %+v", st, evs)
	}
	// Wing bought first at the ask + 2%, then the call and the put sold.
	want := []string{"place 1 BUY LIMIT 65 @61.75/0", "place 2 SELL LIMIT 65 @77.90/0", "place 3 SELL LIMIT 65 @156.30/0"}
	fills := []float64{60.5, 79.5, 159.5}
	for i, w := range want {
		jStep(t, f, st, jSnap(jNow, jPick, qs))
		if f.last() != w {
			t.Fatalf("leg %d: %s, want %s", i, f.last(), w)
		}
		f.fill(f.orders[string(rune('1'+i))].ID, 65, fills[i])
		jStep(t, f, st, jSnap(jNow, jPick, qs)) // records the fill
	}
	evs = jStep(t, f, st, jSnap(jNow, jPick, qs))
	if st.Phase != JadeOpen || len(evs) != 1 || evs[0].Kind != "entered" {
		t.Fatalf("not open: %+v %+v", st, evs)
	}
	if c := st.Credit(); c < 178.49 || c > 178.51 {
		t.Fatalf("credit %.2f", c)
	}
}

func TestJadeEntersThenTakesProfitShortsFirst(t *testing.T) {
	f := newFake(0)
	st := &JadeState{}
	enterAll(t, f, st)
	// Same day: no decision again.
	if evs := jStep(t, f, st, jSnap(jNow, nil, jQuotes(100, 40, 30))); len(evs) != 0 || st.Phase != JadeOpen {
		t.Fatalf("decided twice in a day: %+v", evs)
	}
	// Next day, 50% of the credit is made: P&L = 178.5 - 100 - 40 + 30 = 68.5 < 89.25, so hold.
	day2 := jNow.AddDate(0, 0, 1)
	evs := jStep(t, f, st, jSnap(day2, nil, jQuotes(100, 40, 30)))
	if st.Phase != JadeOpen || len(evs) != 1 || evs[0].Kind != "mark" {
		t.Fatalf("day2 %+v %+v", st, evs)
	}
	day3 := jNow.AddDate(0, 0, 2)
	jStep(t, f, st, jSnap(day3, nil, jQuotes(70, 30, 20))) // 178.5 - 70 - 30 + 20 = 98.5
	if st.Phase != JadeExiting || st.Reason != "tp" {
		t.Fatalf("no tp: %+v", st)
	}
	qs := jQuotes(70, 30, 20)
	want := []string{"place 4 BUY LIMIT 65 @71.95/0", "place 5 BUY LIMIT 65 @31.15/0", "place 6 SELL LIMIT 65 @19.10/0"}
	for i, w := range want {
		jStep(t, f, st, jSnap(day3, nil, qs))
		if f.last() != w {
			t.Fatalf("exit leg %d: %s, want %s", i, f.last(), w)
		}
		f.fill(string(rune('4'+i)), 65, []float64{70.5, 30.5, 19.5}[i])
		jStep(t, f, st, jSnap(day3, nil, qs))
	}
	evs = jStep(t, f, st, jSnap(day3, nil, qs))
	if st.Phase != JadeFlat || len(evs) != 1 || evs[0].Kind != "exited" {
		t.Fatalf("not flat: %+v %+v", st, evs)
	}
	// The expiry isn't entered again.
	if jStep(t, f, st, jSnap(day3.AddDate(0, 0, 1), jPick, qs)); st.Phase != JadeFlat {
		t.Fatal("re-entered the same expiry")
	}
}

func TestJadeTimeExitAt21Days(t *testing.T) {
	f := newFake(0)
	st := &JadeState{}
	enterAll(t, f, st)
	d := time.Date(2027, 1, 5, 15, 20, 30, 0, market.IST) // 21 days to 2027-01-26
	jStep(t, f, st, jSnap(d, nil, jQuotes(150, 85, 62)))
	if st.Phase != JadeExiting || st.Reason != "time" {
		t.Fatalf("%+v", st)
	}
}

func TestJadeHaltsWhenAccountHoldsLess(t *testing.T) {
	f := newFake(0)
	st := &JadeState{}
	enterAll(t, f, st)
	f.held[JadeKey(jExp, 23400, false)] = 0 // the put was closed by hand
	jStep(t, f, st, jSnap(jNow.AddDate(0, 0, 2), nil, jQuotes(70, 30, 20)))
	evs := jStep(t, f, st, jSnap(jNow.AddDate(0, 0, 2), nil, jQuotes(70, 30, 20)))
	if st.Halted == "" || len(evs) != 1 || evs[0].Kind != "halt" {
		t.Fatalf("%+v %+v", st, evs)
	}
	n := len(f.log)
	jStep(t, f, st, jSnap(jNow.AddDate(0, 0, 3), nil, jQuotes(70, 30, 20)))
	if len(f.log) != n {
		t.Fatal("placed after halting")
	}
}

func TestJadeNoMarginSkipsTheExpiry(t *testing.T) {
	f := newFake(0)
	st := &JadeState{}
	s := jSnap(jNow, jPick, jQuotes(160, 80, 60))
	s.MarginOK = false
	evs := jStep(t, f, st, s)
	if st.Phase != JadeFlat || !st.DoneFor(jExp) || len(evs) != 1 || !strings.Contains(evs[0].Text, "margin") || len(f.log) != 0 {
		t.Fatalf("%+v %+v", st, evs)
	}
}

func TestJadeRepricesThenHaltsUnfilled(t *testing.T) {
	f := newFake(0)
	st := &JadeState{}
	qs := jQuotes(160, 80, 60)
	jStep(t, f, st, jSnap(jNow, jPick, qs))
	jStep(t, f, st, jSnap(jNow, jPick, qs)) // places the wing at 61.75
	for i, px := range []float64{61, 62, 63} {
		q := jQuotes(160, 80, px)
		jStep(t, f, st, jSnap(jNow, jPick, q))
		if !strings.HasPrefix(f.last(), "modify 1 65 @") || st.Order.Mods != i+1 {
			t.Fatalf("reprice %d: %s", i, f.last())
		}
	}
	evs := jStep(t, f, st, jSnap(jNow, jPick, jQuotes(160, 80, 64)))
	if st.Halted == "" || f.last() != "cancel 1" || len(evs) != 1 {
		t.Fatalf("%s %+v %+v", f.last(), st, evs)
	}
}

func TestJadePartialFillPlacesTheRest(t *testing.T) {
	f := newFake(0)
	st := &JadeState{}
	qs := jQuotes(160, 80, 60)
	jStep(t, f, st, jSnap(jNow, jPick, qs))
	jStep(t, f, st, jSnap(jNow, jPick, qs))
	f.fill("1", 25, 60.5)
	f.orders["1"].Status = Cancelled // cancelled by hand after 25 filled
	jStep(t, f, st, jSnap(jNow, jPick, qs))
	if st.Legs[0].EntryFilled != 25 || st.Order != nil {
		t.Fatalf("%+v", st)
	}
	jStep(t, f, st, jSnap(jNow, jPick, qs))
	if f.last() != "place 2 BUY LIMIT 40 @61.75/0" {
		t.Fatal(f.last())
	}
}
