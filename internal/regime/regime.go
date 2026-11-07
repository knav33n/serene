package regime

import (
	"fmt"
	"math"

	"calm/internal/market"
)

// State is the market regime.
type State string

const (
	Bull    State = "Bull"
	Neutral State = "Neutral"
	Bear    State = "Bear"
)

// Index names the rules depend on.
const (
	Market   = "Nifty 500"
	VIX      = "India VIX"
	Large    = "Nifty 100"
	HighBeta = "Nifty High Beta 50"
	LowVol   = "Nifty Low Volatility 50"
	Small250 = "Nifty Smallcap 250"
	Small100 = "NIFTY Smallcap 100" // stands in before Smallcap 250 has history
)

// The rules. These are starting values chosen before looking at results.
const (
	longMA, mediumMA = 200, 50
	slopeDays        = 20
	crossPct         = 1.0
	highDays         = 252
	ddGood, ddBad    = -5.0, -15.0
	breadthGood      = 60.0
	breadthBad       = 40.0
	minBreadth       = 5 // sectors needed before breadth counts
	vixCalm, vixFear = 15.0, 22.0
	BullScore        = 4
	BearScore        = -4
	ExitBull         = 1 // a state ends only once the score has clearly left it
	ExitBear         = -1
	ConfirmDays      = 5
	directionDays    = 20
	directionMove    = 2
	correctionPct    = -10.0
	bearMarketPct    = -20.0
)

// Check is one input to the score. Value explains the reading in words.
type Check struct {
	Name      string
	Score     int
	Available bool
	Value     string
}

// Day is the regime on one date.
type Day struct {
	Date      market.Date
	Close     float64 // the market index
	Score     int
	Checks    []Check
	State     State  // confirmed
	Target    State  // what today's score alone says
	Pending   int    // days the target has held, when it differs from State
	Direction string // "weakening", "recovering", or ""
	Drawdown  float64
	SmallDD   float64
	Breadth   float64 // % of sectors above their 200-day average, NaN if unavailable
	Sectors   int     // sectors counted in Breadth
}

// DrawdownLabel names a fall from the peak.
func DrawdownLabel(dd float64) string {
	switch {
	case math.IsNaN(dd):
		return ""
	case dd <= bearMarketPct:
		return "Bear market"
	case dd <= correctionPct:
		return "Correction"
	}
	return ""
}

// aboveCheck scores a series against its long average.
func aboveCheck(x, avg float64, unit string) (int, string) {
	if x >= avg {
		return 1, fmt.Sprintf("%.1f%% above its 200-day average", (x/avg-1)*100) + unit
	}
	return -1, fmt.Sprintf("%.1f%% below its 200-day average", (1-x/avg)*100) + unit
}

