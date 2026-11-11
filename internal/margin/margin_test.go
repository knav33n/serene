package margin

import (
	"testing"

	"serene/internal/market"
)

func opt(strike float64, r market.Right, expiry string) market.Instrument {
	d, _ := market.ParseDate(expiry)
	return market.Option(market.NFO, "NIFTY", d, market.Rupees(strike), r, 65)
}

func TestRequired(t *testing.T) {
	spot := func(string) (market.Paise, bool) { return market.Rupees(24000), true }
	m := Model{NakedPct: 10} // Rs 2,400 per unit naked
	const exp, later = "2026-11-09", "2026-11-17"
	cases := []struct {
		name string
		hs   []Holding
		want market.Paise
	}{
		{"long only", []Holding{{opt(24000, market.Call, exp), 65}}, 0},
		{"naked short", []Holding{{opt(24000, market.Call, exp), -65}}, market.Rupees(2400 * 65)},
		{"call credit spread", []Holding{{opt(24000, market.Call, exp), -65}, {opt(24100, market.Call, exp), 65}}, market.Rupees(100 * 65)},
		{"put credit spread", []Holding{{opt(24000, market.Put, exp), -65}, {opt(23900, market.Put, exp), 65}}, market.Rupees(100 * 65)},
		{"debit spread", []Holding{{opt(24000, market.Call, exp), 65}, {opt(24100, market.Call, exp), -65}}, 0},
		{"partly hedged", []Holding{{opt(24000, market.Call, exp), -130}, {opt(24100, market.Call, exp), 65}}, market.Rupees(100*65 + 2400*65)},
		{"other expiry does not hedge", []Holding{{opt(24000, market.Call, exp), -65}, {opt(24100, market.Call, later), 65}}, market.Rupees(2400 * 65)},
		{"other right does not hedge", []Holding{{opt(24000, market.Call, exp), -65}, {opt(24100, market.Put, exp), 65}}, market.Rupees(2400 * 65)},
		{"wide spread capped at naked", []Holding{{opt(24000, market.Call, exp), -65}, {opt(27000, market.Call, exp), 65}}, market.Rupees(2400 * 65)},
	}
	for _, c := range cases {
		got, err := m.Required(c.hs, spot)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}
