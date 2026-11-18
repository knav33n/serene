package main

import (
	"math"
	"testing"
	"time"

	"serene/internal/kite"
	"serene/internal/market"
	"serene/internal/upstox"
)

// fipaCandles makes n daily candles closing at close(i), highs 1% above.
func fipaCandles(n int, close func(i int) float64) ([]kite.Candle, map[market.Date]float64) {
	var c []kite.Candle
	bench := map[market.Date]float64{}
	t := time.Date(2025, 1, 1, 0, 0, 0, 0, market.IST)
	for i := 0; i < n; i++ {
		d := t.AddDate(0, 0, i)
		v := close(i)
		c = append(c, kite.Candle{Time: d, Open: v, High: v * 1.01, Low: v * 0.99, Close: v})
		bench[market.DateOf(d)] = 100 + float64(i)*0.01 // the index barely moves
	}
	return c, bench
}

func TestCombScore(t *testing.T) {
	n := 300
	// A steady riser: at its high and ahead of both indices.
	c, bench := fipaCandles(n, func(i int) float64 { return 100 + float64(i) })
	last := market.DateOf(c[n-1].Time)
	rl := combRules{zonePct: 10, roeMin: 15, roceMin: 15, assetsMin: 30}
	fipa, cepa := combScans(rl)[0], combScans(rl)[1]
	r, ok := combScore(c, bench, bench, last)
	if !ok || !fipa.price(r) || !cepa.price(r) || r.fromHigh > 0 || r.fromHigh < -1.1 {
		t.Fatalf("riser: ok %v %+v", ok, r)
	}
	// A sector index rising faster: CEPA's sector rule fails, FIPA's don't care.
	sector := map[market.Date]float64{}
	for i, k := range c {
		sector[market.DateOf(k.Time)] = 100 * math.Pow(1.02, float64(i))
	}
	if r, ok = combScore(c, bench, sector, last); !ok || !fipa.price(r) || cepa.price(r) || r.sx3m >= 0 {
		t.Fatalf("hot sector: ok %v %+v", ok, r)
	}
	// 20% off its high: fails FIPA's zone though it rose this week.
	c, bench = fipaCandles(n, func(i int) float64 {
		switch {
		case i < 200:
			return 100 + float64(i)
		case i < n-3:
			return 240
		}
		return 245
	})
	if r, ok = combScore(c, bench, bench, last); !ok || fipa.price(r) || r.fromHigh > -10 {
		t.Fatalf("off high: ok %v %+v", ok, r)
	}
	// Under a year of candles, or no candle on the last date: skipped.
	if _, ok = combScore(c[:200], bench, bench, market.DateOf(c[199].Time)); ok {
		t.Fatal("short history scored")
	}
	if _, ok = combScore(c[:n-1], bench, bench, last); ok {
		t.Fatal("stale stock scored")
	}
}

func TestCombFundamentals(t *testing.T) {
	fipa, cepa := combScans(combRules{zonePct: 10, roeMin: 15, roceMin: 15, assetsMin: 30})[0], combScans(combRules{zonePct: 10, roeMin: 15, roceMin: 15, assetsMin: 30})[1]
	good := fundamentals{ok: true, revUp: true, opUp: true, npUp: true, roe: 18, roeSector: 12, roce: 20, roceSector: 15, assets3y: 40}
	for _, s := range []combScan{fipa, cepa} {
		if why := s.fundFail(good); why != "" {
			t.Fatalf("%s: good stock fails: %s", s.name, why)
		}
	}
	bank := good
	bank.roce, bank.roceSector, bank.assets3y = math.NaN(), math.NaN(), math.NaN() // Upstox has neither for banks
	for _, s := range []combScan{fipa, cepa} {
		if why := s.fundFail(bank); why != "" {
			t.Fatalf("%s: bank fails: %s", s.name, why)
		}
	}
	bad := good
	bad.npUp, bad.roe = false, math.NaN()
	if why := fipa.fundFail(bad); why != "net profit, ROE" {
		t.Fatalf("FIPA bad stock: %q", why)
	}
	thin := good
	thin.roe, thin.assets3y = 14, math.NaN() // under 15%, and no asset history though not a bank
	if why := cepa.fundFail(thin); why != "ROE, fixed assets" {
		t.Fatalf("CEPA thin stock: %q", why)
	}
	slow := good
	slow.assets3y = 10
	if why := cepa.fundFail(slow); why != "fixed assets" {
		t.Fatalf("CEPA slow assets: %q", why)
	}
}

func TestRose(t *testing.T) {
	h := func(v ...float64) []upstox.Period {
		var out []upstox.Period
		for _, x := range v {
			out = append(out, upstox.Period{Value: x})
		}
		return out
	}
	if !rose(h(10, 5), true) || rose(h(5, 10), false) || rose(h(-1, -5), true) || !rose(h(-1, -5), false) || rose(h(10), false) {
		t.Fatal("rose")
	}
}
