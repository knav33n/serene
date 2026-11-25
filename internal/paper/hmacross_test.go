package paper

import (
	"math"
	"testing"
	"time"

	"serene/internal/market"
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

// tens is n 10-minute bars from 09:15 today closing at closes[i] (100 past
// the slice), after 10 flat warm bars at 100 the day before.
func tens(n int, closes ...float64) []market.Bar {
	var out []market.Bar
	for i := range 10 {
		t := prevDay.At(13, 30).Add(time.Duration(i) * 10 * time.Minute)
		out = append(out, market.Bar{Timeframe: 10, Time: t, End: t.Add(10 * time.Minute), Close: market.Rupees(100)})
	}
	for i := range n {
		c := 100.0
		if i < len(closes) {
			c = closes[i]
		}
		t := day.At(9, 15).Add(time.Duration(i) * 10 * time.Minute)
		out = append(out, market.Bar{Timeframe: 10, Time: t, End: t.Add(10 * time.Minute), Close: market.Rupees(c)})
	}
	return out
}

// mins is one minute bar per low from hh:mm.
func mins(hh, mm int, lows ...float64) []market.Bar {
	var out []market.Bar
	for i, l := range lows {
		t := day.At(hh, mm).Add(time.Duration(i) * time.Minute)
		out = append(out, market.Bar{Timeframe: market.Minute1, Time: t, End: t.Add(time.Minute), Low: market.Rupees(l)})
	}
	return out
}

func crossAt(inst market.Instrument, hh, mm int, up bool) Cross {
	t := day.At(hh, mm)
	return Cross{Instrument: inst, Time: t, End: t.Add(10 * time.Minute), Up: up}
}

func TestHMANears(t *testing.T) {
	cfg := CrossConfig{Day: day, Fast: 2, Slow: 3, TF: 10, NearPct: 0.02}
	// Flat closes: both HMAs are 100. The up-cross bar is 09:25–09:35.
	up := crossAt(ce, 9, 25, true)
	ones := append(mins(9, 30, 100), mins(9, 35, 110, 105, 101.5, 100)...)
	got := HMANears([]Leg{{Instrument: ce, Bars: tens(6), Ones: ones}}, []Cross{up}, cfg)
	if len(got) != 1 || !got[0].Time.Equal(day.At(9, 37)) || got[0].Low != market.Rupees(101.5) || math.Abs(got[0].MA-100) > 1e-9 {
		t.Fatalf("want one alert at 09:37 on the 101.50 low vs 100, got %+v", got)
	}
	// A down-cross known at 09:45 ends the watch: the 09:46 dip isn't an alert.
	down := crossAt(ce, 9, 35, false)
	ones = mins(9, 35, 110, 110, 110, 110, 110, 110, 110, 110, 110, 110, 110, 100)
	if got := HMANears([]Leg{{Instrument: ce, Bars: tens(6), Ones: ones}}, []Cross{up, down}, cfg); len(got) != 0 {
		t.Errorf("alert after the next cross: %+v", got)
	}
	// Down-crosses alone and the other leg's crosses never alert.
	if got := HMANears([]Leg{{Instrument: ce, Bars: tens(6), Ones: mins(9, 35, 100)}},
		[]Cross{crossAt(ce, 9, 25, false), crossAt(pe, 9, 25, true)}, cfg); len(got) != 0 {
		t.Errorf("want none, got %+v", got)
	}
	// The averages are the last closed bar's: during 09:35–09:45, whose close
	// of 200 lifts them, a 101 low is still near 100.
	got = HMANears([]Leg{{Instrument: ce, Bars: tens(4, 100, 100, 200), Ones: mins(9, 40, 101)}}, []Cross{up}, cfg)
	if len(got) != 1 || math.Abs(got[0].MA-100) > 1e-9 {
		t.Errorf("want the 09:25 bar's HMA of 100, got %+v", got)
	}
}
