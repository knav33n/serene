package engine

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"calm/internal/cost"
	"calm/internal/margin"
	"calm/internal/market"
	"calm/internal/sim"
	"calm/internal/strategy"
)

var (
	day, _    = market.ParseDate("2026-11-09") // an expiry day
	nextDay   = day.AddDays(1)
	indexK    = market.Index(market.NSE, "NIFTY").Key()
	ce        = market.Option(market.NFO, "NIFTY", day, market.Rupees(24000), market.Call, 65)
	pe        = market.Option(market.NFO, "NIFTY", day, market.Rupees(24000), market.Put, 65)
	later     = market.Option(market.NFO, "NIFTY", nextDay, market.Rupees(24000), market.Call, 65)
	contracts = []market.Instrument{ce, pe, later}
)

// script runs a callback on each 1-minute index bar.
type script struct {
	onBar func(ctx strategy.Context, b market.Bar) error
	fills []market.Fill
}

func (s *script) Init(ctx strategy.Context) error { return ctx.Subscribe(indexK, market.Minute1) }
func (s *script) OnBar(ctx strategy.Context, b market.Bar) error {
	return s.onBar(ctx, b)
}
func (s *script) OnFill(_ strategy.Context, f market.Fill) error {
	s.fills = append(s.fills, f)
	return nil
}

func bar(key string, t time.Time, open, cl market.Paise) market.Bar {
	return market.Bar{Key: key, Timeframe: market.Minute1, Time: t, End: t.Add(time.Minute),
		Open: open, High: max(open, cl), Low: min(open, cl), Close: cl, Volume: 75}
}

// session returns index bars for d at a fixed level, plus an option bar per
// minute from each given contract at fixed prices.
func session(d market.Date, level market.Paise, cs ...market.Instrument) []market.Bar {
	var out []market.Bar
	for m := range 375 {
		t := d.At(9, 15).Add(time.Duration(m) * time.Minute)
		out = append(out, bar(indexK, t, level, level))
		for _, c := range cs {
			out = append(out, bar(c.Key(), t, 10000, 10000))
		}
	}
	return out
}

func replace(bars []market.Bar, key string, t time.Time, open, cl market.Paise) {
	for i := range bars {
		if bars[i].Key == key && bars[i].Time.Equal(t) {
			bars[i] = bar(key, t, open, cl)
			return
		}
	}
	panic("bar not found: " + key)
}

