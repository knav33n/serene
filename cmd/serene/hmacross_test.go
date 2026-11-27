package main

import (
	"strings"
	"testing"
	"time"

	"serene/internal/kite"
	"serene/internal/market"
	"serene/internal/paper"
)

func TestHMACrossBarsDropsForming(t *testing.T) {
	d := market.Date{Year: 2026, Month: time.November, Day: 23}
	inst := market.Option(market.NFO, "NIFTY", d, market.Rupees(26000), market.Call, 65)
	var cs []kite.Candle
	for m := range 23 { // 09:15 .. 09:37, and Kite's forming 09:38
		cs = append(cs, kite.Candle{Time: d.At(9, 15).Add(time.Duration(m) * time.Minute), Open: 100, High: 101, Low: 99, Close: 100})
	}
	now := d.At(9, 38).Add(20 * time.Second)
	bars, _, err := hmaCrossBars(inst, cs, d, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != 2 || !bars[1].End.Equal(d.At(9, 35)) {
		t.Fatalf("want 09:15 and 09:25 bars only, got %d: %+v", len(bars), bars)
	}
}

func TestHMACrossTextSensex(t *testing.T) {
	d := market.Date{Year: 2026, Month: time.November, Day: 26}
	ce := market.Option(market.BFO, "SENSEX", d, market.Rupees(85000), market.Call, 20)
	c := paper.Cross{Instrument: ce, Time: d.At(9, 25), End: d.At(9, 35), Up: true}
	if s := hmaCrossText(c, paper.DefaultHMACross(d)); !strings.Contains(s, "hma_crossover* SENSEX 85000") {
		t.Errorf("text %q", s)
	}
}
