package strategies

import (
	"testing"

	"calm/internal/market"
	"calm/internal/strategy"
)

func TestTradingDaysTo(t *testing.T) {
	d := func(s string) market.Date {
		x, err := market.ParseDate(s)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	cases := []struct {
		day, exp string
		want     int
	}{
		{"2026-11-17", "2026-11-17", 0}, // Tuesday expiry day
		{"2026-11-16", "2026-11-17", 1}, // Monday
		{"2026-11-13", "2026-11-17", 2}, // Friday, over the weekend
		{"2026-11-06", "2026-11-09", 1}, // Friday before a Monday expiry (2026-11-10 is a holiday)
		{"2025-08-26", "2025-08-28", 1}, // 2025-08-27 was a holiday
		{"2026-01-23", "2026-01-27", 1}, // over Republic Day Monday
	}
	for _, c := range cases {
		if got, err := tradingDaysTo(d(c.day), d(c.exp)); err != nil || got != c.want {
			t.Errorf("%s to %s: %d, want %d", c.day, c.exp, got, c.want)
		}
	}
}

func TestTradingDaysToPastHolidayList(t *testing.T) {
	if _, err := tradingDaysTo(market.Date{Year: 2026, Month: 12, Day: 31}, market.Date{Year: 2027, Month: 1, Day: 5}); err == nil {
		t.Error("an expiry past the holiday list should be an error")
	}
}

func TestEMACrossFirstOneTradeADay(t *testing.T) {
	store, from, to := lhbStore(t)
	for _, dte := range []float64{0, 1} {
		r, err := lhbRunWithStrategy(t, store, from, to, "ema_cross_first", strategy.Params{"dte": dte, "strike_step": 100})
		if err != nil {
			t.Fatal(err)
		}
		buys := map[market.Date]int{}
		open := map[string]market.Fill{}
		for _, f := range r.Fills {
			key := f.Instrument.Key()
			switch f.Side {
			case market.Buy:
				buys[market.DateOf(f.Time)]++
				open[key] = f
			case market.Sell:
				in, ok := open[key]
				if !ok {
					t.Fatalf("sold %s without a position", key)
				}
				delete(open, key)
				if market.DateOf(in.Time) != market.DateOf(f.Time) {
					t.Errorf("%s held overnight", key)
				}
			}
			if n, _ := tradingDaysTo(market.DateOf(f.Time), f.Instrument.Expiry); f.Side == market.Buy && n != int(dte) {
				t.Errorf("dte %v: bought %s on %s", dte, key, market.DateOf(f.Time))
			}
		}
		for day, n := range buys {
			if n > 1 {
				t.Errorf("dte %v: %d entries on %s", dte, n, day)
			}
		}
		t.Logf("dte %v: %d entries", dte, len(buys))
		if len(buys) == 0 {
			t.Errorf("dte %v: no trades in the synthetic data", dte)
		}
		if len(open) != 0 {
			t.Errorf("positions left open: %v", open)
		}
	}
}
