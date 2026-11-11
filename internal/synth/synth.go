// Package synth generates a synthetic index and option chain. The index is a
// driftless random walk and options are priced fairly from it, so no strategy
// has a real edge here. That makes it a check on the system: after costs, a
// strategy should lose about what it paid, and a big profit means a bug.
package synth

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"serene/internal/data"
	"serene/internal/market"
	"serene/internal/options"
)

// Config describes a dataset.
type Config struct {
	Underlying      string
	From, To        market.Date
	Seed            uint64
	Spot            float64 // starting index level
	Vol             float64 // annualised, both realised and implied
	LotSize         int64
	StrikeStep      float64
	StrikesEachSide int
	Expiries        int     // listed weekly expiries
	MissingProb     float64 // chance a contract has no candle in a minute
	Holidays        map[market.Date]bool
}

// Default returns a NIFTY-like dataset. The market holiday on Tuesday
// 2026-11-24 falls in the default test range, which moves that expiry.
func Default(from, to market.Date, seed uint64) Config {
	h, _ := market.ParseDate("2026-11-24")
	return Config{Underlying: "NIFTY", From: from, To: to, Seed: seed, Spot: 24000, Vol: 0.15,
		LotSize: 65, StrikeStep: 50, StrikesEachSide: 12, Expiries: 2, MissingProb: 0.02,
		Holidays: map[market.Date]bool{h: true}}
}

func (c Config) tradingDay(d market.Date) bool {
	w := d.Weekday()
	return w != time.Saturday && w != time.Sunday && !c.Holidays[d]
}

// expiriesFrom returns the next n weekly expiries on or after d. Expiry is
// Tuesday, or the previous trading day if that is a holiday.
func (c Config) expiriesFrom(d market.Date, n int) []market.Date {
	var out []market.Date
	for t := d.AddDays(-7); len(out) < n; t = t.AddDays(1) {
		if t.Weekday() != time.Tuesday {
			continue
		}
		e := t
		for !c.tradingDay(e) {
			e = e.AddDays(-1)
		}
		if !e.Before(d) {
			out = append(out, e)
		}
	}
	return out
}

// Generate writes the dataset to the store and returns the trading days made.
func Generate(ctx context.Context, s *data.Store, c Config) (int, error) {
	rng := rand.New(rand.NewPCG(c.Seed, 0x63616c6d))
	spot := c.Spot
	idx := market.Index(market.NSE, c.Underlying)
	seen := map[string]market.Instrument{}
	session := market.NSESession
	const substeps = 4
	yearMinutes := 365.0 * 24 * 60
	days := 0
	var prevClose time.Time

	step := func(minutes float64) {
		// Driftless in price: the -0.5 sigma^2 term keeps it a martingale.
		sd := c.Vol * math.Sqrt(minutes/yearMinutes)
		spot *= math.Exp(-0.5*sd*sd + sd*rng.NormFloat64())
	}

	for d := c.From; !c.To.Before(d); d = d.AddDays(1) {
		if !c.tradingDay(d) {
			continue
		}
		open := session.Open(d)
		if !prevClose.IsZero() {
			step(open.Sub(prevClose).Minutes())
		}
		var contracts []market.Instrument
		center := math.Round(spot/c.StrikeStep) * c.StrikeStep
		for _, exp := range c.expiriesFrom(d, c.Expiries) {
			for k := -c.StrikesEachSide; k <= c.StrikesEachSide; k++ {
				strike := market.Rupees(center + float64(k)*c.StrikeStep)
				for _, r := range []market.Right{market.Call, market.Put} {
					contracts = append(contracts, market.Option(market.NFO, c.Underlying, exp, strike, r, c.LotSize))
				}
			}
		}
		minutes := int(session.Close(d).Sub(open).Minutes())
		bars := make([]market.Bar, 0, minutes*(len(contracts)+1))
		for m := range minutes {
			t := open.Add(time.Duration(m) * time.Minute)
			o := spot
			hi, lo := spot, spot
			for range substeps {
				step(1.0 / substeps)
				hi, lo = math.Max(hi, spot), math.Min(lo, spot)
			}
			cl := spot
			bars = append(bars, market.Bar{Key: idx.Key(), Timeframe: market.Minute1, Time: t, End: t.Add(time.Minute),
				Open: market.Rupees(o), High: market.Rupees(hi), Low: market.Rupees(lo), Close: market.Rupees(cl)})
			for _, ct := range contracts {
				if rng.Float64() < c.MissingProb {
					continue
				}
				po := fair(ct, o, t, c.Vol)
				pc := fair(ct, cl, t.Add(time.Minute), c.Vol)
				pa, pb := fair(ct, hi, t, c.Vol), fair(ct, lo, t, c.Vol)
				bars = append(bars, market.Bar{Key: ct.Key(), Timeframe: market.Minute1, Time: t, End: t.Add(time.Minute),
					Open: po, Close: pc, High: max(po, pc, pa, pb), Low: min(po, pc, pa, pb),
					Volume: 1000 + rng.Int64N(50000), OI: 100000})
			}
		}
		prevClose = session.Close(d)
		if err := s.PutBars(ctx, c.Underlying, bars); err != nil {
			return days, err
		}
		for _, ct := range contracts {
			seen[ct.Key()] = ct
		}
		days++
	}
	if days == 0 {
		return 0, fmt.Errorf("no trading days between %s and %s", c.From, c.To)
	}
	all := make([]market.Instrument, 0, len(seen))
	for _, ct := range seen {
		all = append(all, ct)
	}
	return days, s.PutContracts(ctx, c.Underlying, all)
}

// fair prices a contract and rounds to the 5 paise tick, never below one tick.
func fair(ct market.Instrument, spot float64, t time.Time, vol float64) market.Paise {
	in := options.Input{Forward: spot, Strike: ct.Strike.Rupees(), Call: ct.Right == market.Call,
		Years: options.YearsToExpiry(t, ct.Expiry)}
	p := market.Rupees(options.Price(in, vol))
	return max((p+2)/5*5, 5)
}
