package manual

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"serene/internal/cost"
	"serene/internal/market"
)

type fake struct {
	mu        sync.Mutex
	name      string
	funds     float64
	positions []Position
	fills     []Fill
	orders    []Order
	states    map[string]OrderState
	placed    []OrderReq
	cancelled []string
	fillAt    func(o OrderReq) OrderState // what a placed order becomes
	n         int
}

func (f *fake) Name() string    { return f.name }
func (f *fake) Product() string { return "MIS" }
func (f *fake) Symbol(c Contract) (string, error) {
	return fmt.Sprintf("NIFTY%s%g%s", c.Expiry, c.Strike, c.Right), nil
}
func (f *fake) Funds(context.Context) (float64, error)        { return f.funds, nil }
func (f *fake) Positions(context.Context) ([]Position, error) { return f.positions, nil }
func (f *fake) Fills(context.Context) ([]Fill, error)         { return f.fills, nil }
func (f *fake) Charges(_ context.Context, fs []Fill) (map[string]float64, error) {
	out := map[string]float64{}
	for _, x := range fs {
		out[x.OrderID] = 10
	}
	return out, nil
}
func (f *fake) Orders(context.Context) ([]Order, error) { return f.orders, nil }
func (f *fake) Place(_ context.Context, o OrderReq) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := fmt.Sprint(f.n)
	f.placed = append(f.placed, o)
	st := OrderState{Done: true, Filled: o.Qty, Average: o.Price}
	if f.fillAt != nil {
		st = f.fillAt(o)
	}
	if f.states == nil {
		f.states = map[string]OrderState{}
	}
	f.states[id] = st
	f.orders = append(f.orders, Order{ID: id, Symbol: o.Symbol, Exchange: o.Exchange, Side: o.Side, Tag: Tag, Open: !st.Done, Price: o.Price})
	return id, nil
}
func (f *fake) Cancel(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, id)
	if s, ok := f.states[id]; ok {
		s.Done = true
		f.states[id] = s
	}
	return nil
}
func (f *fake) Order(_ context.Context, id string) (OrderState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.states[id], nil
}

