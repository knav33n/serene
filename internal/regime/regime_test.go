package regime

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"serene/internal/market"
)

// build makes a panel of n weekday-ish dates from price functions.
func build(n int, series map[string]func(i int) float64, meta map[string]Meta) *Panel {
	start := market.Date{Year: 2020, Month: 1, Day: 1}
	var rows []market.IndexDay
	for name, f := range series {
		for i := range n {
			v := f(i)
			if math.IsNaN(v) {
				continue
			}
			rows = append(rows, market.IndexDay{Date: start.AddDays(i), Name: name, Source: name,
				Close: market.Rupees(v), Volume: 100, PE: 20 + float64(i%10), PB: 3})
		}
	}
	return NewPanel(rows, func(n string) Meta { return meta[n] })
}

func flat(v float64) func(int) float64 { return func(int) float64 { return v } }

func grow(start, pct float64) func(int) float64 {
	return func(i int) float64 { return start * math.Pow(1+pct/100, float64(i)) }
}

func sectorMeta(names ...string) map[string]Meta {
	m := map[string]Meta{}
	for _, n := range names {
		m[n] = Meta{Group: "sectoral", Breadth: true}
	}
	return m
}

func allMarket(f func(int) float64, small func(int) float64, beta func(int) float64, sectors func(int) float64, vix float64) map[string]func(int) float64 {
	s := map[string]func(int) float64{Market: f, Large: flat(1000), Small250: small, HighBeta: beta, LowVol: flat(1000), VIX: flat(vix)}
	for i := range 6 {
		s[fmt.Sprintf("S%d", i)] = sectors
	}
	return s
}

func TestSMA(t *testing.T) {
	got := sma([]float64{math.NaN(), 1, 2, 3, 4}, 2)
	if !math.IsNaN(got[1]) || got[2] != 1.5 || got[4] != 3.5 {
		t.Errorf("%v", got)
	}
}

func TestDrawdownUsesWindow(t *testing.T) {
	got := drawdown([]float64{200, 100, 110, 100, 100}, 3)
	if got[1] != -50 {
		t.Errorf("%v", got)
	}
	// The 200 has left the 3-day window by index 3, so the high is 110.
	if want := (100.0/110 - 1) * 100; math.Abs(got[3]-want) > 1e-9 {
		t.Errorf("%v", got)
	}
}

func TestSteadyBull(t *testing.T) {
	meta := sectorMeta("S0", "S1", "S2", "S3", "S4", "S5")
	series := allMarket(grow(1000, 0.1), grow(1000, 0.1), grow(1000, 0.1), grow(1000, 0.1), 12)
	series[VIX] = grow(14, -0.01) // calm, and below its own average
	days := Timeline(build(400, series, meta))
	if len(days) == 0 {
		t.Fatal("no days")
	}
	last := days[len(days)-1]
	if last.Score != 7 || last.State != Bull || last.Breadth != 100 || last.Sectors != 6 {
		t.Errorf("%+v", last)
	}
}

func TestBearConfirmsAfterFiveCloses(t *testing.T) {
	// 300 days of rise, then a steady fall.
	turn := 300
	f := func(i int) float64 {
		if i < turn {
			return 1000 * math.Pow(1.001, float64(i))
		}
		return 1000 * math.Pow(1.001, float64(turn)) * math.Pow(0.99, float64(i-turn))
	}
	meta := sectorMeta("S0", "S1", "S2", "S3", "S4", "S5")
	p := build(420, allMarket(f, f, f, f, 30), meta)
	days := Timeline(p)
	last := days[len(days)-1]
	if last.State != Bear {
		t.Fatalf("expected Bear: %+v", last)
	}
	// The state must flip exactly ConfirmDays after the score first crossed.
	crossed := -1
	for i, d := range days {
		if d.Score <= BearScore && crossed < 0 {
			crossed = i
		}
		if crossed < 0 {
			continue
		}
		if i-crossed < ConfirmDays-1 {
			if d.State == Bear {
				t.Errorf("confirmed early at day %d", i-crossed)
			}
			if d.Target != Bear || d.Pending != i-crossed+1 {
				t.Errorf("pending: %+v", d)
			}
		}
		if i-crossed == ConfirmDays-1 && d.State != Bear {
			t.Errorf("not confirmed on the fifth close: %+v", d)
		}
	}
	tr := Transitions(days)
	if len(tr) == 0 || tr[len(tr)-1].To != Bear {
		t.Errorf("%+v", tr)
	}
	if last.Direction != "" && last.Direction != "weakening" {
		t.Errorf("direction %q", last.Direction)
	}
}

