// Package brokertest is the contract every broker implementation must meet.
// The simulator runs it now; paper, Zerodha and Upstox adapters, and the fake
// broker used for failure tests, will run the same suite.
package brokertest

import (
	"context"
	"errors"
	"testing"
	"time"

	"serene/internal/broker"
	"serene/internal/market"
	"serene/internal/oms"
)

// Venue is a broker under test and a way to move its market.
type Venue interface {
	broker.Broker
	broker.Account
}

// Harness builds a fresh venue for each check.
type Harness struct {
	// New returns a venue, an instrument it can trade, and the longest an order
	// may wait unfilled before the venue cancels it.
	New func(t *testing.T) (v Venue, inst market.Instrument, maxWait time.Duration)
	// Trade advances the venue one minute from t; traded says whether the
	// instrument trades in that minute.
	Trade func(t *testing.T, v Venue, inst market.Instrument, at time.Time, traded bool) []market.Fill
	// Start is a minute inside a session.
	Start time.Time
}

func order(id int64, inst market.Instrument, side market.Side, placed time.Time) market.Order {
	cid, _ := oms.ClientID("ct", id)
	return market.Order{ID: id, ClientID: cid, Tag: "contract", Kind: market.MarketOrder, Product: market.Carry,
		Placed: placed, Legs: []market.Leg{{Instrument: inst, Side: side, Lots: 1}}}
}

func state(t *testing.T, v Venue, id int64) oms.State {
	t.Helper()
	os, err := v.Orders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range os {
		if o.ID == id {
			return o.LegStates[0].State
		}
	}
	t.Fatalf("order %d missing from Orders", id)
	return ""
}

// Run checks the contract.
func Run(t *testing.T, h Harness) {
	ctx := context.Background()

	t.Run("fill updates order and position", func(t *testing.T) {
		v, inst, _ := h.New(t)
		if err := v.Submit(ctx, order(1, inst, market.Sell, h.Start)); err != nil {
			t.Fatal(err)
		}
		if v.Pending() != 1 {
			t.Fatalf("pending %d", v.Pending())
		}
		fills := h.Trade(t, v, inst, h.Start.Add(time.Minute), true)
		if len(fills) != 1 || fills[0].OrderID != 1 || fills[0].Qty != inst.LotSize || fills[0].Side != market.Sell {
			t.Fatalf("fills = %+v", fills)
		}
		if s := state(t, v, 1); s != oms.Filled || v.Pending() != 0 {
			t.Fatalf("state %s pending %d", s, v.Pending())
		}
		ps, err := v.Positions(ctx)
		if err != nil || len(ps) != 1 || ps[0].Key != inst.Key() || ps[0].Qty != -inst.LotSize {
			t.Fatalf("positions = %+v, %v", ps, err)
		}
	})

	t.Run("cancel withdraws an open order once", func(t *testing.T) {
		v, inst, _ := h.New(t)
		v.Submit(ctx, order(1, inst, market.Buy, h.Start))
		if err := v.Cancel(ctx, 1); err != nil {
			t.Fatal(err)
		}
		if s := state(t, v, 1); s != oms.Cancelled || v.Pending() != 0 {
			t.Fatalf("state %s pending %d", s, v.Pending())
		}
		if err := v.Cancel(ctx, 1); !errors.Is(err, broker.ErrNotOpen) {
			t.Fatalf("second cancel: %v", err)
		}
		if err := v.Cancel(ctx, 99); !errors.Is(err, broker.ErrNotOpen) {
			t.Fatalf("cancel of unknown order: %v", err)
		}
		if fills := h.Trade(t, v, inst, h.Start.Add(time.Minute), true); len(fills) != 0 {
			t.Fatalf("a cancelled order filled: %+v", fills)
		}
	})

	t.Run("unfilled order is cancelled after max wait", func(t *testing.T) {
		v, inst, wait := h.New(t)
		v.Submit(ctx, order(1, inst, market.Buy, h.Start))
		for at := h.Start.Add(time.Minute); !at.After(h.Start.Add(wait + time.Minute)); at = at.Add(time.Minute) {
			h.Trade(t, v, inst, at, false)
		}
		if s := state(t, v, 1); s != oms.Cancelled || v.Pending() != 0 {
			t.Fatalf("state %s pending %d", s, v.Pending())
		}
	})

	t.Run("end of day cancels open orders", func(t *testing.T) {
		v, inst, _ := h.New(t)
		v.Submit(ctx, order(1, inst, market.Buy, h.Start))
		if err := v.EndOfDay(ctx); err != nil {
			t.Fatal(err)
		}
		if s := state(t, v, 1); s != oms.Cancelled || v.Pending() != 0 {
			t.Fatalf("state %s pending %d", s, v.Pending())
		}
	})

	t.Run("an order with no quantity is refused", func(t *testing.T) {
		v, inst, _ := h.New(t)
		o := order(1, inst, market.Buy, h.Start)
		o.Legs[0].Lots = 0
		if err := v.Submit(ctx, o); err == nil {
			t.Fatal("accepted")
		}
		if v.Pending() != 0 {
			t.Fatalf("pending %d", v.Pending())
		}
	})
}