func costs(t *testing.T) *cost.Schedule {
	s, err := cost.Load("../../configs/costs.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var expiry = market.Date{Year: 2026, Month: time.November, Day: 24}

var nifty = Contract{Expiry: expiry, Strike: 25400, Right: "CE", LotSize: 65, FreezeQty: 1755, Tick: 0.05}

// at is Friday 20 Nov 2026, IST.
func at(h, m int) func() time.Time {
	return func() time.Time { return market.Date{Year: 2026, Month: time.November, Day: 20}.At(h, m) }
}

func desk(t *testing.T, bs ...*fake) *Desk {
	return &Desk{
		Broker: func(_ context.Context, name string) (Broker, error) {
			for _, b := range bs {
				if b.name == name {
					return b, nil
				}
			}
			return nil, fmt.Errorf("%s isn't logged in", name)
		},
		Contracts: func(context.Context) ([]Contract, error) {
			later := nifty
			later.Expiry = expiry.AddDays(7)
			past := nifty
			past.Expiry = market.Date{Year: 2026, Month: time.November, Day: 17}
			return []Contract{later, nifty, past}, nil
		},
		LTP:       func(context.Context, Contract) (float64, error) { return 150, nil },
		Costs:     costs(t),
		Now:       at(10, 0),
		LimitWait: 30 * time.Millisecond,
		Poll:      5 * time.Millisecond,
	}
}

func TestOrderSizes(t *testing.T) {
	cases := []struct {
		qty, lot, freeze int64
		want             string
	}{
		{130, 65, 1755, "[130]"},
		{1755, 65, 1755, "[1755]"},
		{3640, 65, 1755, "[1755 1755 130]"},
		{3640, 65, 0, "[3640]"},
		{200, 75, 1800, "[200]"},
	}
	for _, c := range cases {
		if got := fmt.Sprint(OrderSizes(c.qty, c.lot, c.freeze)); got != c.want {
			t.Errorf("%d/%d/%d: %s, want %s", c.qty, c.lot, c.freeze, got, c.want)
		}
	}
}

func TestTargetNetsThePercentAfterCharges(t *testing.T) {
	d := desk(t)
	now := at(10, 0)()
	for _, qty := range []int64{65, 3640} {
		target, err := d.Target(nifty, 150, qty, 2, now)
		if err != nil {
			t.Fatal(err)
		}
		net := func(sell float64) float64 {
			in, _ := d.charges(nifty, market.Buy, qty, 150, now)
			out, _ := d.charges(nifty, market.Sell, qty, sell, now)
			return (sell-150)*float64(qty) - in - out
		}
		want := 0.02 * 150 * float64(qty)
		if net(target) < want || net(target-0.05) >= want {
			t.Errorf("qty %d: target %.2f nets %.2f, a tick less %.2f, want %.2f", qty, target, net(target), net(target-0.05), want)
		}
	}
}

func TestLotsStayInsideTheSpendAndTheOrderCap(t *testing.T) {
	d := desk(t)
	now := at(10, 0)()
	n, err := d.Lots(nifty, 500000, 150, now)
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := d.charges(nifty, market.Buy, n*65, 150, now)
	if n != 51 || 150*float64(n*65)+ch > 500000 {
		t.Errorf("lots %d, spend %.2f", n, 150*float64(n*65)+ch)
	}
	if n, _ := d.Lots(nifty, 1e9, 1, now); n != MaxOrders*27 {
		t.Errorf("cap: %d lots, want %d", n, MaxOrders*27)
	}
	if n, _ := d.Lots(nifty, 5000, 150, now); n != 0 {
		t.Errorf("₹5000 bought %d lots", n)
	}
}

func TestLimitBuySplitsCancelsTheRestAndPlacesTheTarget(t *testing.T) {
	z := &fake{name: "zerodha", funds: 1e7}
	z.fillAt = func(o OrderReq) OrderState {
		if o.Side == "BUY" && o.Qty < 1755 { // the last, smaller order fills half and then waits
			return OrderState{Filled: 65, Average: 149.95}
		}
		return OrderState{Done: true, Filled: o.Qty, Average: o.Price}
	}
	d := desk(t, z)
	limit := 150.0
	r, err := d.Buy(context.Background(), BuyReq{Expiry: expiry.String(), Strike: 25400, Right: "CE", Limit: &limit,
		Target: 2, Capital: 600000, Brokers: []string{"zerodha"}})
	if err != nil {
		t.Fatal(err)
	}
	// 600000 buys 61 lots at 150: 3965 as 1755 + 1755 + 455
	var buys, sells []string
	for _, o := range z.placed {
		s := fmt.Sprintf("%s %s %d @ %.2f %s", o.Side, o.Type, o.Qty, o.Price, o.Product)
		if o.Side == "BUY" {
			buys = append(buys, s)
		} else {
			sells = append(sells, s)
		}
	}
	if strings.Join(buys, "; ") != "BUY LIMIT 1755 @ 150.00 MIS; BUY LIMIT 1755 @ 150.00 MIS; BUY LIMIT 455 @ 150.00 MIS" {
		t.Fatalf("buys: %v", buys)
	}
	if len(z.cancelled) != 1 || z.cancelled[0] != "3" {
		t.Errorf("cancelled %v, want the unfilled third", z.cancelled)
	}
	b := r.Trades[0]
	if b.Qty != 3575 || len(sells) != 3 || !strings.HasPrefix(sells[2], "SELL LIMIT 65 @") {
		t.Errorf("bought %+v, sells %v", b, sells)
	}
	if want, _ := d.Target(nifty, b.Entry, 3575, 2, at(10, 0)()); b.Target != want || b.Target <= 150 {
		t.Errorf("target %.2f, want %.2f", b.Target, want)
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "55 of the 61 lots") {
		t.Errorf("warnings %v", r.Warnings)
	}
}

func TestBuyOnOneBrokerStillGoesOutWhenTheOtherFails(t *testing.T) {
	u := &fake{name: "upstox", funds: 20000}
	d := desk(t, u)
	r, err := d.Buy(context.Background(), BuyReq{Expiry: expiry.String(), Strike: 25400, Right: "CE",
		Target: 2, Capital: 500000, Brokers: []string{"zerodha", "upstox"}})
	if err != nil {
		t.Fatal(err)
	}
	// At market, sized at the LTP (150) to Upstox's ₹20,000 free: 2 lots
	if len(r.Trades) != 1 || r.Trades[0].Qty != 130 || u.placed[0].Type != "MARKET" {
		t.Errorf("%+v %+v", r, u.placed)
	}
	if len(r.Errors) != 1 || !strings.HasPrefix(r.Errors[0], "Zerodha: ") {
		t.Errorf("errors %v", r.Errors)
	}
}

func TestBuyRefusals(t *testing.T) {
	z := &fake{name: "zerodha", funds: 1e7}
	limit, bad := 150.0, 150.03
	ok := BuyReq{Expiry: expiry.String(), Strike: 25400, Right: "CE", Limit: &limit, Target: 2, Capital: 500000, Brokers: []string{"zerodha"}}
	cases := []struct {
		name string
		now  func() time.Time
		edit func(r *BuyReq)
		want string
	}{
		{"before the open", at(9, 14), nil, "taken from 9:15"},
		{"after 15:20", at(15, 20), nil, "taken from 9:15"},
		{"a holiday", func() time.Time { return market.Date{Year: 2026, Month: time.November, Day: 22}.At(10, 0) }, nil, "trading day"},
		{"no broker", nil, func(r *BuyReq) { r.Brokers = nil }, "pick a broker"},
		{"unknown broker", nil, func(r *BuyReq) { r.Brokers = []string{"dhan"} }, "no broker"},
		{"off the tick", nil, func(r *BuyReq) { r.Limit = &bad }, "multiple of the tick"},
		{"no such strike", nil, func(r *BuyReq) { r.Strike = 25425 }, "no live Nifty"},
		{"too little capital", nil, func(r *BuyReq) { r.Capital = 5000 }, "doesn't buy a lot"},
	}
	for _, c := range cases {
		d := desk(t, z)
		if c.now != nil {
			d.Now = c.now
		}
		r := ok
		if c.edit != nil {
			c.edit(&r)
		}
		if _, err := d.Buy(context.Background(), r); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if len(z.placed) != 0 {
		t.Errorf("placed %v", z.placed)
	}
}

func TestExitCancelsTheContractsOrdersFirstAndSplits(t *testing.T) {
	sym := "NIFTY2026-11-2425400CE"
	z := &fake{name: "zerodha",
		positions: []Position{{Symbol: sym, Name: sym, Exchange: "NFO", Product: "MIS", Qty: 3640},
			{Symbol: sym, Name: sym, Exchange: "NFO", Product: "NRML", Qty: 65}},
		orders: []Order{{ID: "t1", Symbol: sym, Exchange: "NFO", Side: "SELL", Open: true},
			{ID: "old", Symbol: sym, Exchange: "NFO", Open: false}, {ID: "other", Symbol: "X", Exchange: "NFO", Open: true}}}
	d := desk(t, z)
	r, err := d.Exit(context.Background(), "zerodha", sym, "NFO", "MIS")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(z.cancelled) != "[t1]" {
		t.Errorf("cancelled %v", z.cancelled)
	}
	var got []string
	for _, o := range z.placed {
		got = append(got, fmt.Sprintf("%s %s %d %s", o.Side, o.Type, o.Qty, o.Product))
	}
	if strings.Join(got, ", ") != "SELL MARKET 1755 MIS, SELL MARKET 1755 MIS, SELL MARKET 130 MIS" || r.Qty != 3640 {
		t.Errorf("orders %v, result %+v", got, r)
	}
	z.positions = nil
	if _, err := d.Exit(context.Background(), "zerodha", sym, "NFO", "MIS"); err == nil || !strings.Contains(err.Error(), "no ") {
		t.Errorf("exit with nothing held: %v", err)
	}
}

func TestExitBuysBackAShort(t *testing.T) {
	z := &fake{name: "zerodha", positions: []Position{{Symbol: "S", Exchange: "NFO", Product: "NRML", Qty: -75}}}
	if _, err := desk(t, z).Exit(context.Background(), "zerodha", "S", "NFO", "NRML"); err != nil {
		t.Fatal(err)
	}
	if o := z.placed[0]; o.Side != "BUY" || o.Qty != 75 {
		t.Errorf("%+v", o)
	}
}

func TestSquareOffTakesOnlyTheDesksPositions(t *testing.T) {
	z := &fake{name: "zerodha",
		positions: []Position{{Symbol: "MINE", Exchange: "NFO", Product: "MIS", Qty: 65},
			{Symbol: "HMR", Exchange: "NFO", Product: "MIS", Qty: 65},
			{Symbol: "MINE", Exchange: "NFO", Product: "NRML", Qty: 65}},
		orders: []Order{{ID: "a", Symbol: "MINE", Tag: Tag}, {ID: "b", Symbol: "HMR", Tag: "hmr1120z01"}}}
	if err := desk(t, z).SquareOff(context.Background(), "zerodha"); err != nil {
		t.Fatal(err)
	}
	if len(z.placed) != 1 || z.placed[0].Symbol != "MINE" || z.placed[0].Product != "MIS" {
		t.Errorf("%+v", z.placed)
	}
}

func TestOverview(t *testing.T) {
	z := &fake{name: "zerodha", funds: 123456,
		positions: []Position{{Symbol: "A", Name: "A", Exchange: "NFO", Product: "MIS", Qty: 65, PnL: 500},
			{Symbol: "B", Name: "B", Exchange: "NFO", Product: "NRML", Qty: 0, PnL: -100}},
		fills: []Fill{{OrderID: "1", Time: "09:30:00", Symbol: "A", Product: "MIS", Side: "BUY", Qty: 65, Price: 100},
			{OrderID: "2", Time: "10:30:00", Symbol: "B", Product: "NRML", Side: "SELL", Qty: 65, Price: 90},
			{OrderID: "3", Time: "10:00:00", Symbol: "B", Product: "NRML", Side: "BUY", Qty: 65, Price: 91.5}},
		orders: []Order{{ID: "1", Symbol: "A", Tag: Tag, Side: "BUY"}, {ID: "9", Symbol: "A", Tag: Tag, Side: "SELL", Open: true, Price: 104.4},
			{ID: "2", Symbol: "B", Tag: "hmr1120z02"}}}
	d := desk(t, z)
	d.Now = at(15, 25)
	v, err := d.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(v.Expiries) != "[2026-11-24 2026-12-01]" {
		t.Errorf("expiries %v", v.Expiries)
	}
	if *v.Free["zerodha"].Free != 123456 || v.Free["upstox"].Error == "" || v.Trades.Errors["upstox"] == "" {
		t.Errorf("free %+v errors %v", v.Free, v.Trades.Errors)
	}
	if len(v.Positions["zerodha"].Rows) != 1 {
		t.Errorf("open positions %+v", v.Positions["zerodha"])
	}
	rows := v.Trades.Rows
	if len(rows) != 2 || rows[0].Symbol != "B" || rows[0].By != "nifty_hma_retest" || len(rows[0].Orders) != 2 {
		t.Fatalf("rows %+v", rows)
	}
	if a := rows[1]; !a.Manual || a.Target == nil || *a.Target != 104.4 || *a.Charges != 10 || *a.Net != 490 {
		t.Errorf("manual row %+v", a)
	}
	tot := v.Trades.Totals["zerodha"]
	if tot.PnL != 400 || *tot.Charges != 30 || *tot.Net != 370 || tot.Orders != 3 || tot.Priced != 3 {
		t.Errorf("totals %+v", tot)
	}
}

func TestByOrderAndUpstoxTimes(t *testing.T) {
	got := ByOrder([]Fill{{OrderID: "1", Time: "09:34:18", Qty: 65, Price: 100}, {OrderID: "1", Time: "09:34:17", Qty: 195, Price: 101},
		{OrderID: "2", Time: "10:00:00", Qty: 65, Price: 50}})
	if len(got) != 2 || got[0].Qty != 260 || got[0].Price != 100.75 || got[0].Time != "09:34:17" {
		t.Errorf("%+v", got)
	}
	// Seen on 20 Nov: placed 09:34:17, exchange_timestamp 15:04:17
	if s := UpstoxFillTime("2026-11-20 15:04:17", "2026-11-20 09:34:17"); s != "09:34:17" {
		t.Errorf("skewed: %s", s)
	}
	if s := UpstoxFillTime("2026-11-20 09:34:18", "2026-11-20 09:34:17"); s != "09:34:18" {
		t.Errorf("already IST: %s", s)
	}
	if s := UpstoxFillTime("", "2026-11-20 09:34:17"); s != "09:34:17" {
		t.Errorf("no exchange time: %s", s)
	}
}

func TestServerGuards(t *testing.T) {
	z := &fake{name: "zerodha", funds: 1}
	s := &Server{Addr: "127.0.0.1:8767", Desk: desk(t, z), UI: fstest.MapFS{"index.html": {Data: []byte("<html>manual</html>")}}}
	h := s.Handler()
	call := func(method, path, host, origin, ctype, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://"+host+path, strings.NewReader(body))
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if ctype != "" {
			r.Header.Set("Content-Type", ctype)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	const host, origin = "127.0.0.1:8767", "http://127.0.0.1:8767"
	exit := `{"broker":"zerodha","symbol":"A","exchange":"NFO","product":"MIS"}`
	for _, c := range []struct {
		name                      string
		host, origin, ctype, body string
		code                      int
	}{
		{"rebinding host", "evil.example:8767", origin, "application/json", exit, 403},
		{"foreign origin", host, "https://evil.example", "application/json", exit, 403},
		{"no origin", host, "", "application/json", exit, 403},
		{"a form post", host, origin, "text/plain", exit, 415},
	} {
		if w := call("POST", "/api/manual/exit", c.host, c.origin, c.ctype, c.body); w.Code != c.code {
			t.Errorf("%s: %d", c.name, w.Code)
		}
	}
	if len(z.placed) != 0 || len(z.cancelled) != 0 {
		t.Error("a blocked request had an effect")
	}
	w := call("POST", "/api/manual", host, origin, "application/json", "{}")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) || !strings.Contains(w.Body.String(), `"expiries"`) {
		t.Errorf("overview: %d %s", w.Code, w.Body)
	}
	w = call("POST", "/api/manual/exit", host, origin, "application/json", exit)
	if !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Errorf("exit of nothing: %s", w.Body)
	}
	if w := call("GET", "/", host, "", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "manual") || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("page: %d %s", w.Code, w.Body)
	}
}

func TestRunRefusesNonLoopback(t *testing.T) {
	s := &Server{Addr: "0.0.0.0:8767", Desk: &Desk{}}
	if err := s.Run(context.Background()); err == nil {
		t.Error("wildcard address accepted")
	}
}

func TestMain(m *testing.M) { os.Exit(m.Run()) }

// splitDesk lists 25400 CE/PE (ATM) and 25300 CE / 25500 PE (ITM): ATM at 150, ITM at 220.
func splitDesk(t *testing.T, bs ...*fake) *Desk {
	d := desk(t, bs...)
	ce, pe, itmCE, itmPE := nifty, nifty, nifty, nifty
	pe.Right, itmPE.Right = "PE", "PE"
	itmCE.Strike, itmPE.Strike = 25300, 25500
	d.Contracts = func(context.Context) ([]Contract, error) { return []Contract{ce, pe, itmCE, itmPE}, nil }
	d.LTP = func(_ context.Context, c Contract) (float64, error) {
		if c.Strike == 25400 {
			return 150, nil
		}
		return 220, nil
	}
	return d
}

func buys(f *fake) map[string]int64 {
	out := map[string]int64{}
	for _, o := range f.placed {
		if o.Side == "BUY" {
			out[o.Symbol] += o.Qty
		}
	}
	return out
}

func TestSplitAboveFiveLakhHalfATMHalfITM(t *testing.T) {
	for _, right := range []string{"CE", "PE"} {
		z := &fake{name: "zerodha", funds: 800000}
		z.fillAt = func(o OrderReq) OrderState { // market buys fill at the LTP
			if o.Side == "BUY" && strings.Contains(o.Symbol, "25400") {
				return OrderState{Done: true, Filled: o.Qty, Average: 150}
			} else if o.Side == "BUY" {
				return OrderState{Done: true, Filled: o.Qty, Average: 220}
			}
			return OrderState{Filled: 0, Average: 0} // targets rest
		}
		d := splitDesk(t, z)
		r, err := d.Buy(context.Background(), BuyReq{Expiry: expiry.String(), Strike: 25400, Right: right,
			Target: 2, Capital: 600000, Brokers: []string{"zerodha"}})
		if err != nil {
			t.Fatal(err)
		}
		itm := "25300"
		if right == "PE" {
			itm = "25500"
		}
		atmSym, itmSym := fmt.Sprintf("NIFTY%s25400%s", expiry, right), fmt.Sprintf("NIFTY%s%s%s", expiry, itm, right)
		// ₹3L each: 30 lots at 150 (₹2.93L), 20 lots at 220 (₹2.86L)
		got := buys(z)
		if len(got) != 2 || got[atmSym] != 30*65 || got[itmSym] != 20*65 {
			t.Errorf("%s: buys %v, want %s 1950 and %s 1300", right, got, atmSym, itmSym)
		}
		if len(r.Trades) != 2 || r.Trades[1].Contract != "NIFTY "+expiry.String()+" "+itm+" "+right || r.Trades[1].Target <= 220 {
			t.Errorf("%s: trades %+v", right, r.Trades)
		}
		if len(r.Warnings) != 0 {
			t.Errorf("%s: warnings %v", right, r.Warnings)
		}
		sells := 0
		for _, o := range z.placed {
			if o.Side == "SELL" {
				sells++
			}
		}
		if sells != 3 { // 1950 goes out as 1755 + 195 (the freeze), 1300 as one
			t.Errorf("%s: %d target sells, want 3", right, sells)
		}
	}
}

func TestFiveLakhOrLessBuysATMOnly(t *testing.T) {
	z := &fake{name: "zerodha", funds: 500000}
	d := splitDesk(t, z)
	r, err := d.Buy(context.Background(), BuyReq{Expiry: expiry.String(), Strike: 25400, Right: "CE",
		Target: 2, Capital: 700000, Brokers: []string{"zerodha"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := buys(z); len(got) != 1 || got[fmt.Sprintf("NIFTY%s25400CE", expiry)] != 51*65 {
		t.Errorf("buys %v, want 51 lots of 25400 CE", got)
	}
	if len(r.Trades) != 1 {
		t.Errorf("trades %+v", r.Trades)
	}
}

func TestSplitWithoutTheITMStrikeBuysATMAndWarns(t *testing.T) {
	z := &fake{name: "zerodha", funds: 800000}
	d := desk(t, z) // lists 25400 CE only
	r, err := d.Buy(context.Background(), BuyReq{Expiry: expiry.String(), Strike: 25400, Right: "CE",
		Target: 2, Capital: 600000, Brokers: []string{"zerodha"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := buys(z); len(got) != 1 || got[fmt.Sprintf("NIFTY%s25400CE", expiry)] != 61*65 {
		t.Errorf("buys %v, want all of ₹6L in 25400 CE", got)
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "ATM only: no ITM 25300 CE") {
		t.Errorf("warnings %v", r.Warnings)
	}
}

func TestSplitCarriesTheLimitOffsetToITM(t *testing.T) {
	z := &fake{name: "zerodha", funds: 1e6}
	d := splitDesk(t, z)
	limit := 148.0 // 2 under the ATM's 150, so the ITM's goes at 218
	if _, err := d.Buy(context.Background(), BuyReq{Expiry: expiry.String(), Strike: 25400, Right: "PE", Limit: &limit,
		Target: 2, Capital: 600000, Brokers: []string{"zerodha"}}); err != nil {
		t.Fatal(err)
	}
	prices := map[string]string{}
	for _, o := range z.placed {
		if o.Side == "BUY" {
			prices[o.Symbol] = fmt.Sprintf("%s %.2f", o.Type, o.Price)
		}
	}
	if prices[fmt.Sprintf("NIFTY%s25400PE", expiry)] != "LIMIT 148.00" || prices[fmt.Sprintf("NIFTY%s25500PE", expiry)] != "LIMIT 218.00" {
		t.Errorf("buy prices %v", prices)
	}
}