// Timeline computes the regime for every date from the market index's 200th
// day on. It returns nil if the panel lacks the market index.
func Timeline(p *Panel) []Day {
	m, ok := p.Series[Market]
	if !ok {
		return nil
	}
	n := len(p.Dates)
	empty := nans(n)
	closes := func(name string) []float64 {
		if s, ok := p.Series[name]; ok {
			return s.Close
		}
		return empty
	}
	ma200, ma50 := sma(m.Close, longMA), sma(m.Close, mediumMA)
	dd := drawdown(m.Close, highDays)

	type sector struct{ close, ma []float64 }
	var sectors []sector
	for _, s := range p.Series {
		if s.Meta.Breadth {
			sectors = append(sectors, sector{s.Close, sma(s.Close, longMA)})
		}
	}

	large := closes(Large)
	small250, small100 := ratio(closes(Small250), large), ratio(closes(Small100), large)
	small250MA, small100MA := sma(small250, longMA), sma(small100, longMA)
	smallDD250, smallDD100 := drawdown(closes(Small250), highDays), drawdown(closes(Small100), highDays)
	beta := ratio(closes(HighBeta), closes(LowVol))
	betaMA := sma(beta, longMA)
	vix := closes(VIX)
	vixMA := sma(vix, longMA)

	var out []Day
	state, target, held := Neutral, Neutral, 0
	for i := range n {
		if math.IsNaN(ma200[i]) {
			continue
		}
		d := Day{Date: p.Dates[i], Close: m.Close[i], Drawdown: dd[i], Breadth: math.NaN()}

		c1 := Check{Name: "Long trend", Available: i >= slopeDays && !math.IsNaN(ma200[i-slopeDays])}
		if c1.Available {
			rising := ma200[i] > ma200[i-slopeDays]
			above := m.Close[i] > ma200[i]
			word, slope := "below", "falling"
			if above {
				word = "above"
			}
			if rising {
				slope = "rising"
			}
			c1.Value = word + " a " + slope + " 200-day average"
			if above && rising {
				c1.Score = 1
			} else if !above && !rising {
				c1.Score = -1
			}
		}

		gap := (ma50[i]/ma200[i] - 1) * 100
		c2 := Check{Name: "Medium trend", Available: true, Value: fmt.Sprintf("50-day average %+.1f%% from 200-day", gap)}
		if gap > crossPct {
			c2.Score = 1
		} else if gap < -crossPct {
			c2.Score = -1
		}

		c3 := Check{Name: "Drawdown", Available: true, Value: fmt.Sprintf("%.1f%% from 1-year high", dd[i])}
		if dd[i] > ddGood {
			c3.Score = 1
		} else if dd[i] < ddBad {
			c3.Score = -1
		}

		c4 := Check{Name: "Sector breadth", Value: "too few sectors with history"}
		above := 0
		for _, s := range sectors {
			if !math.IsNaN(s.ma[i]) {
				d.Sectors++
				if s.close[i] > s.ma[i] {
					above++
				}
			}
		}
		if d.Sectors >= minBreadth {
			d.Breadth = float64(above) / float64(d.Sectors) * 100
			c4.Available = true
			c4.Value = fmt.Sprintf("%d of %d sectors above 200-day average", above, d.Sectors)
			if d.Breadth >= breadthGood {
				c4.Score = 1
			} else if d.Breadth <= breadthBad {
				c4.Score = -1
			}
		}

		c5 := Check{Name: "Small vs large", Value: "no history"}
		smallRatio, smallAvg := small250[i], small250MA[i]
		d.SmallDD = smallDD250[i]
		if math.IsNaN(smallAvg) {
			smallRatio, smallAvg = small100[i], small100MA[i]
			d.SmallDD = smallDD100[i]
		}
		if !math.IsNaN(smallAvg) {
			c5.Available = true
			c5.Score, c5.Value = aboveCheck(smallRatio, smallAvg, "")
		}

		c6 := Check{Name: "High beta vs low vol", Value: "no history"}
		if !math.IsNaN(betaMA[i]) {
			c6.Available = true
			c6.Score, c6.Value = aboveCheck(beta[i], betaMA[i], "")
		}

		c7 := Check{Name: "India VIX", Value: "no history"}
		if !math.IsNaN(vix[i]) {
			c7.Available = true
			c7.Value = fmt.Sprintf("%.1f", vix[i])
			if vix[i] > vixFear {
				c7.Score = -1
			} else if vix[i] < vixCalm && !math.IsNaN(vixMA[i]) && vix[i] < vixMA[i] {
				c7.Score = 1
			}
		}

		d.Checks = []Check{c1, c2, c3, c4, c5, c6, c7}
		for _, c := range d.Checks {
			d.Score += c.Score
		}

		now := desired(state, d.Score)
		if now == target {
			held++
		} else {
			target, held = now, 1
		}
		if target != state && held >= ConfirmDays {
			state = target
		}
		d.State, d.Target = state, target
		if target != state {
			d.Pending = held
		}
		if len(out) >= directionDays {
			switch move := d.Score - out[len(out)-directionDays].Score; {
			case move >= directionMove:
				d.Direction = "recovering"
			case move <= -directionMove:
				d.Direction = "weakening"
			}
		}
		out = append(out, d)
	}
	return out
}

// desired is the state a score calls for, given the current state. Entering
// Bull or Bear takes a strong score, but leaving one needs the score to fall
// back past ExitBull or ExitBear, which stops scores near the line flipping it.
func desired(state State, score int) State {
	switch {
	case score >= BullScore:
		return Bull
	case score <= BearScore:
		return Bear
	case state == Bull && score > ExitBull:
		return Bull
	case state == Bear && score < ExitBear:
		return Bear
	}
	return Neutral
}

// Transition is a confirmed change of state.
type Transition struct {
	Date     market.Date
	From, To State
	Close    float64
	Drawdown float64 // of the market when confirmed
	Days     int     // trading days the previous state lasted
}

// Transitions lists the confirmed changes in a timeline.
func Transitions(days []Day) []Transition {
	var out []Transition
	last := 0
	for i := 1; i < len(days); i++ {
		if days[i].State != days[i-1].State {
			out = append(out, Transition{days[i].Date, days[i-1].State, days[i].State, days[i].Close, days[i].Drawdown, i - last})
			last = i
		}
	}
	return out
}
