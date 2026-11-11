package risk

import (
	"testing"
	"time"

	"serene/internal/market"
)

var (
	day, _ = market.ParseDate("2026-11-10")
	ce     = market.Option(market.NFO, "NIFTY", day, market.Rupees(24000), market.Call, 65)
	pe     = market.Option(market.NFO, "NIFTY", day, market.Rupees(24000), market.Put, 65)
)

func sell(i market.Instrument, lots int64) market.Leg {
	return market.Leg{Instrument: i, Side: market.Sell, Lots: lots}
}
func buy(i market.Instrument, lots int64) market.Leg {
	return market.Leg{Instrument: i, Side: market.Buy, Lots: lots}
}

func state(now time.Time, start, equity market.Paise, held map[string]Holding) State {
	return State{Now: now, DayStartEquity: start, Equity: equity, Held: held,
		Last: func(key string) (market.Bar, bool) {
			return market.Bar{Key: key, End: now, Close: 10000}, true
		}}
}

func gate(t *testing.T, l Limits) *Gate {
	t.Helper()
	g, err := New(l)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestIsExit(t *testing.T) {
	short := map[string]Holding{ce.Key(): {Qty: -130, LotSize: 65}, pe.Key(): {Qty: -130, LotSize: 65}}
	cases := []struct {
		name string
		legs []market.Leg
		want bool
	}{
		{"buy back both", []market.Leg{buy(ce, 2), buy(pe, 2)}, true},
		{"buy back part", []market.Leg{buy(ce, 1)}, true},
		{"adds to the short", []market.Leg{sell(ce, 1)}, false},
		{"passes flat", []market.Leg{buy(ce, 3)}, false},
		{"closes one, opens another", []market.Leg{buy(ce, 2), buy(market.Option(market.NFO, "NIFTY", day, market.Rupees(24100), market.Call, 65), 1)}, false},
		{"two legs on one contract summing past flat", []market.Leg{buy(ce, 2), buy(ce, 1)}, false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		if got := IsExit(short, c.legs); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestDailyLossHaltsEntriesForTheDayButNotExits(t *testing.T) {
	g := gate(t, Limits{MaxDailyLoss: market.Rupees(10000)})
	now := day.At(11, 0)
	held := map[string]Holding{ce.Key(): {Qty: -65, LotSize: 65}}
	if v := g.Check(state(now, market.Rupees(1e6), market.Rupees(995000), held), []market.Leg{sell(pe, 1)}); !v.Allowed {
		t.Fatalf("within limit: %+v", v)
	}
	if v := g.Check(state(now, market.Rupees(1e6), market.Rupees(990000), held), []market.Leg{sell(pe, 1)}); v.Allowed || v.Reason != "daily_loss" {
		t.Fatalf("at limit: %+v", v)
	}
	// The halt holds after the loss recovers, the same day.
	later := now.Add(time.Hour)
	if v := g.Check(state(later, market.Rupees(1e6), market.Rupees(1e6), held), []market.Leg{sell(pe, 1)}); v.Allowed || v.Reason != "daily_loss" {
		t.Fatalf("after recovery: %+v", v)
	}
	if v := g.Check(state(later, market.Rupees(1e6), market.Rupees(980000), held), []market.Leg{buy(ce, 1)}); !v.Allowed || !v.Exit {
		t.Fatalf("exit while halted: %+v", v)
	}
	// The next IST day starts clean.
	next := day.AddDays(1).At(9, 20)
	if v := g.Check(state(next, market.Rupees(980000), market.Rupees(980000), held), []market.Leg{sell(pe, 1)}); !v.Allowed {
		t.Fatalf("next day: %+v", v)
	}
}

func TestManualHaltLatchesUntilReset(t *testing.T) {
	g := gate(t, Limits{})
	now := day.At(10, 0)
	g.Halt("")
	for _, at := range []time.Time{now, day.AddDays(3).At(10, 0)} {
		if v := g.Check(state(at, 0, 0, nil), []market.Leg{sell(ce, 1)}); v.Allowed || v.Reason != "halted" {
			t.Fatalf("at %s: %+v", at, v)
		}
	}
	g.Reset()
	if v := g.Check(state(now, 0, 0, nil), []market.Leg{sell(ce, 1)}); !v.Allowed {
		t.Fatalf("after reset: %+v", v)
	}
}

func TestSizeRateAndQuoteLimits(t *testing.T) {
	now := day.At(10, 0)
	g := gate(t, Limits{MaxLotsPerLeg: 2, MaxOpenLots: 3, MaxOrdersPerMinute: 2,
		MaxQuoteAge: 2 * time.Minute, MaxLimitAwayPct: 20})
	check := func(s State, legs ...market.Leg) Verdict { return g.Check(s, legs) }
	if v := check(state(now, 0, 0, nil), sell(ce, 3)); v.Reason != "max_lots" {
		t.Fatalf("lots: %+v", v)
	}
	held := map[string]Holding{ce.Key(): {Qty: -130, LotSize: 65}}
	if v := check(state(now, 0, 0, held), sell(pe, 2)); v.Reason != "max_open_lots" {
		t.Fatalf("open lots: %+v", v)
	}
	stale := state(now, 0, 0, nil)
	stale.Last = func(key string) (market.Bar, bool) {
		return market.Bar{End: now.Add(-3 * time.Minute), Close: 10000}, true
	}
	if v := check(stale, sell(pe, 1)); v.Reason != "stale_quote" {
		t.Fatalf("stale: %+v", v)
	}
	far := sell(pe, 1)
	far.Limit = 7000
	if v := check(state(now, 0, 0, nil), far); v.Reason != "price_collar" {
		t.Fatalf("collar: %+v", v)
	}
	// Refused orders do not count towards the rate; two allowed ones do.
	for range 2 {
		if v := check(state(now, 0, 0, nil), sell(pe, 1)); !v.Allowed {
			t.Fatalf("within rate: %+v", v)
		}
	}
	if v := check(state(now.Add(30*time.Second), 0, 0, nil), sell(pe, 1)); v.Reason != "order_rate" {
		t.Fatalf("rate: %+v", v)
	}
	if v := check(state(now.Add(time.Minute), 0, 0, nil), sell(pe, 1)); !v.Allowed {
		t.Fatalf("after the window: %+v", v)
	}
}

func TestNegativeLimitsRefused(t *testing.T) {
	if _, err := New(Limits{MaxOpenLots: -1}); err == nil {
		t.Fatal("accepted")
	}
}
