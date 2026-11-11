package sim

import (
	"context"
	"testing"
	"time"

	"serene/internal/market"
)

var (
	day, _ = market.ParseDate("2026-11-06")
	ce     = market.Option(market.NFO, "NIFTY", day, market.Rupees(24000), market.Call, 65)
	pe     = market.Option(market.NFO, "NIFTY", day, market.Rupees(24000), market.Put, 65)
)

func bars(price market.Paise, is ...market.Instrument) map[string]market.Bar {
	m := map[string]market.Bar{}
	for _, i := range is {
		m[i.Key()] = market.Bar{Key: i.Key(), Open: price, Close: price * 2, Volume: 75}
	}
	return m
}

func TestFillsAtOpenWithSlippageRoundedAgainstTrader(t *testing.T) {
	b := BarOpen{SlippageBps: 10, MinTicks: 1, Tick: 5}
	cases := []struct {
		side  market.Side
		price market.Paise
		want  market.Paise
	}{
		{market.Buy, 10000, 10010}, // 10bps = 10p
		{market.Sell, 10000, 9990}, //
		{market.Buy, 10003, 10015}, // 10p slip -> 10013 -> up to tick
		{market.Sell, 10003, 9990}, // 9993 -> down to tick
		{market.Buy, 100, 105},     // min tick applies
		{market.Sell, 5, 5},        // price cannot go below one tick
	}
	for _, c := range cases {
		if got := b.slip(c.price, c.side); got != c.want {
			t.Errorf("%s at %d: got %d want %d", c.side, c.price, got, c.want)
		}
	}
}

func TestMultiLegWaitsForAllLegsThenTimesOut(t *testing.T) {
	ctx := context.Background()
	b, _ := New(Config{Tick: 5, MaxWait: 2 * time.Minute})
	o := market.Order{ID: 1, Tag: "x", Placed: day.At(10, 0), Legs: []market.Leg{
		{Instrument: ce, Side: market.Sell, Lots: 1}, {Instrument: pe, Side: market.Sell, Lots: 1},
	}}
	if err := b.Submit(ctx, o); err != nil {
		t.Fatal(err)
	}
	// Only one leg traded: nothing fills.
	f, _ := b.OnMinute(ctx, day.At(10, 0), bars(100, ce))
	if len(f) != 0 || b.Pending() != 1 {
		t.Fatalf("fills %d pending %d", len(f), b.Pending())
	}
	// Both legs trade: both fill, and they use the open.
	f, _ = b.OnMinute(ctx, day.At(10, 1), bars(100, ce, pe))
	if len(f) != 2 || f[0].Price != 100 || f[0].Qty != 65 {
		t.Fatalf("fills %+v", f)
	}

	if err := b.Submit(ctx, o); err != nil {
		t.Fatal(err)
	}
	b.OnMinute(ctx, day.At(10, 2), bars(100, ce))
	if b.Pending() != 1 {
		t.Fatal("cancelled too early")
	}
	b.OnMinute(ctx, day.At(10, 3), bars(100, ce))
	if b.Pending() != 0 || b.Unfilled != 1 {
		t.Fatalf("pending %d unfilled %d", b.Pending(), b.Unfilled)
	}
}

func TestEndOfDayCancels(t *testing.T) {
	ctx := context.Background()
	b, _ := New(Config{Tick: 5, MaxWait: time.Hour})
	b.Submit(ctx, market.Order{Legs: []market.Leg{{Instrument: ce, Side: market.Buy, Lots: 1}}})
	b.EndOfDay(ctx)
	if b.Pending() != 0 || b.Unfilled != 1 {
		t.Fatalf("pending %d unfilled %d", b.Pending(), b.Unfilled)
	}
}

func TestRejectsZeroQuantity(t *testing.T) {
	b, _ := New(Config{Tick: 5})
	noLot := ce
	noLot.LotSize = 0
	err := b.Submit(context.Background(), market.Order{Legs: []market.Leg{{Instrument: noLot, Side: market.Buy, Lots: 1}}})
	if err == nil {
		t.Error("zero lot size accepted")
	}
}
