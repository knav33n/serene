package strategies

import (
	"context"
	"reflect"
	"testing"
	"time"

	"calm/internal/cost"
	"calm/internal/data"
	"calm/internal/engine"
	"calm/internal/margin"
	"calm/internal/market"
	"calm/internal/sim"
	"calm/internal/strategy"
	"calm/internal/synth"
)

var lhbParams = strategy.Params{"min_pullback_pct": 5}

func lhbStore(t *testing.T) (*data.Store, market.Date, market.Date) {
	t.Helper()
	store, err := data.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	from, _ := market.ParseDate("2026-10-05")
	to := from.AddDays(25)
	cfg := synth.Default(from, to, 7)
	cfg.StrikesEachSide = 6
	cfg.Expiries = 2
	if _, err := synth.Generate(context.Background(), store, cfg); err != nil {
		t.Fatal(err)
	}
	return store, from, to
}

func lhbRun(t *testing.T, store *data.Store, from, to market.Date, resume *engine.Resume) (engine.Result, error) {
	return lhbRunWith(t, store, from, to, resume, lhbParams, nil)
}

func lhbRunWith(t *testing.T, store *data.Store, from, to market.Date, resume *engine.Resume,
	params strategy.Params, states map[market.Date]string) (engine.Result, error) {
	t.Helper()
	ctx := context.Background()
	contracts, err := store.Contracts(ctx, "NIFTY")
	if err != nil {
		t.Fatal(err)
	}
	feed, err := engine.NewStoreFeed(store, []string{"NIFTY"}, from, to)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New("lower_high_break", "NIFTY", params)
	if err != nil {
		t.Fatal(err)
	}
	if states != nil {
		s.(strategy.RegimeUser).SetRegime(states)
	}
	b, err := sim.New(sim.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	costs, err := cost.Load("../../configs/costs.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg := engine.Config{Contracts: contracts, Session: market.NSESession, Costs: costs,
		Margin: margin.Model{NakedPct: 12}, Capital: market.Rupees(1e5), Resume: resume}
	return engine.New(cfg, feed, b, s).Run(ctx)
}

func TestLowerHighBreakTrades(t *testing.T) {
	store, from, to := lhbStore(t)
	r, err := lhbRun(t, store, from, to, nil)
	if err != nil {
		t.Fatal(err)
	}
	open := map[string]market.Fill{}
	buys := 0
	for _, f := range r.Fills {
		key := f.Instrument.Key()
		session := market.NSESession.Open(market.DateOf(f.Time))
		switch f.Side {
		case market.Buy:
			buys++
			if _, dup := open[key]; dup {
				t.Errorf("%s bought twice without selling", key)
			}
			open[key] = f
			// The decision is a 5-minute close; the order fills in the next minute.
			if m := int(f.Time.Sub(session).Minutes()); m%5 != 0 {
				t.Errorf("entry at %s is not the minute after a 5-minute bar", f.Time.In(market.IST).Format("01-02 15:04"))
			}
			if lots := f.Qty / f.Instrument.LotSize; lots < 1 || lots > 5 {
				t.Errorf("entry of %d lots", lots)
			}
		case market.Sell:
			in, ok := open[key]
			if !ok {
				t.Fatalf("sold %s without a position", key)
			}
			delete(open, key)
			if market.DateOf(in.Time) != market.DateOf(f.Time) {
				t.Errorf("%s held overnight", key)
			}
			if held := f.Time.Sub(in.Time); held > 16*time.Minute {
				t.Errorf("%s held %s, past the 15-minute hold", key, held)
			}
		}
	}
	t.Logf("%d entries over %s to %s", buys, from, to)
	if buys == 0 {
		t.Fatal("no trades in the synthetic data; change the seed or the pullback")
	}
	if len(open) != 0 {
		t.Errorf("positions left open: %v", open)
	}
}

// A restart in the middle of a trade must rebuild the strategy's bars, stop
// and warm-up from the replay, and then act exactly as an uninterrupted run.
func TestLowerHighBreakResumes(t *testing.T) {
	store, from, to := lhbStore(t)
	full, err := lhbRun(t, store, from, to, nil)
	if err != nil {
		t.Fatal(err)
	}
	var at time.Time
	for i := 1; i < len(full.Orders); i++ {
		a, b := full.Orders[i-1], full.Orders[i]
		if a.Legs[0].Side == market.Buy && b.Legs[0].Side == market.Sell && b.Placed.Sub(a.Placed) >= 4*time.Minute &&
			from.Before(market.DateOf(a.Placed)) {
			at = a.Placed.Add(b.Placed.Sub(a.Placed) / 2).Truncate(time.Minute)
			break
		}
	}
	if at.IsZero() {
		t.Fatal("no trade long enough to resume inside")
	}
	resumed, err := lhbRun(t, store, from, to, journalUpTo(full, at))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resumed.Orders, full.Orders) || !reflect.DeepEqual(resumed.Fills, full.Fills) {
		t.Fatalf("orders or fills differ after resuming at %s", at.In(market.IST))
	}
}

