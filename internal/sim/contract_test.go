package sim

import (
	"context"
	"testing"
	"time"

	"calm/internal/broker/brokertest"
	"calm/internal/market"
)

func TestBrokerContract(t *testing.T) {
	later := market.Option(market.NFO, "NIFTY", day.AddDays(7), market.Rupees(24000), market.Call, 65)
	brokertest.Run(t, brokertest.Harness{
		Start: day.At(10, 0),
		New: func(t *testing.T) (brokertest.Venue, market.Instrument, time.Duration) {
			b, err := New(Config{Tick: 5, MaxWait: 3 * time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			return b, later, 3 * time.Minute
		},
		Trade: func(t *testing.T, v brokertest.Venue, inst market.Instrument, at time.Time, traded bool) []market.Fill {
			bars := map[string]market.Bar{}
			if traded {
				bars[inst.Key()] = market.Bar{Key: inst.Key(), Time: at, End: at.Add(time.Minute), Open: 10000, Close: 10000, Volume: 1}
			}
			fills, err := v.OnMinute(context.Background(), at, bars)
			if err != nil {
				t.Fatal(err)
			}
			return fills
		},
	})
}

func TestExpiredPositionsDropAtEndOfDay(t *testing.T) {
	ctx := context.Background()
	b, _ := New(Config{Tick: 5, MaxWait: time.Minute})
	b.Submit(ctx, market.Order{ID: 1, Placed: day.At(10, 0), Legs: []market.Leg{{Instrument: ce, Side: market.Sell, Lots: 1}}})
	b.OnMinute(ctx, day.At(10, 1), bars(100, ce))
	if ps, _ := b.Positions(ctx); len(ps) != 1 {
		t.Fatalf("positions %+v", ps)
	}
	b.EndOfDay(ctx) // ce expires today
	if ps, _ := b.Positions(ctx); len(ps) != 0 {
		t.Fatalf("expired position kept: %+v", ps)
	}
}