func TestShortSpikeDoesNotChangeState(t *testing.T) {
	meta := sectorMeta("S0", "S1", "S2", "S3", "S4", "S5")
	// Bull throughout, except four days of panic.
	vix := func(i int) float64 {
		if i >= 350 && i < 354 {
			return 50
		}
		return 12
	}
	f := func(i int) float64 {
		v := 1000 * math.Pow(1.001, float64(i))
		if i >= 350 && i < 354 {
			return v * 0.5
		}
		return v
	}
	s := allMarket(f, f, f, f, 12)
	s[VIX] = vix
	days := Timeline(build(400, s, meta))
	for _, d := range days[len(days)-100:] {
		if d.State == Bear {
			t.Fatalf("four bad days flipped the state: %+v", d)
		}
	}
}

func TestMissingInputsScoreZero(t *testing.T) {
	// Only the market index exists, so most checks are unavailable.
	days := Timeline(build(300, map[string]func(int) float64{Market: grow(1000, 0.1)}, nil))
	last := days[len(days)-1]
	for _, c := range last.Checks {
		switch c.Name {
		case "Long trend", "Medium trend", "Drawdown":
			if !c.Available {
				t.Errorf("%s should be available", c.Name)
			}
		default:
			if c.Available || c.Score != 0 {
				t.Errorf("%s: %+v", c.Name, c)
			}
		}
	}
	if !math.IsNaN(last.Breadth) {
		t.Errorf("breadth %v", last.Breadth)
	}
	if last.Score != 3 {
		t.Errorf("score %d", last.Score)
	}
}

func TestNoMarketIndex(t *testing.T) {
	if Timeline(build(10, map[string]func(int) float64{"X": flat(1)}, nil)) != nil {
		t.Error("want nil")
	}
}

func TestSmallcapFallback(t *testing.T) {
	s := map[string]func(int) float64{Market: grow(1000, 0.1), Large: flat(1000), Small100: grow(1000, 0.1),
		Small250: func(i int) float64 {
			if i < 250 {
				return math.NaN()
			}
			return 500
		}}
	days := Timeline(build(300, s, nil))
	last := days[len(days)-1]
	if c := last.Checks[4]; !c.Available || c.Score != 1 {
		t.Errorf("should fall back to Smallcap 100: %+v", c)
	}
}

func TestCardBarsKeepTheirProportions(t *testing.T) {
	rows := []Row{{Name: "Nifty A", Change: []float64{3.2}}, {Name: "Nifty B", Change: []float64{-1.6}}}
	bars := cardBars(rows, 3.2)
	if bars[0].Width != 50 || bars[1].Width != 25 || bars[1].Up {
		t.Errorf("the largest move fills half the track and the rest scale to it: %+v", bars)
	}
}

func TestDrawdownLabel(t *testing.T) {
	for dd, want := range map[float64]string{-5: "", -10: "Correction", -19.9: "Correction", -20: "Bear market", -40: "Bear market"} {
		if got := DrawdownLabel(dd); got != want {
			t.Errorf("%v: %q", dd, got)
		}
	}
}

