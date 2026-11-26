package optbt

import (
	"math"
	"testing"

	"serene/internal/cost"
	"serene/internal/market"
)

var (
	tDay  = market.Date{Year: 2024, Month: 6, Day: 3}
	tNext = market.Date{Year: 2024, Month: 6, Day: 4}
	tExp  = market.Date{Year: 2024, Month: 6, Day: 6}
)

type fixture struct{ d, next *Day }

// newFixture: NIFTY at 22000 all day; every listed strike 21000–23000 trades
// flat at a price falling 10 per 50 points out of the money (ATM = 100).
func newFixture() *fixture {
	f := &fixture{newDay(tDay, indices["NIFTY"]), newDay(tNext, indices["NIFTY"])}
	for i, d := range []*Day{f.d, f.next} {
		d.Ord = i
		for s := 0; s < 375; s++ {
			px := market.Rupees(22000)
			d.put(market.Instrument{}, s, market.Bar{Open: px, High: px, Low: px, Close: px})
		}
		for k := -20; k <= 20; k++ {
			strike := market.Rupees(22000 + 50*float64(k))
			for _, r := range []market.Right{market.Call, market.Put} {
				otm := k
				if r == market.Put {
					otm = -k
				}
				p := max(100-10*float64(otm), 1)
				if otm < 0 {
					p = 100 - 50*float64(otm)
				}
				f.flat(d, strike, r, p, 0, 375)
			}
		}
		d.finish(map[market.Date]int{tDay: 0, tNext: 1, tExp: 3})
	}
	return f
}

func (f *fixture) flat(d *Day, strike market.Paise, r market.Right, p float64, from, to int) {
	px := market.Rupees(p)
	for s := from; s < to; s++ {
		d.put(market.Option(market.NFO, "NIFTY", tExp, strike, r, 25), s, market.Bar{Open: px, High: px, Low: px, Close: px})
	}
}

func (f *fixture) bar(d *Day, strike float64, r market.Right, s int, o, h, l, c float64) {
	d.put(market.Option(market.NFO, "NIFTY", tExp, market.Rupees(strike), r, 25), s,
		market.Bar{Open: market.Rupees(o), High: market.Rupees(h), Low: market.Rupees(l), Close: market.Rupees(c)})
}

func baseCell() Cell {
	return Cell{Index: "NIFTY", DTE: -1, Side: "both", Strike: Rule{Name: "atm", Kind: KindATM},
		Entry: 5, Exit: 360, SL: 25, SLMode: "leg", ReentryMode: "cost"}
}

