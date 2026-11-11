package dow

import (
	"testing"
	"time"

	"serene/internal/market"
)

// bars builds 15-minute bars from (high, low) rupee pairs.
func bars(hl ...[2]float64) []market.Bar {
	t0 := time.Date(2026, 11, 9, 9, 0, 0, 0, market.IST)
	out := make([]market.Bar, len(hl))
	for i, v := range hl {
		t := t0.Add(time.Duration(i) * 15 * time.Minute)
		out[i] = market.Bar{Time: t, End: t.Add(15 * time.Minute), High: market.Rupees(v[0]), Low: market.Rupees(v[1]),
			Open: market.Rupees(v[1]), Close: market.Rupees(v[0])}
	}
	return out
}

func TestSwingsLabelsAndTrend(t *testing.T) {
	// Up to 110, down to 104, up to 115, down to 108, up to 112.
	bs := bars([2]float64{101, 100}, [2]float64{110, 105}, [2]float64{106, 104}, [2]float64{115, 108},
		[2]float64{111, 108}, [2]float64{112, 109})
	sw, run := Swings(bs, market.Rupees(4))
	want := []struct {
		label Label
		price float64
		conf  int // index of the confirming bar
	}{{L, 100, 1}, {H, 110, 2}, {HL, 104, 3}, {HH, 115, 4}, {HL, 108, 5}}
	if len(sw) != len(want) {
		t.Fatalf("swings %+v", sw)
	}
	for i, w := range want {
		if sw[i].Label != w.label || sw[i].Price != market.Rupees(w.price) || !sw[i].Confirmed.Equal(bs[w.conf].End) {
			t.Errorf("swing %d: %+v, want %+v", i, sw[i], w)
		}
	}
	if !run.High || run.Price != market.Rupees(112) {
		t.Errorf("running %+v", run)
	}
	if got := TrendOf(sw); got != Up {
		t.Errorf("trend %s", got)
	}
	// Only swings confirmed by the end of bar 2 are known then.
	if got := At(sw, bs[2].End); len(got) != 2 {
		t.Errorf("at bar 2: %d swings", len(got))
	}
}

func TestBreaksAreCausal(t *testing.T) {
	// Falling: H 110, L 100, LH 106, LL 98 confirmed; then a rally through 106.
	bs := bars([2]float64{110, 107}, [2]float64{106, 100}, [2]float64{106, 102}, [2]float64{103, 98},
		[2]float64{103, 99}, [2]float64{107, 101})
	sw, _ := Swings(bs, market.Rupees(4))
	if got := TrendOf(At(sw, bs[4].End)); got != Down {
		t.Fatalf("trend before the rally: %s (%+v)", got, sw)
	}
	br := Breaks(bs, sw, bs[5].Time)
	if len(br) != 1 || !br[0].Up || br[0].Level != market.Rupees(106) || !br[0].ChangeOfCharacter {
		t.Fatalf("breaks %+v", br)
	}
	// Bar 3 breaks the low of 100, which bar 2 confirmed; with only H and L
	// known there is no trend yet, so it is not a change of character.
	br = Breaks(bs, sw, bs[3].Time)
	if len(br) != 2 || br[0].Up || br[0].Level != market.Rupees(100) || !br[0].Time.Equal(bs[3].Time) || br[0].ChangeOfCharacter {
		t.Errorf("breaks from bar 3: %+v", br)
	}
}

func TestBreakReportedOnce(t *testing.T) {
	// L 100 confirmed by bar 1; bar 2 breaks it; bar 3 goes lower without a
	// new swing low confirmed in between.
	bs := bars([2]float64{100, 100}, [2]float64{105, 101}, [2]float64{104, 99}, [2]float64{103, 98})
	sw, _ := Swings(bs, market.Rupees(4))
	if br := Breaks(bs, sw, bs[3].Time); len(br) != 0 {
		t.Fatalf("the low was broken by bar 2, yet: %+v", br)
	}
}

func TestWithRunning(t *testing.T) {
	sw := []Swing{{High: true, Price: 110, Label: H}, {Price: 100, Label: L}, {High: true, Price: 106, Label: LH}}
	got := WithRunning(sw, Swing{Price: 98})
	if len(got) != 4 || got[3].Label != LL || TrendOf(got) != Down {
		t.Fatalf("running below the low: %+v", got)
	}
	if got := WithRunning(sw, Swing{Price: 101}); len(got) != 3 {
		t.Fatalf("a leg above the last low is not a swing yet: %+v", got)
	}
	if len(sw) != 3 {
		t.Fatal("input changed")
	}
}
