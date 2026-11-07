package oms

import (
	"strings"
	"testing"

	"calm/internal/market"
)

var (
	day, _ = market.ParseDate("2026-11-10")
	ce     = market.Option(market.NFO, "NIFTY", day, market.Rupees(24000), market.Call, 65)
	pe     = market.Option(market.NFO, "NIFTY", day, market.Rupees(24000), market.Put, 65)
)

func straddle() *Order {
	return New(market.Order{ID: 1, ClientID: "bt000001", Legs: []market.Leg{
		{Instrument: ce, Side: market.Sell, Lots: 2}, {Instrument: pe, Side: market.Sell, Lots: 2},
	}})
}

func TestLegLifecycle(t *testing.T) {
	o := straddle()
	for _, s := range []State{Sent, Acked} {
		if err := o.Move(0, s, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.Fill(0, 65); err != nil || o.LegStates[0].State != PartFilled {
		t.Fatalf("part fill: %v %s", err, o.LegStates[0].State)
	}
	if err := o.Fill(0, 66); err == nil {
		t.Fatal("overfill accepted")
	}
	if err := o.Fill(0, 65); err != nil || o.LegStates[0].State != Filled {
		t.Fatalf("fill: %v %s", err, o.LegStates[0].State)
	}
	if err := o.Move(0, Cancelled, ""); err == nil {
		t.Fatal("a filled leg was cancelled")
	}
	if o.Done() {
		t.Fatal("done with a leg at Intent")
	}
}

func TestUnknownResolvesOnlyByLookup(t *testing.T) {
	o := straddle()
	if err := o.Move(0, Unknown, "timeout"); err == nil {
		t.Fatal("an unsent leg cannot be unknown")
	}
	o.Move(0, Sent, "")
	if err := o.Move(0, Unknown, "timeout"); err != nil {
		t.Fatal(err)
	}
	if err := o.Move(0, Sent, ""); err == nil {
		t.Fatal("an unknown leg must not be sent again")
	}
	if err := o.Move(0, Acked, "found at broker"); err != nil {
		t.Fatal(err)
	}
}

func TestBrokenStructure(t *testing.T) {
	o := straddle()
	o.Move(0, Sent, "")
	o.Fill(0, 130)
	o.Move(1, Sent, "")
	o.Move(1, Rejected, "price band")
	if !o.Done() || !o.Broken() {
		t.Fatalf("done %v broken %v", o.Done(), o.Broken())
	}
}

func TestClientID(t *testing.T) {
	id, err := ClientID("bt", 42)
	if err != nil || id != "bt000042" {
		t.Fatalf("%q %v", id, err)
	}
	for _, bad := range []string{"", "has-dash", "sp ace", strings.Repeat("a", 21)} {
		if CheckClientID(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := ClientID(strings.Repeat("a", 15), 1); err == nil {
		t.Error("over-long ID built")
	}
}
