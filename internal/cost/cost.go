// Package cost computes trading charges from an effective-dated schedule, so
// a historical trade pays the rates that applied on its date.
package cost

import (
	"fmt"
	"math"
	"os"
	"sort"

	"gopkg.in/yaml.v3"

	"calm/internal/market"
)

// Rates is one period's charges. Percentages are as quoted, so 0.15 is 0.15%.
type Rates struct {
	From           string             `yaml:"from"`
	BrokeragePer   float64            `yaml:"brokerage_per_order"` // rupees
	STTSellPct     float64            `yaml:"stt_sell_pct"`
	STTExercisePct float64            `yaml:"stt_exercise_pct"`
	ExchangePct    map[string]float64 `yaml:"exchange_pct"`
	SEBIPct        float64            `yaml:"sebi_pct"`
	StampBuyPct    float64            `yaml:"stamp_buy_pct"`
	GSTPct         float64            `yaml:"gst_pct"`

	from market.Date
}

// Schedule is a list of rate periods.
type Schedule struct {
	periods []Rates
	zero    bool
}

// Zero returns a schedule that charges nothing, for gross analysis and tests.
func Zero() *Schedule { return &Schedule{zero: true} }

// Parse reads a schedule from YAML.
func Parse(b []byte) (*Schedule, error) {
	var doc struct {
		Periods []Rates `yaml:"periods"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse costs: %w", err)
	}
	if len(doc.Periods) == 0 {
		return nil, fmt.Errorf("parse costs: no periods")
	}
	for i := range doc.Periods {
		d, err := market.ParseDate(doc.Periods[i].From)
		if err != nil {
			return nil, err
		}
		doc.Periods[i].from = d
	}
	sort.Slice(doc.Periods, func(i, j int) bool { return doc.Periods[i].from.Before(doc.Periods[j].from) })
	return &Schedule{periods: doc.Periods}, nil
}

// Load reads a schedule file.
func Load(path string) (*Schedule, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load costs: %w", err)
	}
	return Parse(b)
}

func (s *Schedule) ratesOn(d market.Date) (Rates, error) {
	for i := len(s.periods) - 1; i >= 0; i-- {
		if !d.Before(s.periods[i].from) {
			return s.periods[i], nil
		}
	}
	return Rates{}, fmt.Errorf("no cost rates for %s: add an earlier period to the schedule", d)
}

func pct(amount market.Paise, rate float64) market.Paise {
	return market.Paise(math.Round(float64(amount) * rate / 100))
}

// Charges returns the total charges for a fill.
func (s *Schedule) Charges(f market.Fill) (market.Paise, error) {
	if s.zero {
		return 0, nil
	}
	r, err := s.ratesOn(market.DateOf(f.Time))
	if err != nil {
		return 0, err
	}
	turnover := f.Price * market.Paise(f.Qty)
	if f.Kind == market.Settled {
		// Cash settlement has no order, so no brokerage or exchange fee. The
		// long side of an exercised option pays STT on intrinsic value.
		if f.Side == market.Sell && f.Price > 0 {
			return pct(turnover, r.STTExercisePct), nil
		}
		return 0, nil
	}
	exch, ok := r.ExchangePct[string(f.Instrument.Exchange)]
	if !ok {
		return 0, fmt.Errorf("no exchange charge for %s in the period from %s", f.Instrument.Exchange, r.From)
	}
	brokerage := market.Rupees(r.BrokeragePer)
	exchange := pct(turnover, exch)
	sebi := pct(turnover, r.SEBIPct)
	gst := pct(brokerage+exchange+sebi, r.GSTPct)
	total := brokerage + exchange + sebi + gst
	if f.Side == market.Sell {
		total += pct(turnover, r.STTSellPct)
	} else {
		total += pct(turnover, r.StampBuyPct)
	}
	return total, nil
}