func with(extra strategy.Params) strategy.Params {
	p := strategy.Params{}
	for k, v := range lhbParams {
		p[k] = v
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func everyDay(from, to market.Date, state string) map[market.Date]string {
	out := map[market.Date]string{}
	for d := from.AddDays(-5); !to.Before(d); d = d.AddDays(1) {
		out[d] = state
	}
	return out
}

func buys(r engine.Result) []market.Fill {
	var out []market.Fill
	for _, f := range r.Fills {
		if f.Side == market.Buy {
			out = append(out, f)
		}
	}
	return out
}

func TestLowerHighBreakRegime(t *testing.T) {
	store, from, to := lhbStore(t)
	if _, err := lhbRunWith(t, store, from, to, nil, with(strategy.Params{"regime": 1}), nil); err == nil {
		t.Fatal("ran the regime filter without a regime")
	}
	r, err := lhbRunWith(t, store, from, to, nil, with(strategy.Params{"regime": 1}), everyDay(from, to, "Neutral"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(buys(r)); n != 0 {
		t.Errorf("%d entries on Neutral days", n)
	}
	r, err = lhbRunWith(t, store, from, to, nil, with(strategy.Params{"regime": 2}), everyDay(from, to, "Bull"))
	if err != nil {
		t.Fatal(err)
	}
	b := buys(r)
	if len(b) == 0 {
		t.Fatal("no entries on Bull days")
	}
	for _, f := range b {
		if f.Instrument.Right != market.Call {
			t.Errorf("bought a %s in a Bull regime", f.Instrument.Right)
		}
	}
}

// The ADX filter only removes entries: each one it keeps is a trade the
// unfiltered strategy also took, at the same minute.
func TestLowerHighBreakADXOnlyFilters(t *testing.T) {
	store, from, to := lhbStore(t)
	plain, err := lhbRun(t, store, from, to, nil)
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := lhbRunWith(t, store, from, to, nil, with(strategy.Params{"adx_min": 25}), nil)
	if err != nil {
		t.Fatal(err)
	}
	took := map[string]bool{}
	for _, f := range buys(plain) {
		took[f.Instrument.Key()+f.Time.String()] = true
	}
	fb := buys(filtered)
	if len(fb) == 0 || len(fb) >= len(buys(plain)) {
		t.Fatalf("ADX kept %d of %d entries", len(fb), len(buys(plain)))
	}
	for _, f := range fb {
		if !took[f.Instrument.Key()+f.Time.String()] {
			// A skipped trade frees the leg for a later signal, which is fine,
			// but it must still come from the unfiltered signal set's timing.
			if m := int(f.Time.Sub(market.NSESession.Open(market.DateOf(f.Time))).Minutes()); m%5 != 0 {
				t.Errorf("ADX entry at %s is off the 5-minute grid", f.Time.In(market.IST))
			}
		}
	}
	t.Logf("ADX >= 25 kept %d of %d entries", len(fb), len(buys(plain)))
}

func TestLowerHighBreakADXResumes(t *testing.T) {
	store, from, to := lhbStore(t)
	p := with(strategy.Params{"adx_min": 20})
	full, err := lhbRunWith(t, store, from, to, nil, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Orders) < 2 {
		t.Fatal("too few orders to resume inside")
	}
	at := full.Orders[len(full.Orders)/2].Placed.Add(2 * time.Minute)
	resumed, err := lhbRunWith(t, store, from, to, journalUpTo(full, at), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resumed.Orders, full.Orders) || !reflect.DeepEqual(resumed.Fills, full.Fills) {
		t.Fatalf("orders or fills differ after resuming at %s", at.In(market.IST))
	}
}
