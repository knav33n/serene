package main

import (
	"testing"
	"time"

	"serene/internal/market"
)

func divDayBars(ohlc ...[4]float64) []market.Bar {
	var out []market.Bar
	t := time.Date(2020, 1, 1, 9, 15, 0, 0, market.IST)
	for i, b := range ohlc {
		d := t.AddDate(0, 0, i)
		out = append(out, market.Bar{Time: d, End: d, Open: market.Rupees(b[0]), High: market.Rupees(b[1]),
			Low: market.Rupees(b[2]), Close: market.Rupees(b[3])})
	}
	return out
}

func TestSimulateExits(t *testing.T) {
	r := divRule{targetR: 2, maxHold: 3}
	// Signal at 0; buy at 100 with the stop at 95, so the target is 110.
	bars := divDayBars([4]float64{99, 100, 98, 99}, [4]float64{100, 104, 99, 103}, [4]float64{103, 111, 102, 110})
	tr := simulate("X", bars, []int{0}, []float64{95}, r)
	if len(tr) != 1 || tr[0].why != "target" || tr[0].out != 110 || tr[0].days != 2 {
		t.Fatalf("target: %+v", tr)
	}
	// A gap below the stop fills at the open.
	bars = divDayBars([4]float64{99, 100, 98, 99}, [4]float64{100, 101, 99, 100}, [4]float64{90, 92, 89, 91})
	tr = simulate("X", bars, []int{0}, []float64{95}, r)
	if len(tr) != 1 || tr[0].why != "stop" || tr[0].out != 90 {
		t.Fatalf("gap stop: %+v", tr)
	}
	// Neither level: out at the close after maxHold days.
	bars = divDayBars([4]float64{99, 100, 98, 99}, [4]float64{100, 101, 99, 100}, [4]float64{100, 101, 99, 100}, [4]float64{100, 102, 99, 101}, [4]float64{101, 102, 100, 101})
	tr = simulate("X", bars, []int{0}, []float64{95}, r)
	if len(tr) != 1 || tr[0].why != "time" || tr[0].out != 101 || tr[0].days != 3 {
		t.Fatalf("time: %+v", tr)
	}
}
