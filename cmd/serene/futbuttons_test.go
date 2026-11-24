package main

import (
	"testing"

	"serene/internal/kite"
	"serene/internal/market"
)

func TestButtonFutureNeeds15Days(t *testing.T) {
	d := func(s string) market.Date {
		v, err := market.ParseDate(s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	ins := []kite.Instrument{
		{Exchange: "MCX", Type: "FUT", Name: "SILVERM", Symbol: "SILVERM27FEBFUT", Expiry: d("2027-02-26")},
		{Exchange: "MCX", Type: "FUT", Name: "SILVERM", Symbol: "SILVERM26NOVFUT", Expiry: d("2026-11-30")},
		{Exchange: "MCX", Type: "FUT", Name: "SILVERM", Symbol: "SILVERM26DECFUT", Expiry: d("2026-12-09")},
	}
	for _, c := range []struct{ today, want string }{
		{"2026-11-10", "SILVERM26NOVFUT"}, // 20 days
		{"2026-11-15", "SILVERM26NOVFUT"}, // exactly 15
		{"2026-11-16", "SILVERM26DECFUT"}, // Nov at 14 days, Dec at 23
		{"2026-11-24", "SILVERM26DECFUT"}, // Dec at exactly 15
		{"2026-11-25", "SILVERM27FEBFUT"}, // Dec at 14
	} {
		today := d(c.today)
		got, ok := buttonFuture(mcxFutures(ins, "SILVERM", today), today)
		if !ok || got.Symbol != c.want {
			t.Errorf("%s: got %q %v, want %s", c.today, got.Symbol, ok, c.want)
		}
	}
	if _, ok := buttonFuture(mcxFutures(ins, "SILVERM", d("2027-02-20")), d("2027-02-20")); ok {
		t.Error("a future under 15 days out got a button")
	}
}
