package synth

import (
	"context"
	"testing"

	"serene/internal/data"
	"serene/internal/market"
)

func TestExpiryMovesOffHoliday(t *testing.T) {
	d, _ := market.ParseDate("2026-11-18")
	c := Default(d, d, 1)
	got := c.expiriesFrom(d, 2)
	if got[0].String() != "2026-11-23" || got[1].String() != "2026-12-01" {
		t.Fatalf("got %v", got)
	}
}

func TestGenerateIsReproducibleAndWellFormed(t *testing.T) {
	ctx := context.Background()
	from, _ := market.ParseDate("2026-11-02")
	to, _ := market.ParseDate("2026-11-03")
	cfg := Default(from, to, 7)
	cfg.StrikesEachSide = 1
	run := func() []market.Bar {
		s, err := data.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, err := Generate(ctx, s, cfg); err != nil {
			t.Fatal(err)
		}
		bars, err := s.DayBars(ctx, "NIFTY", to)
		if err != nil {
			t.Fatal(err)
		}
		return bars
	}
	a, b := run(), run()
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("lengths %d %d", len(a), len(b))
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Key != y.Key || x.Close != y.Close {
			t.Fatalf("not reproducible at %d", i)
		}
		if x.High < max(x.Open, x.Close) || x.Low > min(x.Open, x.Close) {
			t.Fatalf("bad OHLC: %+v", x)
		}
	}
}
