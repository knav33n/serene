package paper

import (
	"testing"
)

func TestHMACrossesBothWays(t *testing.T) {
	// 40 flat warm bars, then a rally (cross up) and a slide (cross down).
	var today []bar
	p := 100.0
	for range 15 {
		p += 3
		today = append(today, bar{p - 3, p - 4, p})
	}
	for range 15 {
		p -= 3
		today = append(today, bar{p + 3, p - 1, p})
	}
	cfg := CrossConfig{Day: day, Fast: 5, Slow: 13, TF: 1}
	got := HMACrosses([]Leg{{Instrument: ce, Bars: legBars(today)}}, cfg)
	if len(got) < 2 || !got[0].Up || got[len(got)-1].Up {
		t.Fatalf("want an up then a down cross, got %+v", got)
	}
	for _, c := range got {
		if c.Time.Before(day.At(9, 15)) {
			t.Errorf("cross on the warm-up day at %s", c.Time)
		}
		if c.Up != (c.Fast > c.Slow) {
			t.Errorf("cross %+v: direction disagrees with the averages", c)
		}
	}
}

func TestHMACrossesFlatNone(t *testing.T) {
	if got := HMACrosses([]Leg{{Instrument: pe, Bars: legBars(flat(100, 30))}}, DefaultHMACross(day)); len(got) != 0 {
		t.Fatalf("flat bars crossed: %+v", got)
	}
}
