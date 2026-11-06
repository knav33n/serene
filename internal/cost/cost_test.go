package cost

import (
	"testing"

	"calm/internal/market"
)

const sample = `
periods:
  - from: "2026-04-01"
    brokerage_per_order: 20
    stt_sell_pct: 0.15
    stt_exercise_pct: 0.15
    exchange_pct: {NFO: 0.03553}
    sebi_pct: 0.0001
    stamp_buy_pct: 0.003
    gst_pct: 18
  - from: "2024-10-01"
    brokerage_per_order: 20
    stt_sell_pct: 0.1
    stt_exercise_pct: 0.125
    exchange_pct: {NFO: 0.03553}
    sebi_pct: 0.0001
    stamp_buy_pct: 0.003
    gst_pct: 18
`

func fill(date string, side market.Side, kind market.FillKind, price market.Paise) market.Fill {
	d, _ := market.ParseDate(date)
	return market.Fill{Instrument: market.Option(market.NFO, "NIFTY", d, market.Rupees(24000), market.Call, 65),
		Side: side, Qty: 65, Price: price, Time: d.At(10, 0), Kind: kind}
}

// Turnover is 65 x Rs 100 = Rs 6,500 in every case below.
func TestChargesByHand(t *testing.T) {
	s, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		f    market.Fill
		want market.Paise
	}{
		// Brokerage 2000p; exchange 650000*0.0003553=230.945 -> 231p;
		// SEBI 0.65 -> 1p; GST 18% of 2232 = 401.76 -> 402p;
		// STT 0.15% = 975p. Total 2000+231+1+402+975.
		{"sell after 2026-04", fill("2026-11-06", market.Sell, market.Traded, 10000), 3609},
		// Same, but stamp 0.003% = 19.5 -> 20p instead of STT.
		{"buy", fill("2026-11-06", market.Buy, market.Traded, 10000), 2654},
		// STT at the older 0.1% = 650p.
		{"sell before 2026-04", fill("2026-03-31", market.Sell, market.Traded, 10000), 3284},
		// STT on intrinsic only: 0.15% of 650000p.
		{"long settled ITM", fill("2026-11-06", market.Sell, market.Settled, 10000), 975},
		{"long settled OTM", fill("2026-11-06", market.Sell, market.Settled, 0), 0},
		{"short settled ITM", fill("2026-11-06", market.Buy, market.Settled, 10000), 0},
	}
	for _, c := range cases {
		got, err := s.Charges(c.f)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}

func TestChargesFailOutsideSchedule(t *testing.T) {
	s, _ := Parse([]byte(sample))
	if _, err := s.Charges(fill("2024-09-30", market.Sell, market.Traded, 10000)); err == nil {
		t.Error("date before the schedule accepted")
	}
	f := fill("2026-11-06", market.Sell, market.Traded, 10000)
	f.Instrument.Exchange = market.BFO
	if _, err := s.Charges(f); err == nil {
		t.Error("exchange missing from the schedule accepted")
	}
}
