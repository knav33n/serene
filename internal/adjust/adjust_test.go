package adjust

import (
	"math"
	"testing"

	"calm/internal/market"
)

func TestSnap(t *testing.T) {
	cases := []struct {
		r        float64
		num, den int
		ok       bool
	}{
		{0.5, 1, 2, true},     // 1:1 bonus
		{0.2001, 1, 5, true},  // ₹10 to ₹2 split, measured with rounding
		{2.0 / 3, 2, 3, true}, // 1:2 bonus
		{0.0667, 1, 15, true}, // ITC 2005: ₹10 to ₹1 and a 1:2 bonus
		{0.7519, 3, 4, true},  // Wipro's 1:3 bonus in 2019, as measured
		{0.9532, 0, 0, false}, // Reliance's 2023 demerger
		{0.77, 0, 0, false},   // Reliance's 2006 demerger
		{0.8788, 0, 0, false}, // Wipro's 2013 demerger, 0.4% from a 1:7 bonus
	}
	for _, c := range cases {
		n, d, ok := Snap(c.r)
		if n != c.num || d != c.den || ok != c.ok {
			t.Errorf("Snap(%v) = %d/%d %v, want %d/%d %v", c.r, n, d, ok, c.num, c.den, c.ok)
		}
	}
}

// series builds n trading days of bhavcopy prices around base, and Upstox
// prices for the same days divided by the factor in force on each.
func series(n int, base float64, events map[int]float64) (up, bh []Day) {
	d := market.Date{Year: 2024, Month: 1, Day: 1}
	for i := range n {
		p := base * (1 + 0.01*math.Sin(float64(i)))
		for at, r := range events {
			if i >= at {
				p *= r // the unadjusted price falls on the ex-date
			}
		}
		adj := 1.0
		for at, r := range events {
			if i < at {
				adj *= r
			}
		}
		bh = append(bh, Day{d, p, p * 1.01, p * 0.99, p})
		up = append(up, Day{d, p * adj, p * 1.01 * adj, p * 0.99 * adj, p * adj})
		d = d.AddDays(1)
	}
	return up, bh
}

func TestDetect(t *testing.T) {
	up, bh := series(60, 2600, map[int]float64{20: 0.5, 40: 0.2})
	ev := Detect(up, bh)
	if len(ev) != 2 {
		t.Fatalf("got %d events: %+v", len(ev), ev)
	}
	if ev[0].ExDate != bh[20].Date || ev[0].Num != 1 || ev[0].Den != 2 || ev[0].Kind != KindSplitBonus {
		t.Errorf("first: %+v", ev[0])
	}
	if ev[1].ExDate != bh[40].Date || ev[1].Num != 1 || ev[1].Den != 5 {
		t.Errorf("second: %+v", ev[1])
	}
}

func TestDetectIgnoresOneBadDay(t *testing.T) {
	up, bh := series(30, 500, nil)
	up[10].Open, up[10].High, up[10].Low, up[10].Close = up[10].Open*0.7, up[10].High*0.7, up[10].Low*0.7, up[10].Close*0.7
	// A differing close alone, as on a special session, moves no median.
	up[15].Close *= 1.05
	if ev := Detect(up, bh); len(ev) != 0 {
		t.Errorf("got %+v", ev)
	}
}

func TestDetectDropsSmallOther(t *testing.T) {
	up, bh := series(40, 600, map[int]float64{20: 1.0285})
	if ev := Detect(up, bh); len(ev) != 0 {
		t.Errorf("an upward step was kept: %+v", ev)
	}
	up, bh = series(40, 600, map[int]float64{20: 0.9532})
	if ev := Detect(up, bh); len(ev) != 1 || ev[0].Kind != KindOther {
		t.Errorf("a demerger: %+v", ev)
	}
}

func TestDetectNoUpstoxHistory(t *testing.T) {
	_, bh := series(10, 100, nil)
	if ev := Detect(nil, bh); ev != nil {
		t.Errorf("got %+v", ev)
	}
}

func TestSpans(t *testing.T) {
	a := market.Date{Year: 2020, Month: 5, Day: 4}
	b := market.Date{Year: 2024, Month: 10, Day: 28}
	c := market.Date{Year: 2023, Month: 7, Day: 20}
	sp := Spans([]Event{{ExDate: b, Ratio: 0.5, Kind: KindSplitBonus}, {ExDate: a, Ratio: 0.2, Kind: KindSplitBonus},
		{ExDate: c, Ratio: 0.9, Kind: KindOther}})
	if len(sp) != 4 {
		t.Fatalf("%+v", sp)
	}
	near := func(x, y float64) bool { return math.Abs(x-y) < 1e-12 }
	if sp[0].To != a.AddDays(-1) || !near(sp[0].Factor, 0.1) || !near(sp[0].FactorAll, 0.09) ||
		sp[1].From != a || sp[1].To != c.AddDays(-1) || !near(sp[1].Factor, 0.5) || !near(sp[1].FactorAll, 0.45) ||
		sp[2].From != c || !near(sp[2].Factor, 0.5) || !near(sp[2].FactorAll, 0.5) ||
		sp[3].From != b || sp[3].Factor != 1 || sp[3].FactorAll != 1 {
		t.Errorf("%+v", sp)
	}
}