func TestSectors(t *testing.T) {
	meta := map[string]Meta{"Up": {Group: "sectoral"}, "Down": {Group: "sectoral"}, "New": {Group: "sectoral"},
		"Hidden": {Group: "thematic"}, Market: {Group: "broad"}}
	spike := func(i int) float64 {
		v := 1000 * math.Pow(1.001, float64(i))
		if i >= 299 {
			return v * 1.2
		}
		if i%2 == 0 {
			return v * 1.002
		}
		return v
	}
	s := map[string]func(int) float64{Market: flat(1000), "Up": spike, "Down": func(i int) float64 { return 1000 * math.Pow(0.999, float64(i)) * (1 + 0.003*float64(i%2)) }, "Hidden": flat(1),
		"New": func(i int) float64 {
			if i < 295 {
				return math.NaN()
			}
			return 100 + float64(i)
		}}
	p := build(300, s, meta)
	rows := Sectors(p, 299, "sectoral")
	if len(rows) != 3 {
		t.Fatalf("%+v", rows)
	}
	by := map[string]Row{}
	for _, r := range rows {
		by[r.Name] = r
	}
	if rows[0].Name != "Up" {
		t.Errorf("best mover first: %s", rows[0].Name)
	}
	up := by["Up"]
	if !up.Unusual || up.Quadrant != Leading || !up.Above200 || !up.Has200 {
		t.Errorf("Up: %+v", up)
	}
	if d := by["Down"]; d.Quadrant != Lagging || d.Above200 || d.Unusual {
		t.Errorf("Down: %+v", d)
	}
	n := by["New"]
	if n.Has200 || n.Quadrant != "" || !math.IsNaN(n.Change[5]) || math.IsNaN(n.Change[0]) {
		t.Errorf("a new index has no long history: %+v", n)
	}
}

func TestQuadrants(t *testing.T) {
	for _, tc := range []struct {
		t, m float64
		want string
	}{{1, 1, Leading}, {1, -1, Weakening}, {-1, -1, Lagging}, {-1, 1, Improving}, {math.NaN(), 1, ""}} {
		if got := quadrant(tc.t, tc.m); got != tc.want {
			t.Errorf("%v %v: %q", tc.t, tc.m, got)
		}
	}
}

func TestPEPercentile(t *testing.T) {
	p := build(400, map[string]func(int) float64{Market: flat(1000)}, nil)
	pct, pe := PEPercentile(p, Market, 0, 399)
	if math.IsNaN(pct) || pe != 29 || pct != 100 {
		t.Errorf("%v %v", pct, pe)
	}
	if pct, _ := PEPercentile(p, Market, 300, 399); !math.IsNaN(pct) {
		t.Error("too little history should give NaN")
	}
}

func TestReviewAndStats(t *testing.T) {
	d := func(day int) market.Date { return market.Date{Year: 2020, Month: 1, Day: 1}.AddDays(day) }
	st := func(i int, c float64, s State) Day { return Day{Date: d(i), Close: c, State: s} }
	days := []Day{st(0, 100, Bull), st(1, 110, Bull), st(2, 104, Neutral), st(3, 88, Bear), st(4, 77, Bear),
		st(5, 90, Neutral), st(6, 100, Bull)}
	e := Review(days, []Window{{"fall", d(0), d(5)}})[0]
	if e.Peak != d(1) || e.Trough != d(4) || math.Abs(e.Fall+30) > 1e-9 {
		t.Errorf("%+v", e)
	}
	if e.LeftBull != d(2) || e.EnteredBear != d(3) || math.Abs(e.BearDD+20) > 1e-9 {
		t.Errorf("%+v", e)
	}
	if e.Bull != d(6) || e.SinceTrough != 2 || math.Abs(e.BullGain-(100.0/77-1)*100) > 1e-9 {
		t.Errorf("%+v", e)
	}
	stats := Stats(days, 1)
	if stats[0].State != Bull || stats[0].Days != 3 {
		t.Errorf("%+v", stats[0])
	}
	// Bear days 3 and 4 are followed by -12.5% and +16.9%.
	if b := stats[2]; b.Days != 2 || b.Positive != 50 || math.Abs(b.WorstNext+12.5) > 1e-9 {
		t.Errorf("%+v", b)
	}
}

