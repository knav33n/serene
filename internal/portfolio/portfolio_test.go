package portfolio

import (
	"testing"

	"calm/internal/market"
)

var (
	exp, _ = market.ParseDate("2026-11-09")
	ce     = market.Option(market.NFO, "NIFTY", exp, market.Rupees(24000), market.Call, 65)
)

func fill(side market.Side, qty int64, price, charges market.Paise) market.Fill {
	return market.Fill{Tag: "t", Instrument: ce, Side: side, Qty: qty, Price: price,
		Time: exp.At(10, 0), Kind: market.Traded, Charges: charges}
}

func TestLongRoundTrip(t *testing.T) {
	p := New(0)
	p.Apply(fill(market.Buy, 65, 10000, 50))
	p.Apply(fill(market.Sell, 65, 12000, 70))
	if len(p.Trades) != 1 || len(p.Positions()) != 0 {
		t.Fatalf("trades %d positions %d", len(p.Trades), len(p.Positions()))
	}
	tr := p.Trades[0]
	if tr.Gross != 130000 || tr.Charges != 120 || tr.Net() != 129880 {
		t.Errorf("bad trade: %+v", tr)
	}
	if p.Cash != tr.Net() {
		t.Errorf("cash %d does not match net %d", p.Cash, tr.Net())
	}
}

func TestShortRoundTrip(t *testing.T) {
	p := New(0)
	p.Apply(fill(market.Sell, 65, 10000, 0))
	p.Apply(fill(market.Buy, 65, 4000, 0))
	if tr := p.Trades[0]; tr.Gross != 390000 || tr.Side != market.Sell {
		t.Errorf("bad trade: %+v", tr)
	}
}

func TestScalingInAndOutStaysExact(t *testing.T) {
	p := New(0)
	p.Apply(fill(market.Buy, 65, 10000, 0))
	p.Apply(fill(market.Buy, 130, 10001, 0)) // awkward average
	p.Apply(fill(market.Sell, 65, 11000, 0))
	p.Apply(fill(market.Sell, 65, 9000, 0))
	p.Apply(fill(market.Sell, 65, 12000, 0))
	if len(p.Trades) != 1 {
		t.Fatalf("trades %d", len(p.Trades))
	}
	if p.Cash != p.Trades[0].Gross {
		t.Errorf("rounding leaked: cash %d gross %d", p.Cash, p.Trades[0].Gross)
	}
}

func TestFlipClosesAndReopens(t *testing.T) {
	p := New(0)
	p.Apply(fill(market.Buy, 65, 10000, 0))
	p.Apply(fill(market.Sell, 130, 11000, 0))
	if len(p.Trades) != 1 {
		t.Fatalf("trades %d", len(p.Trades))
	}
	pos := p.Positions()
	if len(pos) != 1 || pos[0].Qty != -65 || pos[0].OpenValue != -715000 {
		t.Fatalf("positions %+v", pos)
	}
}

func TestEquityMarksPositions(t *testing.T) {
	p := New(1000000)
	p.Apply(fill(market.Sell, 65, 10000, 0))
	got := p.Equity(func(string) (market.Paise, bool) { return 12000, true })
	if want := market.Paise(1000000 - 65*2000); got != want {
		t.Errorf("got %d want %d", got, want)
	}
	if got := p.Equity(func(string) (market.Paise, bool) { return 0, false }); got != 1000000 {
		t.Errorf("unmarked position should stay at cost, got %d", got)
	}
}
