package manual

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"serene/internal/market"
)

func TestOneTradeADay(t *testing.T) {
	z := &fake{name: "zerodha", funds: 1e6}
	d := desk(t, z)
	l, err := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	d.Ledger = l
	req := BuyReq{Expiry: expiry.String(), Strike: 25400, Right: "CE", Target: 2, Capital: 100000, Brokers: []string{"zerodha"}}
	if _, err := d.Buy(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	n := len(z.placed)
	if _, err := d.Buy(context.Background(), req); err == nil || !strings.Contains(err.Error(), "already been taken") {
		t.Fatalf("second buy: %v", err)
	}
	if len(z.placed) != n {
		t.Errorf("the second buy placed orders: %d → %d", n, len(z.placed))
	}
	// Kept across a restart; the next day is free again.
	l2, err := OpenLedger(l.path)
	if err != nil {
		t.Fatal(err)
	}
	if !l2.TradedOn(market.Date{Year: 2026, Month: time.November, Day: 20}) || l2.TradedOn(market.Date{Year: 2026, Month: time.November, Day: 23}) {
		t.Error("traded days not kept")
	}
}

func TestFailedBuyDoesntUseTheDay(t *testing.T) {
	d := desk(t) // no broker logged in
	d.Ledger, _ = OpenLedger("")
	req := BuyReq{Expiry: expiry.String(), Strike: 25400, Right: "CE", Target: 2, Capital: 100000, Brokers: []string{"zerodha"}}
	if _, err := d.Buy(context.Background(), req); err == nil {
		t.Fatal("bought with no broker")
	}
	if d.Ledger.TradedOn(market.Date{Year: 2026, Month: time.November, Day: 20}) {
		t.Error("a buy that placed nothing used up the day")
	}
}

// sinceForTest moves the card's first day to 18 Nov, so the desk fakes'
// Friday 20 Nov falls inside it.
func sinceForTest(t *testing.T) {
	saved := Since
	Since = market.Date{Year: 2026, Month: time.November, Day: 18}
	t.Cleanup(func() { Since = saved })
}

func TestCardTally(t *testing.T) {
	sinceForTest(t)
	l, _ := OpenLedger("")
	day := func(dd int) market.Date { return market.Date{Year: 2026, Month: time.November, Day: dd} }
	l.Put(day(17), "zerodha", DayNet{Net: 9999}) // before Since: not counted
	l.Put(day(18), "upstox", DayNet{Net: 812.39})
	l.Put(day(19), "upstox", DayNet{Net: 779.82})
	l.Put(day(18), "zerodha", DayNet{Net: -500})
	// Friday 20th: live, not yet recorded
	tl := l.Tally(day(20), map[string]float64{"zerodha": 1000, "upstox": 100})
	if tl.Brokers["zerodha"] != 500 || tl.Brokers["upstox"] != 1692.21 || tl.Net != 2192.21 || tl.Days != 3 {
		t.Errorf("tally %+v", tl)
	}
	// Once the 20th is recorded, the live figure isn't added again.
	l.Put(day(20), "zerodha", DayNet{Net: 1000})
	if tl := l.Tally(day(20), map[string]float64{"zerodha": 1000}); tl.Brokers["zerodha"] != 500 {
		t.Errorf("double-counted today: %+v", tl)
	}
	// On a holiday (24th) the recorded days still sum; 18, 19, 20 and 23 Nov traded.
	if tl := l.Tally(day(24), nil); tl.Days != 4 || tl.Net != 2092.21 {
		t.Errorf("holiday tally %+v", tl)
	}
}

func TestOverviewCardAndTraded(t *testing.T) {
	sinceForTest(t)
	z := &fake{name: "zerodha", funds: 1e6,
		positions: []Position{{Symbol: "A", Name: "A", Exchange: "NFO", Product: "MIS", Qty: 0, PnL: 500}},
		fills: []Fill{{OrderID: "1", Time: "09:30:00", Symbol: "A", Product: "MIS", Side: "BUY", Qty: 65, Price: 100},
			{OrderID: "2", Time: "10:30:00", Symbol: "A", Product: "MIS", Side: "SELL", Qty: 65, Price: 107.69}},
		orders: []Order{{ID: "1", Symbol: "A", Tag: Tag, Side: "BUY"}, {ID: "2", Symbol: "A", Tag: Tag, Side: "SELL"}}}
	d := desk(t, z)
	d.Ledger, _ = OpenLedger("")
	d.Ledger.Put(market.Date{Year: 2026, Month: time.November, Day: 18}, "upstox", DayNet{Net: 812.39})
	d.Now = at(14, 0)
	v, err := d.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The fake charges ₹10 an order: 500 − 20 live today.
	if !v.TradingDay || !v.Traded || v.Card.Brokers["zerodha"] != 480 || v.Card.Net != 1292.39 {
		t.Errorf("view traded %v trading %v card %+v", v.Traded, v.TradingDay, v.Card)
	}
	if _, ok := d.Ledger.Get(market.Date{Year: 2026, Month: time.November, Day: 20}, "zerodha"); ok {
		t.Error("recorded before the close")
	}
	d.Now = at(15, 35)
	if err := d.RecordDay(context.Background()); err == nil || !strings.Contains(err.Error(), "Upstox") {
		t.Errorf("RecordDay with Upstox unreadable: %v", err)
	}
	if n, ok := d.Ledger.Get(market.Date{Year: 2026, Month: time.November, Day: 20}, "zerodha"); !ok || n.Net != 480 || n.Source != "desk" {
		t.Errorf("recorded %+v %v", n, ok)
	}
	// A holiday: no trading day, the card still sums.
	d.Now = func() time.Time { return market.Date{Year: 2026, Month: time.November, Day: 24}.At(11, 0) }
	z.fills, z.positions, z.orders = nil, nil, nil
	v, _ = d.Overview(context.Background())
	if v.TradingDay || v.Card.Net != 1292.39 {
		t.Errorf("holiday view trading %v card %+v", v.TradingDay, v.Card)
	}
}