func runWith(t *testing.T, bars []market.Bar, costs *cost.Schedule, capital market.Paise, s strategy.Strategy) (Result, *sim.Broker) {
	t.Helper()
	b, err := sim.New(sim.Config{Tick: 5, MaxWait: 3 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Contracts: contracts, Session: market.NSESession, Costs: costs,
		Margin: margin.Model{NakedPct: 10}, Capital: capital}
	r, err := New(cfg, NewSliceFeed(bars), b, s).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return r, b
}

func once(fn func(ctx strategy.Context) error, at time.Time) *script {
	done := false
	return &script{onBar: func(ctx strategy.Context, b market.Bar) error {
		if !done && !b.End.Before(at) {
			done = true
			return fn(ctx)
		}
		return nil
	}}
}

func TestOrderFillsAtNextBarOpen(t *testing.T) {
	bars := session(day, 2400000, ce)
	replace(bars, ce.Key(), day.At(10, 0), 11100, 22200) // decision bar
	replace(bars, ce.Key(), day.At(10, 1), 33300, 44400) // fill bar
	s := once(func(ctx strategy.Context) error {
		return ctx.Submit("x", market.Leg{Instrument: ce, Side: market.Buy, Lots: 1})
	}, day.At(10, 1))
	_, _ = runWith(t, bars, cost.Zero(), market.Rupees(1e6), s)
	if len(s.fills) == 0 || s.fills[0].Price != 33300 {
		t.Fatalf("expected fill at next open 33300, got %+v", s.fills)
	}
}

// Golden test: a short strangle-style pair, one leg closed by hand and the
// other settled. Costs and P&L are worked out in the comments.
func TestGoldenMultiLeg(t *testing.T) {
	costs, err := cost.Load("../../configs/costs.yaml")
	if err != nil {
		t.Fatal(err)
	}
	bars := session(day, market.Rupees(24100), ce, pe)
	for _, k := range []market.Instrument{ce, pe} {
		replace(bars, k.Key(), day.At(10, 1), 10000, 10000)
	}
	replace(bars, pe.Key(), day.At(11, 1), 4000, 4000)
	s := &script{}
	step := 0
	s.onBar = func(ctx strategy.Context, b market.Bar) error {
		switch {
		case step == 0 && b.End.Equal(day.At(10, 1)):
			step = 1
			return ctx.Submit("pair",
				market.Leg{Instrument: ce, Side: market.Sell, Lots: 1},
				market.Leg{Instrument: pe, Side: market.Sell, Lots: 1})
		case step == 1 && b.End.Equal(day.At(11, 1)):
			step = 2
			return ctx.Submit("pair", market.Leg{Instrument: pe, Side: market.Buy, Lots: 1})
		}
		return nil
	}
	r, _ := runWith(t, bars, costs, market.Rupees(1000000), s)

	// Sell CE and PE at Rs 100: turnover 650000p each.
	//   brokerage 2000 + exchange 231 + SEBI 1 + GST 402 + STT 975 = 3609 each
	// Buy PE at Rs 40: turnover 260000p.
	//   brokerage 2000, exchange 92.378 -> 92, SEBI 0.26 -> 0,
	//   GST 18% of 2092 = 376.56 -> 377, stamp 7.8 -> 8. Total 2477.
	// CE settles at intrinsic: index 24100 - strike 24000 = Rs 100. A short
	// position being settled pays no STT.
	wantCharges := market.Paise(3609 + 3609 + 2477)
	if r.Charges != wantCharges {
		t.Errorf("charges %d, want %d", r.Charges, wantCharges)
	}
	// CE: sold 100, settled 100 = 0. PE: sold 100, bought 40 = Rs 60 x 65.
	wantNet := market.Rupees(60*65) - wantCharges
	if got := r.Equity[len(r.Equity)-1].Equity - r.Capital; got != wantNet {
		t.Errorf("net %d, want %d", got, wantNet)
	}
	if len(r.Trades) != 2 {
		t.Fatalf("trades %d", len(r.Trades))
	}
	settled := 0
	for _, tr := range r.Trades {
		if tr.Settled {
			settled++
		}
	}
	if settled != 1 {
		t.Errorf("settled legs %d", settled)
	}
}

func TestSettlementAtExpiry(t *testing.T) {
	cases := []struct {
		name  string
		inst  market.Instrument
		side  market.Side
		index market.Paise
		want  market.Paise // gross P&L per unit
	}{
		{"long call ITM", ce, market.Buy, market.Rupees(24150), market.Rupees(50)},
		{"long call OTM expires worthless", ce, market.Buy, market.Rupees(23900), market.Rupees(-100)},
		{"short put OTM keeps premium", pe, market.Sell, market.Rupees(24100), market.Rupees(100)},
		{"short put ITM", pe, market.Sell, market.Rupees(23700), market.Rupees(-200)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := once(func(ctx strategy.Context) error {
				return ctx.Submit("x", market.Leg{Instrument: c.inst, Side: c.side, Lots: 1})
			}, day.At(10, 0))
			r, _ := runWith(t, session(day, c.index, c.inst), cost.Zero(), market.Rupees(1e7), s)
			if len(r.Trades) != 1 || !r.Trades[0].Settled {
				t.Fatalf("trades %+v", r.Trades)
			}
			if want := c.want * 65; r.Trades[0].Gross != want {
				t.Errorf("gross %d want %d", r.Trades[0].Gross, want)
			}
		})
	}
}

func runSettled(closes map[string]map[market.Date]market.Paise) (Result, error) {
	s := once(func(ctx strategy.Context) error {
		return ctx.Submit("x", market.Leg{Instrument: ce, Side: market.Buy, Lots: 1})
	}, day.At(10, 0))
	b, err := sim.New(sim.Config{Tick: 5, MaxWait: 3 * time.Minute})
	if err != nil {
		return Result{}, err
	}
	cfg := Config{Contracts: contracts, Session: market.NSESession, Costs: cost.Zero(),
		Margin: margin.Model{NakedPct: 10}, Capital: market.Rupees(1e7), IndexCloses: closes}
	return New(cfg, NewSliceFeed(session(day, market.Rupees(24100), ce)), b, s).Run(context.Background())
}

