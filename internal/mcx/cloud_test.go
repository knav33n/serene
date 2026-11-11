package mcx

import (
	"math"
	"testing"
	"time"

	"serene/internal/cost"
	"serene/internal/market"
)

// steady makes n bars each moving step from the last close, with no wick
// against the move.
func steady(n int, start, step float64) []market.Bar {
	var out []market.Bar
	t := day1.At(9, 0)
	c := start
	for i := 0; i < n; i++ {
		o := c
		c += step
		out = append(out, market.Bar{Time: t, End: t.Add(time.Hour), Open: market.Rupees(o), Close: market.Rupees(c),
			High: market.Rupees(math.Max(o, c)), Low: market.Rupees(math.Min(o, c))})
		t = t.Add(time.Hour)
	}
	return out
}

func TestCloudReadingsTrend(t *testing.T) {
	up := Cloud(steady(120, 1000, 1)).Readings[119]
	// RSI sits at 100 with no losses, so the momentum readings are level.
	want := Reading{LongTrend: 1, ShortTrend: 1, HeikinAshi: 1, Clouds: 1}
	if up != want {
		t.Errorf("rising %v, want %v", up, want)
	}
	down := Cloud(steady(120, 1000, -1)).Readings[119]
	if down != (Reading{LongTrend: -1, ShortTrend: -1, HeikinAshi: -1, Clouds: -1}) {
		t.Errorf("falling %v", down)
	}
	if b, _ := up.Count(); b != 4 || up.Bias(4) != 1 || up.Bias(5) != 0 || down.Bias(4) != -1 {
		t.Errorf("bias of %v", up)
	}
}

func TestCloudReadingsMomentumTurn(t *testing.T) {
	bars := append(steady(120, 1000, 1), steady(1, 1120, -1)...)
	bars[120].Time, bars[120].End = bars[119].End, bars[119].End.Add(time.Hour)
	r := Cloud(bars).Readings[120]
	if r[STMomentum] != -1 || r[MTMomentum] != -1 || r[LTMomentum] != -1 {
		t.Errorf("one down bar after a run: %v", r)
	}
	if r[HeikinAshi] != 1 {
		t.Errorf("HA lags: one small down bar after a run is still green: %v", r)
	}
}

func TestCloudReadingsWarmup(t *testing.T) {
	r := Cloud(steady(10, 1000, 1)).Readings[9]
	if r[LongTrend] != 0 || r[Clouds] != 0 {
		t.Errorf("EMA74 isn't known after 10 bars: %v", r)
	}
}

// wave is several days of minute bars rising in swings, an 8-hour cycle on
// an upward drift, then falling the same way.
func wave(days int) []market.Bar {
	var out []market.Bar
	n := 0
	for d := 0; d < days; d++ {
		day := day1.AddDays(d)
		for t := day.At(9, 0); t.Before(day.At(23, 0)); t = t.Add(time.Minute) {
			drift := 0.05 * float64(n)
			if d >= days/2 {
				drift = 0.05*float64(n0(days)) - 0.05*float64(n-n0(days))
			}
			c := 1000 + drift + 30*math.Sin(2*math.Pi*float64(n)/480)
			out = append(out, market.Bar{Key: keyA, Time: t, End: t.Add(time.Minute), Open: market.Rupees(c),
				High: market.Rupees(c + 0.5), Low: market.Rupees(c - 0.5), Close: market.Rupees(c), Volume: 1})
			n++
		}
	}
	return out
}

func n0(days int) int { return days / 2 * 14 * 60 }

func cloudConfig(exit string) CloudConfig {
	return CloudConfig{Name: "CRUDEOIL", Spec: Spec{Multiplier: 10, Tick: 5}, N: 5, Exit: exit,
		Lookback: 30 * 24 * time.Hour, Multiple: 2.5, Costs: cost.Zero(), From: day1, To: day1.AddDays(60)}
}

func TestCloudBacktestProperties(t *testing.T) {
	bars := wave(40)
	cd := PrepareCloud(bars)
	H := cd.sr.frames[entryFrame][keyA]
	ind := cd.ind[entryFrame][keyA]
	hourAt := map[time.Time]int{}
	for i, b := range H {
		hourAt[b.End] = i
	}
	for _, exit := range ExitRules {
		cfg := cloudConfig(exit)
		trades, err := CloudBacktest(cd, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if len(trades) == 0 {
			t.Errorf("%s: no trades", exit)
		}
		var long, short int
		for _, tr := range trades {
			i, ok := hourAt[tr.Signal]
			if !ok {
				t.Fatalf("%s: signal %s isn't a 1H close", exit, tr.Signal)
			}
			if ind.Readings[i].Bias(cfg.N) != int(tr.Side) {
				t.Errorf("%s: %v entered against the bias %v", exit, tr, ind.Readings[i])
			}
			if !tr.Entry.After(tr.Signal) && !tr.Entry.Equal(tr.Signal) {
				t.Errorf("%s: entry before signal %v", exit, tr)
			}
			switch tr.Reason {
			case "stop", "end", "roll":
			case "target":
				if exit != Exit2R {
					t.Errorf("%s: target exit", exit)
				}
			default:
				if tr.Reason != exit || exit == ExitSwing30 || exit == Exit2R {
					t.Errorf("%s: reason %s", exit, tr.Reason)
				}
			}
			if tr.Side == market.Buy {
				long++
			} else {
				short++
			}
		}
		if long == 0 || short == 0 {
			t.Errorf("%s: %d long, %d short: the wave rises then falls", exit, long, short)
		}
	}
}

func TestCloudBacktestNoLookahead(t *testing.T) {
	bars := wave(40)
	cut := day1.AddDays(25).At(0, 0)
	var head []market.Bar
	for _, b := range bars {
		if b.Time.Before(cut) {
			head = append(head, b)
		}
	}
	a, err := CloudBacktest(PrepareCloud(bars), cloudConfig(ExitVote30))
	if err != nil {
		t.Fatal(err)
	}
	b, err := CloudBacktest(PrepareCloud(head), cloudConfig(ExitVote30))
	if err != nil {
		t.Fatal(err)
	}
	for i, tr := range b {
		if !tr.Exit.Before(cut.Add(-time.Hour)) {
			break
		}
		if i >= len(a) || a[i] != tr {
			t.Fatalf("trade %d differs once later data exists:\n%v\n%v", i, tr, a[i])
		}
	}
}

func TestCloudNineFilter(t *testing.T) {
	cd := PrepareCloud(wave(40))
	H := cd.sr.frames[entryFrame][keyA]
	atr := cd.ind[entryFrame][keyA].ATR14
	cfg := cloudConfig(ExitVote30)
	cfg.NineK = 0.0001 // any move their way at all
	trades, err := CloudBacktest(cd, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trades {
		if tr.Entry.Hour() != 10 {
			continue
		}
		for i, b := range H {
			if b.Time.Equal(market.DateOf(tr.Entry).At(9, 0)) {
				mv := (b.Close - b.Open).Rupees() * float64(tr.Side)
				if mv > cfg.NineK*atr[i] {
					t.Errorf("entered at 10 after a 9:00 move of %.1f its way: %v", mv, tr)
				}
			}
		}
	}
}