func TestReportRenders(t *testing.T) {
	meta := sectorMeta("S0", "S1", "S2", "S3", "S4", "S5")
	meta[Market] = Meta{Group: "broad"}
	f := func(i int) float64 { return 1000 * math.Pow(1.001, float64(i)) * (1 + 0.01*math.Sin(float64(i))) }
	p := build(420, allMarket(f, f, f, f, 12), meta)
	last := p.Dates[len(p.Dates)-1]
	s, err := Build(p, last, market.Date{}, "sectoral", "broad")
	if err != nil {
		t.Fatal(err)
	}
	s.Warnings = []string{"New <index>"}
	out := filepath.Join(t.TempDir(), "r.html")
	if err := WriteReport(out, s); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	html := string(b)
	for _, want := range []string{"S0", "Nifty 500", "&lt;index&gt;", "const data = {"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(html, "NaN") {
		t.Error("NaN reached the page")
	}
	card := filepath.Join(t.TempDir(), "card.html")
	if err := WriteCard(card, s); err != nil {
		t.Fatal(err)
	}
	c, _ := os.ReadFile(card)
	for _, want := range []string{"Regime checks", "Sectors today", "S0"} {
		if !strings.Contains(string(c), want) {
			t.Errorf("card is missing %q", want)
		}
	}
	if strings.Contains(string(c), "NaN") || strings.Contains(string(c), "Inf") {
		t.Error("a bad number reached the card")
	}
	if !strings.Contains(s.Brief(), "Regime:") {
		t.Error(s.Brief())
	}
	if !strings.Contains(s.Summary(), "Regime:") {
		t.Error(s.Summary())
	}
	if _, err := Build(p, market.Date{Year: 2000, Month: 1, Day: 1}, market.Date{}, "sectoral", "broad"); err == nil {
		t.Error("a date before the data must fail")
	}
}

func TestLongGapRestartsSeries(t *testing.T) {
	// A lone back-calculated row, then real history from day 100. Short gaps
	// within the real history still carry forward.
	p := build(150, map[string]func(int) float64{
		"Anchor": flat(1),
		"New": func(i int) float64 {
			switch {
			case i == 5:
				return 50
			case i < 100, i == 120:
				return math.NaN()
			}
			return float64(i)
		},
	}, nil)
	s := p.Series["New"]
	if s.First != 100 || !math.IsNaN(s.Close[5]) || !math.IsNaN(s.Close[99]) {
		t.Errorf("first %d close[5] %v close[99] %v", s.First, s.Close[5], s.Close[99])
	}
	if s.Close[120] != 119 {
		t.Errorf("a short gap should carry forward: %v", s.Close[120])
	}
}

func TestRebaseScalesHistory(t *testing.T) {
	start := market.Date{Year: 2020, Month: 1, Day: 1}
	row := func(i int, name string, cl, prev float64) market.IndexDay {
		return market.IndexDay{Date: start.AddDays(i), Name: name, Close: market.Rupees(cl), PrevClose: market.Rupees(prev)}
	}
	rows := []market.IndexDay{row(0, "X", 100, 99), row(1, "X", 101, 100), row(2, "X", 204, 202), row(3, "X", 206, 204)}
	meta := func(string) Meta { return Meta{Group: "strategy"} }
	s := NewPanel(rows, meta).Series["X"]
	if len(s.Rebases) != 1 || s.Rebases[0].Date != start.AddDays(2) {
		t.Fatalf("%+v", s.Rebases)
	}
	if s.Close[3] != 206 || s.Close[2] != 204 || math.Abs(s.Close[1]-202) > 1e-9 || math.Abs(s.Close[0]-200) > 1e-9 {
		t.Errorf("%v", s.Close)
	}
	// VIX's published previous close isn't a rebase signal.
	v := NewPanel(rows, func(string) Meta { return Meta{Group: "other"} }).Series["X"]
	if len(v.Rebases) != 0 || v.Close[0] != 100 {
		t.Errorf("%+v", v)
	}
}

func TestDesiredHasHysteresis(t *testing.T) {
	for _, tc := range []struct {
		from  State
		score int
		want  State
	}{
		{Neutral, 3, Neutral}, {Neutral, 4, Bull}, {Neutral, -3, Neutral}, {Neutral, -4, Bear},
		{Bull, 2, Bull}, {Bull, 1, Neutral}, {Bull, -4, Bear},
		{Bear, -2, Bear}, {Bear, -1, Neutral}, {Bear, 4, Bull},
	} {
		if got := desired(tc.from, tc.score); got != tc.want {
			t.Errorf("%s at %d: %s", tc.from, tc.score, got)
		}
	}
}
