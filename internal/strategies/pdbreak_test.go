package strategies

import (
	"testing"

	"serene/internal/market"
	"serene/internal/strategy"
)

func TestPDBreakTrailOncePerSideADay(t *testing.T) {
	store, from, to := lhbStore(t)
	r, err := lhbRunWithStrategy(t, store, from, to, "pd_break_trail", strategy.Params{"strike_step": 100})
	if err != nil {
		t.Fatal(err)
	}
	buys := map[string]int{}
	open := map[string]market.Fill{}
	for _, f := range r.Fills {
		key := f.Instrument.Key()
		switch f.Side {
		case market.Buy:
			if market.DateOf(f.Time) == from {
				t.Errorf("bought %s on the first day, with no previous day", key)
			}
			buys[market.DateOf(f.Time).String()+string(f.Instrument.Right)]++
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
	for k, n := range buys {
		if n > 1 {
			t.Errorf("%d entries for %s", n, k)
		}
	}
	if len(open) != 0 {
		t.Errorf("%d positions left open", len(open))
	}
	t.Logf("%d entries", len(buys))
}
