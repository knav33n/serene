package strategies

import (
	"testing"

	"serene/internal/market"
	"serene/internal/strategy"
)

func TestRSIDivergenceTrades(t *testing.T) {
	store, from, to := lhbStore(t)
	params := strategy.Params{"oversold": 0}
	r, err := lhbRunWithStrategy(t, store, from, to, "rsi_divergence", params)
	if err != nil {
		t.Fatal(err)
	}
	open := map[string]market.Fill{}
	buys := 0
	for _, f := range r.Fills {
		key := f.Instrument.Key()
		switch f.Side {
		case market.Buy:
			buys++
			if _, dup := open[key]; dup {
				t.Errorf("%s bought twice without selling", key)
			}
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
	}
	t.Logf("%d entries", buys)
	if buys == 0 {
		t.Fatal("no trades in the synthetic data")
	}
	if len(open) != 0 {
		t.Errorf("positions left open: %v", open)
	}
}

func TestRSIDivergenceOnIndexTrades(t *testing.T) {
	store, from, to := lhbStore(t)
	r, err := lhbRunWithStrategy(t, store, from, to, "rsi_divergence", strategy.Params{"chart": 1, "oversold": 0})
	if err != nil {
		t.Fatal(err)
	}
	open := map[string]bool{}
	buys := 0
	for _, f := range r.Fills {
		key := f.Instrument.Key()
		if f.Side == market.Buy {
			buys++
			if open[key] {
				t.Errorf("%s bought twice without selling", key)
			}
			open[key] = true
		} else {
			if !open[key] {
				t.Fatalf("sold %s without a position", key)
			}
			delete(open, key)
		}
	}
	t.Logf("%d entries", buys)
	if buys == 0 {
		t.Fatal("no trades in the synthetic data")
	}
	if len(open) != 0 {
		t.Errorf("positions left open: %v", open)
	}
}

func TestRSIDivergenceDiverges(t *testing.T) {
	rsi := []float64{30, 0, 0, 40}
	reg := &rsiDivergence{minGap: 1, maxGap: 10}
	hid := &rsiDivergence{minGap: 1, maxGap: 10, hidden: true}
	// Lower low, higher RSI: regular bullish only.
	if !reg.diverges(rsi, 3, 0, 90, 100, false) || hid.diverges(rsi, 3, 0, 90, 100, false) {
		t.Error("regular bullish")
	}
	// Higher low, lower RSI: hidden bullish only.
	rsi = []float64{40, 0, 0, 30}
	if reg.diverges(rsi, 3, 0, 110, 100, false) || !hid.diverges(rsi, 3, 0, 110, 100, false) {
		t.Error("hidden bullish")
	}
	// Lower high, higher RSI: hidden bearish only.
	rsi = []float64{60, 0, 0, 70}
	if reg.diverges(rsi, 3, 0, 90, 100, true) || !hid.diverges(rsi, 3, 0, 90, 100, true) {
		t.Error("hidden bearish")
	}
}

func TestRSIHiddenOnIndexTrades(t *testing.T) {
	store, from, to := lhbStore(t)
	r, err := lhbRunWithStrategy(t, store, from, to, "rsi_divergence",
		strategy.Params{"chart": 1, "hidden": 1, "oversold": 0, "trend_ema": 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Fills) == 0 {
		t.Fatal("no trades in the synthetic data")
	}
	t.Logf("%d fills", len(r.Fills))
}

func TestRSIConfirmedOnIndexTrades(t *testing.T) {
	store, from, to := lhbStore(t)
	r, err := lhbRunWithStrategy(t, store, from, to, "rsi_divergence",
		strategy.Params{"chart": 1, "oversold": 0, "confirm_ema": 20, "confirm_bars": 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d fills", len(r.Fills))
	if len(r.Fills) == 0 {
		t.Fatal("no confirmed trades in the synthetic data")
	}
	if _, err := New("rsi_divergence", "NIFTY", strategy.Params{"confirm_ema": 20}); err == nil {
		t.Error("confirm_ema accepted on the option chart")
	}
}
