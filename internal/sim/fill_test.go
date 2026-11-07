package sim

import (
	"context"
	"testing"
	"time"

	"calm/internal/broker/brokertest"
	"calm/internal/market"
)

type quotes map[string]Quote

func (q quotes) Quote(key string) (Quote, bool) { v, ok := q[key]; return v, ok }

func TestQuoteFillPaysTheTouch(t *testing.T) {
	now := day.At(10, 0)
	q := quotes{ce.Key(): {Bid: 9900, Ask: 10100, Time: now.Add(-10 * time.Second)}}
	m := QuoteFill{Quotes: q, MaxAge: time.Minute}
	cases := []struct {
		name  string
		leg   market.Leg
		at    time.Time
		price market.Paise
		ok    bool
	}{
		{"buy pays the ask", market.Leg{Instrument: ce, Side: market.Buy}, now, 10100, true},
		{"sell hits the bid", market.Leg{Instrument: ce, Side: market.Sell}, now, 9900, true},
		{"stale quote refused", market.Leg{Instrument: ce, Side: market.Buy}, now.Add(time.Minute), 0, false},
		{"no quote", market.Leg{Instrument: pe, Side: market.Buy}, now, 0, false},
		{"buy limit below ask waits", market.Leg{Instrument: ce, Side: market.Buy, Limit: 10000}, now, 0, false},
		{"buy limit at ask fills at ask", market.Leg{Instrument: ce, Side: market.Buy, Limit: 10100}, now, 10100, true},
		{"sell limit above bid waits", market.Leg{Instrument: ce, Side: market.Sell, Limit: 10000}, now, 0, false},
		{"sell limit below bid fills at bid", market.Leg{Instrument: ce, Side: market.Sell, Limit: 9800}, now, 9900, true},
	}
	for _, c := range cases {
		p, ok := m.Price(c.leg, c.at, nil)
		if p != c.price || ok != c.ok {
			t.Errorf("%s: got %d %v want %d %v", c.name, p, ok, c.price, c.ok)
		}
	}
	if _, ok := (QuoteFill{Quotes: quotes{ce.Key(): {Bid: 0, Ask: 10100, Time: now}}, MaxAge: time.Minute}).
		Price(market.Leg{Instrument: ce, Side: market.Sell}, now, nil); ok {
		t.Error("a sell filled with no bid")
	}
}

func TestBarOpenRefusesLimitOrders(t *testing.T) {
	b, _ := New(Config{Tick: 5})
	o := market.Order{ID: 1, Kind: market.LimitOrder, Legs: []market.Leg{{Instrument: ce, Side: market.Buy, Lots: 1, Limit: 100}}}
	if err := b.Submit(context.Background(), o); err == nil {
		t.Fatal("bar-open model accepted a limit order")
	}
	q, _ := New(Config{Tick: 5, Fills: QuoteFill{Quotes: quotes{}, MaxAge: time.Minute}})
	if err := q.Submit(context.Background(), o); err != nil {
		t.Fatalf("quote model refused a limit order: %v", err)
	}
}

// The paper fill model passes the same broker contract as the backtest one.
func TestQuoteFillContract(t *testing.T) {
	later := market.Option(market.NFO, "NIFTY", day.AddDays(7), market.Rupees(24000), market.Call, 65)
	brokertest.Run(t, brokertest.Harness{
		Start: day.At(10, 0),
		New: func(t *testing.T) (brokertest.Venue, market.Instrument, time.Duration) {
			b, err := New(Config{Tick: 5, MaxWait: 3 * time.Minute, Fills: QuoteFill{Quotes: quotes{}, MaxAge: time.Minute}})
			if err != nil {
				t.Fatal(err)
			}
			return b, later, 3 * time.Minute
		},
		Trade: func(t *testing.T, v brokertest.Venue, inst market.Instrument, at time.Time, traded bool) []market.Fill {
			q := v.(*Broker).fills.(QuoteFill).Quotes.(quotes)
			if traded {
				q[inst.Key()] = Quote{Bid: 9990, Ask: 10010, Time: at}
			} else {
				delete(q, inst.Key())
			}
			fills, err := v.OnMinute(context.Background(), at, nil)
			if err != nil {
				t.Fatal(err)
			}
			return fills
		},
	})
}
