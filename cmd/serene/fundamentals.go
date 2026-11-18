package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"serene/internal/upstox"
)

// qualityMin is the video's example screen: ROE and ROCE at least 20%.
const qualityMin = 20.0

// fundamentals is a stock's current figures from Upstox. Growth is percent,
// latest period over the one before; NaN where Upstox has no figure.
type fundamentals struct {
	ok                  bool
	pe, pb, roe, roce   float64
	revGrowth, npGrowth float64 // latest year over the previous one
	revCAGR3            float64 // over the last three years
	lastQ               string  // latest quarter, e.g. "Sep 2026"
	qNPChange           float64 // that quarter's net profit over the one before
	quality             bool    // ROE and ROCE at least qualityMin
	roeSector           float64 // the sector's values, as Upstox gives them
	roceSector          float64
	// latest year above the one before (profits also above zero); false when
	// Upstox lacks either year
	revUp, opUp, npUp bool
	assets3y          float64 // non-current assets, percent change over 3 years; NaN for banks
}

// rose reports whether a history's latest figure beats the one before it,
// and, if positive is set, is above zero.
func rose(h []upstox.Period, positive bool) bool {
	return len(h) >= 2 && h[0].Value > h[1].Value && (!positive || h[0].Value > 0)
}

// fundCache is what's stored per ISIN and month, as Upstox sent it.
type fundCache struct {
	Fetched   time.Time      `json:"fetched"`
	Ratios    []upstox.Ratio `json:"key_ratios"`
	Yearly    upstox.Income  `json:"income_yearly"`
	Quarterly upstox.Income  `json:"income_quarterly"`
	Balance   upstox.Balance `json:"balance"`
	// HasBalance marks entries fetched with the balance sheet; older ones
	// are fetched again.
	HasBalance bool `json:"has_balance"`
}

func growth(h []upstox.Period, back int) float64 {
	if len(h) <= back || h[back].Value <= 0 || h[0].Value <= 0 {
		return math.NaN()
	}
	return (math.Pow(h[0].Value/h[back].Value, 1/float64(back)) - 1) * 100
}

func (c fundCache) figures() fundamentals {
	f := fundamentals{ok: true,
		pe: upstox.RatioValue(c.Ratios, "P/E"), pb: upstox.RatioValue(c.Ratios, "P/B"),
		roe: upstox.RatioValue(c.Ratios, "ROE"), roce: upstox.RatioValue(c.Ratios, "ROCE")}
	rev, np := c.Yearly.History("revenue"), c.Yearly.History("net_profit")
	f.revGrowth, f.npGrowth, f.revCAGR3 = growth(rev, 1), growth(np, 1), growth(rev, 3)
	f.roeSector, f.roceSector = upstox.RatioSector(c.Ratios, "ROE"), upstox.RatioSector(c.Ratios, "ROCE")
	f.revUp, f.opUp, f.npUp = rose(rev, false), rose(c.Yearly.History("operating_profit"), true), rose(np, true)
	f.qNPChange = math.NaN()
	if q := c.Quarterly.History("net_profit"); len(q) >= 2 {
		f.lastQ = q[0].Period
		if q[1].Value > 0 {
			f.qNPChange = (q[0].Value/q[1].Value - 1) * 100
		}
	}
	f.quality = f.roe >= qualityMin && f.roce >= qualityMin
	f.assets3y = math.NaN()
	if h := c.Balance.Row("Non-Current Assets"); len(h) >= 4 && h[3].Value > 0 {
		f.assets3y = (h[0].Value/h[3].Value - 1) * 100
	}
	return f
}

// loadFundamentals returns figures for the given ISINs, from this month's
// cache under dir or else from Upstox. A missing or expired token skips the
// fetch with a note rather than failing the report.
func loadFundamentals(ctx context.Context, dir string, isins []string) (map[string]fundamentals, []string, error) {
	month := filepath.Join(dir, time.Now().Format("2006-01"))
	out := map[string]fundamentals{}
	var notes, todo []string
	for _, isin := range isins {
		b, err := os.ReadFile(filepath.Join(month, isin+".json"))
		if err != nil {
			todo = append(todo, isin)
			continue
		}
		var c fundCache
		if err := json.Unmarshal(b, &c); err != nil || !c.HasBalance {
			todo = append(todo, isin)
			continue
		}
		out[isin] = c.figures()
	}
	if len(todo) == 0 {
		return out, nil, nil
	}
	client, err := upstoxClient(ctx)
	if err != nil {
		return out, []string{fmt.Sprintf("fundamentals skipped for %d stocks: %v", len(todo), err)}, nil
	}
	if err := os.MkdirAll(month, 0o755); err != nil {
		return nil, nil, err
	}
	fmt.Printf("fetching fundamentals for %d stocks from Upstox (~%s)\n", len(todo),
		(time.Duration(4*len(todo)) * upstox.DefaultInterval).Round(time.Second))
	failed := 0
	for _, isin := range todo {
		c := fundCache{Fetched: time.Now(), HasBalance: true}
		var err error
		if c.Ratios, err = client.KeyRatios(ctx, isin); err == nil {
			if c.Yearly, err = client.IncomeStatement(ctx, isin, false); err == nil {
				if c.Quarterly, err = client.IncomeStatement(ctx, isin, true); err == nil {
					c.Balance, err = client.BalanceSheet(ctx, isin)
				}
			}
		}
		if errors.Is(err, upstox.ErrAuth) || ctx.Err() != nil {
			notes = append(notes, fmt.Sprintf("fundamentals stopped: %v", err))
			break
		}
		if err != nil {
			failed++ // Upstox has no data for some ISINs, such as new listings
			continue
		}
		b, err := json.Marshal(c)
		if err != nil {
			return nil, nil, err
		}
		if err := os.WriteFile(filepath.Join(month, isin+".json"), b, 0o644); err != nil {
			return nil, nil, err
		}
		out[isin] = c.figures()
	}
	if failed > 0 {
		notes = append(notes, fmt.Sprintf("no Upstox fundamentals for %d stocks", failed))
	}
	return out, notes, nil
}