func TestSettlementUsesTheOfficialClose(t *testing.T) {
	// The last bar says 24100; the official close of 24150 is what pays.
	r, err := runSettled(map[string]map[market.Date]market.Paise{indexK: {day: market.Rupees(24150)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Trades) != 1 || !r.Trades[0].Settled {
		t.Fatalf("trades %+v", r.Trades)
	}
	if want := market.Rupees(50) * 65; r.Trades[0].Gross != want {
		t.Errorf("gross %d want %d", r.Trades[0].Gross, want)
	}
}

func TestMissingOfficialCloseIsAnError(t *testing.T) {
	// The last bar must not stand in for a close that is absent.
	_, err := runSettled(map[string]map[market.Date]market.Paise{indexK: {}})
	if err == nil || !strings.Contains(err.Error(), "no index close") {
		t.Fatalf("err %v", err)
	}
}

func TestExpiryComesFromTheContractNotTheCalendar(t *testing.T) {
	// 2026-11-09 is a Monday; a holiday moved this expiry off Tuesday.
	if day.Weekday() != time.Monday {
		t.Fatal("test date should be a Monday")
	}
	s := once(func(ctx strategy.Context) error {
		return ctx.Submit("x", market.Leg{Instrument: ce, Side: market.Buy, Lots: 1})
	}, day.At(10, 0))
	r, _ := runWith(t, session(day, market.Rupees(24000), ce), cost.Zero(), market.Rupees(1e6), s)
	if len(r.Trades) != 1 || !r.Trades[0].Settled {
		t.Fatalf("position should settle on the Monday: %+v", r.Trades)
	}
}

func TestPositionPastExpiryIsAnError(t *testing.T) {
	bars := append(session(day.AddDays(-1), market.Rupees(24000), ce), session(day.AddDays(1), market.Rupees(24000))...)
	s := once(func(ctx strategy.Context) error {
		return ctx.Submit("x", market.Leg{Instrument: ce, Side: market.Buy, Lots: 1})
	}, day.AddDays(-1).At(10, 0))
	b, _ := sim.New(sim.Config{Tick: 5, MaxWait: time.Minute})
	cfg := Config{Contracts: contracts, Session: market.NSESession, Costs: cost.Zero(),
		Margin: margin.Model{NakedPct: 10}, Capital: market.Rupees(1e6)}
	_, err := New(cfg, NewSliceFeed(bars), b, s).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "past its expiry") {
		t.Fatalf("got %v", err)
	}
}

func TestMissingIndexCloseIsAnError(t *testing.T) {
	var bars []market.Bar
	for _, b := range session(day, market.Rupees(24000), ce) {
		if b.Key == indexK && b.Time.After(day.At(12, 0)) {
			continue
		}
		bars = append(bars, b)
	}
	s := once(func(ctx strategy.Context) error {
		return ctx.Submit("x", market.Leg{Instrument: ce, Side: market.Buy, Lots: 1})
	}, day.At(10, 0))
	b, _ := sim.New(sim.Config{Tick: 5, MaxWait: time.Minute})
	cfg := Config{Contracts: contracts, Session: market.NSESession, Costs: cost.Zero(),
		Margin: margin.Model{NakedPct: 10}, Capital: market.Rupees(1e6)}
	_, err := New(cfg, NewSliceFeed(bars), b, s).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no index close") {
		t.Fatalf("got %v", err)
	}
}

func TestNoCandleMeansNoFill(t *testing.T) {
	var bars []market.Bar
	for _, b := range session(day, market.Rupees(24000), ce) {
		// The contract stops trading after 10:00.
		if b.Key == ce.Key() && b.Time.After(day.At(10, 0)) {
			continue
		}
		bars = append(bars, b)
	}
	s := once(func(ctx strategy.Context) error {
		return ctx.Submit("x", market.Leg{Instrument: ce, Side: market.Buy, Lots: 1})
	}, day.At(10, 5))
	r, b := runWith(t, bars, cost.Zero(), market.Rupees(1e6), s)
	if len(s.fills) != 0 || b.Unfilled != 1 || len(r.Trades) != 0 {
		t.Fatalf("fills %d unfilled %d", len(s.fills), b.Unfilled)
	}
}

func TestZeroVolumeBarMeansNoFill(t *testing.T) {
	bars := session(day, market.Rupees(24000), ce)
	// Minutes after the order carry the last price with no trade, then one trades.
	for i := range bars {
		if bars[i].Key == ce.Key() && bars[i].Time.Before(day.At(10, 3)) {
			bars[i].Volume = 0
		}
	}
	s := once(func(ctx strategy.Context) error {
		return ctx.Submit("x", market.Leg{Instrument: ce, Side: market.Buy, Lots: 1})
	}, day.At(10, 0))
	r, _ := runWith(t, bars, cost.Zero(), market.Rupees(1e7), s)
	if len(r.Fills) == 0 || !r.Fills[0].Time.Equal(day.At(10, 3)) {
		t.Fatalf("fills %+v", r.Fills)
	}
}

func TestChainHidesContractsNotYetTraded(t *testing.T) {
	bars := session(day, market.Rupees(24000))
	bars = append(bars, bar(ce.Key(), day.At(11, 0), 10000, 10000))
	var before, after int
	s := &script{onBar: func(ctx strategy.Context, b market.Bar) error {
		switch {
		case b.End.Equal(day.At(10, 0)):
			before = len(ctx.Chain("NIFTY", day))
		case b.End.Equal(day.At(11, 1)):
			after = len(ctx.Chain("NIFTY", day))
		}
		return nil
	}}
	runWith(t, bars, cost.Zero(), market.Rupees(1e6), s)
	if before != 0 || after != 1 {
		t.Fatalf("before %d after %d", before, after)
	}
}

