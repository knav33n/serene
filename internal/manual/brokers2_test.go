package manual

import (
	"testing"

	"serene/internal/breeze"
	"serene/internal/market"
)

func TestICICISymbolRoundTrip(t *testing.T) {
	c := breeze.Contract{Stock: "NIFTY", Expiry: market.Date{Year: 2026, Month: 11, Day: 24}, Strike: 25400, Right: "CE"}
	s := ICICISymbol(c)
	if s != "NIFTY|2026-11-24|25400|CE" {
		t.Fatal(s)
	}
	back, err := ParseICICISymbol(s)
	if err != nil || back != c {
		t.Fatal(back, err)
	}
}

func TestMarketable(t *testing.T) {
	for _, tc := range []struct {
		side      string
		ltp, want float64
	}{{"BUY", 100, 105}, {"BUY", 101.3, 106.4}, {"SELL", 100, 95}, {"SELL", 0.05, 0.05}, {"SELL", 3.33, 3.15}} {
		if got := Marketable(tc.side, tc.ltp, 0.05); got != tc.want {
			t.Errorf("%s %g: %g, want %g", tc.side, tc.ltp, got, tc.want)
		}
	}
}
