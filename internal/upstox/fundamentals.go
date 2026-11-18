package upstox

import (
	"context"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// Company fundamentals, keyed by ISIN. Upstox serves only current ratios and
// the last four years or quarters of results, so these describe today and
// can't be backtested.

// Ratio is one key ratio with its sector's value, as Upstox formats them:
// "20.15", or "4.39%" for a percentage.
type Ratio struct {
	Name    string `json:"name"`
	Company string `json:"company_value"`
	Sector  string `json:"sector_value"`
}

// Period is one figure in a statement's history, in ₹ crore.
type Period struct {
	Value  float64 `json:"value"`
	Period string  `json:"period"` // "Mar 2026"
}

// Line is one category of a statement, newest period first.
type Line struct {
	Category string   `json:"category"` // revenue, operating_profit, net_profit
	History  []Period `json:"history"`
}

// Income is a consolidated income statement summary.
type Income struct {
	TimePeriod string `json:"time_period"`
	Units      string `json:"units_in"`
	Lines      []Line `json:"income_statement"`
}

// KeyRatios returns the company's current P/E, P/B, ROA, ROE, ROCE and others.
func (c *Client) KeyRatios(ctx context.Context, isin string) ([]Ratio, error) {
	var out []Ratio
	err := c.get(ctx, "/v2/fundamentals/"+url.PathEscape(isin)+"/key-ratios", &out)
	return out, err
}

// IncomeStatement returns the consolidated summary, yearly or quarterly.
func (c *Client) IncomeStatement(ctx context.Context, isin string, quarterly bool) (Income, error) {
	tp := "yearly"
	if quarterly {
		tp = "quarterly"
	}
	var out Income
	err := c.get(ctx, "/v2/fundamentals/"+url.PathEscape(isin)+"/income-statement?type=consolidated&time_period="+tp, &out)
	return out, err
}

// RatioValue parses a ratio as Upstox formats it, dropping a % sign. It
// returns NaN for a missing or unreadable value.
func RatioValue(rs []Ratio, name string) float64 { return ratio(rs, name, false) }

// RatioSector parses the sector's value of a ratio, as RatioValue does.
func RatioSector(rs []Ratio, name string) float64 { return ratio(rs, name, true) }

func ratio(rs []Ratio, name string, sector bool) float64 {
	for _, r := range rs {
		if strings.EqualFold(r.Name, name) {
			s := r.Company
			if sector {
				s = r.Sector
			}
			v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64)
			if err != nil {
				return math.NaN()
			}
			return v
		}
	}
	return math.NaN()
}

// History returns a category's figures, newest first, or nil.
func (in Income) History(category string) []Period {
	for _, l := range in.Lines {
		if l.Category == category {
			return l.History
		}
	}
	return nil
}

// Balance is a consolidated balance sheet. Full holds the annual top-level
// rows ("Non-Current Assets", "Total Assets", …), newest period first; Upstox
// has no finer lines such as gross fixed assets.
type Balance struct {
	Units string `json:"units_in"`
	Full  []struct {
		Particular string   `json:"particular"`
		History    []Period `json:"history"`
	} `json:"full_statement"`
}

// BalanceSheet returns the consolidated balance sheet with its full statement.
func (c *Client) BalanceSheet(ctx context.Context, isin string) (Balance, error) {
	var out Balance
	err := c.get(ctx, "/v2/fundamentals/"+url.PathEscape(isin)+"/balance-sheet?type=consolidated&fs=true", &out)
	return out, err
}

// Row returns a full-statement row's figures, newest first, or nil.
func (b Balance) Row(particular string) []Period {
	for _, r := range b.Full {
		if strings.EqualFold(r.Particular, particular) {
			return r.History
		}
	}
	return nil
}