func TestMarginRejection(t *testing.T) {
	var got error
	s := once(func(ctx strategy.Context) error {
		got = ctx.Submit("x", market.Leg{Instrument: ce, Side: market.Sell, Lots: 1})
		return nil
	}, day.At(10, 0))
	// Naked margin is 10% of Rs 24,000 x 65 = Rs 1,56,000.
	r, _ := runWith(t, session(day, market.Rupees(24000), ce), cost.Zero(), market.Rupees(50000), s)
	if !errors.Is(got, strategy.ErrRejected) || r.Rejected != 1 || len(r.Orders) != 0 {
		t.Fatalf("got %v rejected %d orders %d", got, r.Rejected, len(r.Orders))
	}
}

// A strategy that reads the option's price each minute and flips on it, so any
// leak of future data would change its orders.
func reactive() *script {
	s := &script{}
	s.onBar = func(ctx strategy.Context, b market.Bar) error {
		q, ok := ctx.Last(later.Key())
		if !ok || int(b.End.Sub(day.At(9, 15)).Minutes())%10 != 0 || ctx.Pending() > 0 {
			return nil
		}
		side := market.Buy
		if q.Close%2 == 1 {
			side = market.Sell
		}
		return ctx.Submit("x", market.Leg{Instrument: later, Side: side, Lots: 1})
	}
	return s
}

func TestChangingTheFutureCannotChangeThePast(t *testing.T) {
	data := func() []market.Bar {
		bars := session(day, market.Rupees(24000))
		for m := range 375 {
			tm := day.At(9, 15).Add(time.Duration(m) * time.Minute)
			px := market.Paise(10000 + (m*m*7)%991)
			bars = append(bars, bar(later.Key(), tm, px, px+market.Paise(m%3)))
		}
		return bars
	}
	cut := day.At(12, 0)
	orig := reactive()
	r1, _ := runWith(t, data(), cost.Zero(), market.Rupees(1e8), orig)

	changed := data()
	for i := range changed {
		if !changed[i].Time.Before(cut) {
			changed[i].Open *= 3
			changed[i].Close = changed[i].Close*3 + 1
			changed[i].High = max(changed[i].Open, changed[i].Close)
			changed[i].Low = min(changed[i].Open, changed[i].Close)
		}
	}
	alt := reactive()
	r2, _ := runWith(t, changed, cost.Zero(), market.Rupees(1e8), alt)

	before := func(os []market.Order) []market.Order {
		var out []market.Order
		for _, o := range os {
			if !o.Placed.After(cut) {
				out = append(out, o)
			}
		}
		return out
	}
	a, b := before(r1.Orders), before(r2.Orders)
	if len(a) == 0 || !reflect.DeepEqual(a, b) {
		t.Fatalf("orders before the cut differ: %d vs %d", len(a), len(b))
	}
	fillsBefore := func(fs []market.Fill) []market.Fill {
		var out []market.Fill
		for _, f := range fs {
			if f.Time.Before(cut) {
				out = append(out, f)
			}
		}
		return out
	}
	if !reflect.DeepEqual(fillsBefore(r1.Fills), fillsBefore(r2.Fills)) {
		t.Fatal("fills before the cut differ")
	}
	if reflect.DeepEqual(r1.Orders, r2.Orders) {
		t.Fatal("test is too weak: the changed future did not affect later orders")
	}
}

func TestMultipleTimeframesDeliverClosedBarsOnly(t *testing.T) {
	var ends []time.Time
	s := &tfRecorder{seen: &ends}
	runWith(t, session(day, market.Rupees(24000)), cost.Zero(), market.Rupees(1e6), s)
	if len(ends) != 75 {
		t.Fatalf("got %d 5m bars", len(ends))
	}
	if !ends[0].Equal(day.At(9, 20)) {
		t.Fatalf("first bar seen at %s", ends[0])
	}
}

type tfRecorder struct{ seen *[]time.Time }

func (r *tfRecorder) Init(ctx strategy.Context) error { return ctx.Subscribe(indexK, market.Minute5) }
func (r *tfRecorder) OnBar(ctx strategy.Context, b market.Bar) error {
	if !ctx.Now().Equal(b.End) {
		return errors.New("bar delivered before it closed")
	}
	*r.seen = append(*r.seen, ctx.Now())
	return nil
}
func (r *tfRecorder) OnFill(strategy.Context, market.Fill) error { return nil }
