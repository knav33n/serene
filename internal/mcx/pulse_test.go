package mcx

import (
	"testing"
	"time"

	"serene/internal/market"
)

func minute(t time.Time, o, h, l, c float64) market.Bar {
	return market.Bar{Time: t, End: t.Add(time.Minute), Open: market.Rupees(o), High: market.Rupees(h),
		Low: market.Rupees(l), Close: market.Rupees(c)}
}

func TestCleanStaleOpens(t *testing.T) {
	d1, d2 := market.Date{Year: 2026, Month: 11, Day: 9}, market.Date{Year: 2026, Month: 11, Day: 10}
	bars := []market.Bar{
		minute(d1.At(9, 0), 8574, 8580, 8570, 8575),
		minute(d1.At(9, 1), 8575, 8576, 8574, 8575),
		minute(d1.At(23, 54), 8224, 8225, 8223, 8224),
		// Evening-only session: Kite repeats the 09:00 open of 8574.
		minute(d2.At(17, 0), 8574, 8574, 8033, 8048),
		minute(d2.At(17, 1), 8045, 8049, 8042, 8044),
	}
	got := CleanStaleOpens(bars)
	b := got[3]
	if b.Open != market.Rupees(8048) || b.High != market.Rupees(8048) || b.Low != market.Rupees(8033) {
		t.Fatalf("cleaned bar %+v", b)
	}
	if bars[3].Open != market.Rupees(8574) {
		t.Error("the input was changed")
	}
	if got[0] != bars[0] || got[4] != bars[4] {
		t.Error("other bars changed")
	}
}
