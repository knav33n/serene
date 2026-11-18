package main

import (
	"math"
	"testing"

	"serene/internal/upstox"
)

func TestFundamentalsFigures(t *testing.T) {
	c := fundCache{
		Ratios: []upstox.Ratio{{Name: "P/E", Company: "30.5"}, {Name: "ROE", Company: "22%"}, {Name: "ROCE", Company: "19.9%"}},
		Yearly: upstox.Income{Lines: []upstox.Line{
			{Category: "revenue", History: []upstox.Period{{1331, "Mar 2026"}, {1210, "Mar 2025"}, {1100, "Mar 2024"}, {1000, "Mar 2023"}}},
			{Category: "net_profit", History: []upstox.Period{{-5, "Mar 2026"}, {10, "Mar 2025"}}},
		}},
		Quarterly: upstox.Income{Lines: []upstox.Line{
			{Category: "net_profit", History: []upstox.Period{{12, "Sep 2026"}, {10, "Jun 2026"}}},
		}},
	}
	f := c.figures()
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if !f.ok || f.pe != 30.5 || f.roe != 22 || !math.IsNaN(f.pb) || f.quality {
		t.Errorf("%+v", f)
	}
	if !near(f.revGrowth, 10) || !near(f.revCAGR3, 10) || !math.IsNaN(f.npGrowth) {
		t.Errorf("growth %+v", f)
	}
	if f.lastQ != "Sep 2026" || !near(f.qNPChange, 20) {
		t.Errorf("quarter %+v", f)
	}
	c.Ratios[2].Company = "20%"
	if !c.figures().quality {
		t.Error("ROE 22% and ROCE 20% should pass")
	}
}
