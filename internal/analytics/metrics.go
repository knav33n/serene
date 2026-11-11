// Package analytics measures runs and tests whether results are robust.
package analytics

import (
	"math"
	"sort"

	"serene/internal/engine"
	"serene/internal/market"
)

// Metrics summarises a run. Trade statistics group legs by tag, so a
// multi-leg structure counts as one trade.
type Metrics struct {
	Net            market.Paise
	Gross          market.Paise
	Charges        market.Paise
	ReturnPct      float64
	CAGRPct        float64
	Sharpe         float64 // annualised from daily returns, zero risk-free rate
	Sortino        float64
	MaxDrawdown    market.Paise
	MaxDrawdownPct float64 // of starting capital
	Days           int
	Trades         int
	WinRatePct     float64
	ProfitFactor   float64 // +Inf when nothing lost
	AvgWin         market.Paise
	AvgLoss        market.Paise
	Expectancy     market.Paise
	SettledLegs    int
	Rejected       int
	Unfilled       int
}

// Measure computes metrics from a result.
func Measure(r engine.Result, unfilled int) Metrics {
	m := Metrics{Charges: r.Charges, Rejected: r.Rejected, Unfilled: unfilled,
		MaxDrawdown: r.MaxDrawdown, Days: len(r.Equity)}
	capital := r.Capital.Rupees()
	if capital > 0 {
		m.MaxDrawdownPct = 100 * r.MaxDrawdown.Rupees() / capital
	}
	if len(r.Equity) > 0 {
		end := r.Equity[len(r.Equity)-1].Equity
		m.Net = end - r.Capital
		m.Gross = m.Net + r.Charges
		if capital > 0 {
			m.ReturnPct = 100 * m.Net.Rupees() / capital
			if growth := end.Rupees() / capital; growth > 0 {
				m.CAGRPct = 100 * (math.Pow(growth, 252/float64(len(r.Equity))) - 1)
			} else {
				m.CAGRPct = -100
			}
		}
	}
	m.Sharpe, m.Sortino = riskRatios(r)

	byTag := map[string]market.Paise{}
	for _, t := range r.Trades {
		byTag[t.Tag] += t.Net()
		if t.Settled {
			m.SettledLegs++
		}
	}
	var won, lost market.Paise
	wins := 0
	for _, net := range byTag {
		if net > 0 {
			wins++
			won += net
		} else {
			lost -= net
		}
	}
	m.Trades = len(byTag)
	if m.Trades > 0 {
		m.WinRatePct = 100 * float64(wins) / float64(m.Trades)
		m.Expectancy = (won - lost) / market.Paise(m.Trades)
		if wins > 0 {
			m.AvgWin = won / market.Paise(wins)
		}
		if m.Trades > wins {
			m.AvgLoss = lost / market.Paise(m.Trades-wins)
		}
	}
	switch {
	case lost > 0:
		m.ProfitFactor = won.Rupees() / lost.Rupees()
	case won > 0:
		m.ProfitFactor = math.Inf(1)
	}
	return m
}

func dailyReturns(r engine.Result) []float64 {
	prev := r.Capital.Rupees()
	out := make([]float64, 0, len(r.Equity))
	for _, p := range r.Equity {
		eq := p.Equity.Rupees()
		if prev > 0 {
			out = append(out, eq/prev-1)
		}
		prev = eq
	}
	return out
}

func riskRatios(r engine.Result) (sharpe, sortino float64) {
	rets := dailyReturns(r)
	if len(rets) < 2 {
		return 0, 0
	}
	var sum float64
	for _, v := range rets {
		sum += v
	}
	mean := sum / float64(len(rets))
	var sq, down float64
	for _, v := range rets {
		sq += (v - mean) * (v - mean)
		if v < 0 {
			down += v * v
		}
	}
	annual := math.Sqrt(252)
	if sd := math.Sqrt(sq / float64(len(rets)-1)); sd > 0 {
		sharpe = annual * mean / sd
	}
	if dd := math.Sqrt(down / float64(len(rets))); dd > 0 {
		sortino = annual * mean / dd
	}
	return sharpe, sortino
}

// Score reads a named metric, for ranking sweeps. Higher is better.
func (m Metrics) Score(name string) float64 {
	switch name {
	case "net":
		return m.Net.Rupees()
	case "sortino":
		return m.Sortino
	case "calmar":
		if m.MaxDrawdownPct == 0 {
			return 0
		}
		return m.CAGRPct / m.MaxDrawdownPct
	default:
		return m.Sharpe
	}
}

// sortedKeys returns a map's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