func run(t *testing.T, f *fixture, c Cell) DayResult {
	t.Helper()
	e := &Env{Costs: cost.Zero(), SlipBps: 0, NakedPct: 9, Detail: true}
	r, ok := e.Run(&c, f.d, f.next)
	if !ok {
		t.Fatal("no trade")
	}
	return r
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestStopModes(t *testing.T) {
	setup := func() *fixture {
		f := newFixture()
		f.bar(f.d, 22000, market.Call, 10, 100, 130, 100, 110) // CE stop 125, fills at 125
		f.flat(f.d, market.Rupees(22000), market.Put, 80, 15, 375)
		f.bar(f.d, 22000, market.Put, 20, 80, 101, 80, 80) // above cost, below 25%
		return f
	}
	for _, tc := range []struct {
		mode string
		want float64
	}{
		{"leg", (-25 + 20) * 25}, // PE rides to 80
		{"cost", (-25 + 0) * 25}, // PE stop moved to 100, hit at slot 20
		{"all", (-25 + 0) * 25},  // PE squared off at its slot-10 close of 100
	} {
		f := setup()
		c := baseCell()
		c.SLMode = tc.mode
		r := run(t, f, c)
		want := tc.want
		if !near(r.Gross, want) {
			t.Errorf("%s: gross %v, want %v (trades %+v)", tc.mode, r.Gross, want, r.Trades)
		}
		if !r.CESL || r.PESL != (tc.mode == "cost") {
			t.Errorf("%s: ce/pe sl %v %v", tc.mode, r.CESL, r.PESL)
		}
	}
}

func TestWaitAndTrade(t *testing.T) {
	f := newFixture()
	f.bar(f.d, 22000, market.Call, 30, 95, 95, 90, 92) // −10% trigger at 90
	f.flat(f.d, market.Rupees(22000), market.Call, 92, 31, 375)
	c := baseCell()
	c.Wait = -10
	c.Side = "ce"
	r := run(t, f, c)
	if len(r.Trades) != 1 || r.Trades[0].Entry != market.Rupees(90) || r.Trades[0].Exit != market.Rupees(92) {
		t.Fatalf("trades %+v", r.Trades)
	}
	// never triggers: no trade
	f = newFixture()
	e := &Env{Costs: cost.Zero(), NakedPct: 9}
	if _, ok := e.Run(&c, f.d, f.next); ok {
		t.Fatal("traded without the trigger")
	}
}

func TestTrail(t *testing.T) {
	f := newFixture()
	c := baseCell()
	c.Side = "ce"
	c.TrailX, c.TrailY = 10, 5
	f.flat(f.d, market.Rupees(22000), market.Call, 80, 20, 375) // −20%: stop 125 → 115
	f.bar(f.d, 22000, market.Call, 40, 80, 116, 80, 80)
	r := run(t, f, c)
	if r.Trades[0].Exit != market.Rupees(115) || r.Trades[0].Reason != "sl" {
		t.Fatalf("trades %+v", r.Trades)
	}
	c.TrailX = 0 // without the trail the 116 high does nothing
	r = run(t, f, c)
	if r.Trades[0].Exit != market.Rupees(80) {
		t.Fatalf("no trail: %+v", r.Trades)
	}
}

func TestReentry(t *testing.T) {
	f := newFixture()
	f.flat(f.d, market.Rupees(22000), market.Call, 130, 10, 50) // stopped at 125 on slot 10
	f.bar(f.d, 22000, market.Call, 50, 110, 110, 99, 100)       // back to the 100 entry
	f.flat(f.d, market.Rupees(22000), market.Call, 90, 51, 375)
	c := baseCell()
	c.Side = "ce"
	c.Reentry = 1
	r := run(t, f, c)
	if len(r.Trades) != 2 || r.Reentries != 1 {
		t.Fatalf("trades %+v", r.Trades)
	}
	if tr := r.Trades[1]; tr.Entry != market.Rupees(100) || tr.Exit != market.Rupees(90) {
		t.Fatalf("re-entry trade %+v", tr)
	}
	// re-execute: the same ATM strike sold again at the next bar's open, 130
	c.ReentryMode = "reexec"
	r = run(t, f, c)
	if len(r.Trades) != 2 || r.Trades[1].Entry != market.Rupees(130) || r.Trades[1].Exit != market.Rupees(90) {
		t.Fatalf("reexec trades %+v", r.Trades)
	}
}

func TestHedgeAndMargin(t *testing.T) {
	f := newFixture()
	c := baseCell()
	r := run(t, f, c)
	if !near(r.Margin, 0.09*22000*25) {
		t.Fatalf("naked margin %v", r.Margin)
	}
	c.Hedge = Rule{Name: "otm500", Kind: KindOTM, V: 500}
	f.bar(f.d, 22000, market.Call, 10, 100, 130, 100, 110)
	f.flat(f.d, market.Rupees(22500), market.Call, 4, 10, 375) // CE wing, bought at 1
	r = run(t, f, c)
	if !near(r.Margin, 500*25) {
		t.Fatalf("hedged margin %v", r.Margin)
	}
	var wings int
	for _, tr := range r.Trades {
		if tr.Hedge {
			wings++
		}
	}
	if wings != 2 {
		t.Fatalf("trades %+v", r.Trades)
	}
	// CE −25, CE wing 1→4 (+3), PE 0, PE wing 0
	if !near(r.Gross, (-25+3)*25) {
		t.Fatalf("gross %v trades %+v", r.Gross, r.Trades)
	}
}

func TestSTBTGap(t *testing.T) {
	f := newFixture()
	c := baseCell()
	c.Side = "ce"
	c.STBT = true
	c.Exit = 5
	f.flat(f.next, market.Rupees(22000), market.Call, 150, 0, 375) // gaps through the 125 stop
	r := run(t, f, c)
	if tr := r.Trades[0]; tr.Exit != market.Rupees(150) || tr.Reason != "sl" || market.DateOf(tr.ExitT) != tNext {
		t.Fatalf("trades %+v", r.Trades)
	}
	// STBT carries to the expiry after today, which is still tExp
	if r.Expiry != tExp {
		t.Fatalf("expiry %v", r.Expiry)
	}
}

func TestDTE(t *testing.T) {
	f := newFixture()
	if f.d.DTE != 3 {
		t.Fatalf("dte %d", f.d.DTE)
	}
	c := baseCell()
	c.DTE = 0
	e := &Env{Costs: cost.Zero(), NakedPct: 9}
	if _, ok := e.Run(&c, f.d, f.next); ok {
		t.Fatal("dte 0 cell traded on a dte 3 day")
	}
}

func TestPicks(t *testing.T) {
	f := newFixture()
	f.d.Index.o[5] = market.Rupees(22030)
	for _, tc := range []struct {
		rule  string
		round int
		right market.Right
		want  float64
	}{
		{"atm", 0, market.Call, 22050},
		{"atm", 100, market.Call, 22000},
		{"otm200", 0, market.Call, 22250},
		{"otm200", 0, market.Put, 21850},
		{"otm200", 500, market.Put, 22000},
		{"atmpct1", 0, market.Call, 22250},
		{"cp50", 0, market.Call, 22250},   // fixture CE prices fall 10 per 50 points above 22000
		{"cp50", 100, market.Call, 22200}, // 60 at 22200 vs 40 at 22300: first closest
		{"sw1", 0, market.Call, 22300},    // ATM 22050 straddle 90 + 150 = 240 → 22290 → 22300
		{"cpsp25", 0, market.Put, 21800},  // 25% of 240 = 60 → PE at 21800
	} {
		r, err := ParseRule(tc.rule)
		if err != nil {
			t.Fatal(err)
		}
		o := f.d.pick(r, tc.round, tc.right, tExp, 5)
		if o == nil || o.inst.Strike != market.Rupees(tc.want) {
			t.Errorf("%s r%d %s: got %v, want %v", tc.rule, tc.round, tc.right, o, tc.want)
		}
	}
}

func TestExpand(t *testing.T) {
	cfg := &Config{
		Index: []string{"NIFTY"}, DTE: []int{0, 1}, Strike: []string{"atm", "cp100"},
		Entry: []string{"09:20", "15:20"}, Exit: []string{"15:15"}, Hold: []string{"intraday", "stbt"},
		SL: []float64{0, 25}, SLMode: []string{"leg", "cost"}, Reentry: []int{0, 1},
		ReentryMode: []string{"cost", "reexec"}, Hedge: []string{"none", "otm500"},
	}
	cfg.defaults()
	cells, err := cfg.Expand()
	if err != nil {
		t.Fatal(err)
	}
	// dte 2 × strike 2 × (entry 09:20: intraday+stbt, 15:20: stbt only = 3)
	// × (sl0: 1 + sl25: 2 modes × (re0 + re1×2) = 6 → 7) × hedge 2
	if want := 2 * 2 * 3 * 7 * 2; len(cells) != want {
		t.Fatalf("%d cells, want %d", len(cells), want)
	}
	seen := map[string]bool{}
	for i, c := range cells {
		if c.ID != i || seen[c.Name()] {
			t.Fatalf("cell %d %s", i, c.Name())
		}
		seen[c.Name()] = true
	}
}

func TestAccDrawdown(t *testing.T) {
	var a Acc
	xs := []float64{100, -50, -80, 200, -300, 50, 400}
	for i, x := range xs {
		a.Add(&DayResult{Date: tDay, Ord: i, NetSlip: x, Margin: 1000})
	}
	s := a.Summary()
	// equity 100 50 −30 170 −130 −80 320: mdd 300 from 170 (ord 3) to −130 (ord 4), back at ord 6
	if s.MDD != 300 || s.MDDDays != 1 || s.RecDays != 2 {
		t.Fatalf("mdd %v days %d rec %d", s.MDD, s.MDDDays, s.RecDays)
	}
	// % on an account starting at 1000: max of 130/1100, 300/1170
	if !near(s.MDDPct, 300.0/1170*100) {
		t.Fatalf("mdd%% %v", s.MDDPct)
	}
	if s.MaxLossRun != 2 || s.MaxWinRun != 2 || !near(s.Win, 4.0/7*100) {
		t.Fatalf("runs %d %d win %v", s.MaxWinRun, s.MaxLossRun, s.Win)
	}
}

func TestSkipEvents(t *testing.T) {
	f := newFixture()
	c := baseCell()
	c.Skip, c.skipMask = "india", skipSets["india"]
	e := &Env{Costs: cost.Zero(), NakedPct: 9}
	if _, ok := e.Run(&c, f.d, f.next); !ok {
		t.Fatal("no event, should trade")
	}
	f.next.Events = evRBI
	if _, ok := e.Run(&c, f.d, f.next); !ok {
		t.Fatal("intraday cell skipped for tomorrow's event")
	}
	c.STBT, c.Exit = true, 5
	if _, ok := e.Run(&c, f.d, f.next); ok {
		t.Fatal("STBT held into an RBI day")
	}
	f.next.Events = evFOMC
	if _, ok := e.Run(&c, f.d, f.next); !ok {
		t.Fatal("india set skipped an FOMC day")
	}
}

func TestLoadEvents(t *testing.T) {
	all := []market.Date{{Year: 2024, Month: 6, Day: 3}, {Year: 2024, Month: 6, Day: 4}, {Year: 2024, Month: 6, Day: 6}}
	ev, err := loadEvents("../../configs/events.csv", all)
	if err != nil {
		t.Fatal(err)
	}
	if ev[all[0]] != evElection || ev[all[1]] != evElection {
		t.Fatalf("election days %v", ev)
	}
	// 2024-06-07 RBI is after the last listed day, so it lands nowhere
	if ev[all[2]] != 0 {
		t.Fatalf("06-06 %v", ev[all[2]])
	}
}
