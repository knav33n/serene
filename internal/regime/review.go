package regime

import "calm/internal/market"

// Window is a known market fall to review the regime against.
type Window struct {
	Name     string
	From, To market.Date
}

// Episode is how the regime behaved during a Window.
type Episode struct {
	Window
	Peak, Trough          market.Date
	Fall                  float64     // % peak to trough
	LeftBull, EnteredBear market.Date // first confirmed in the window, after the peak
	LeftBullDD, BearDD    float64     // % from the peak on that day
	Bull                  market.Date // first Bull after the trough, within a year
	BullGain              float64     // % above the trough on that day
	SinceTrough           int         // trading days from trough to that Bull
}

// Review measures each window on a timeline.
func Review(days []Day, windows []Window) []Episode {
	out := make([]Episode, 0, len(windows))
	for _, w := range windows {
		e := Episode{Window: w}
		first, last := -1, -1
		for i, d := range days {
			if !d.Date.Before(w.From) && !w.To.Before(d.Date) {
				if first < 0 {
					first = i
				}
				last = i
			}
		}
		if first < 0 {
			out = append(out, e)
			continue
		}
		// The peak comes first, so search the trough after it.
		peak := first
		for i := first; i <= last; i++ {
			if days[i].Close > days[peak].Close {
				peak = i
			}
		}
		trough := peak
		for i := peak; i <= last; i++ {
			if days[i].Close < days[trough].Close {
				trough = i
			}
		}
		e.Peak, e.Trough = days[peak].Date, days[trough].Date
		from := func(i int) float64 { return (days[i].Close/days[peak].Close - 1) * 100 }
		e.Fall = from(trough)
		for i := peak + 1; i <= last; i++ {
			if e.LeftBull.IsZero() && days[i-1].State == Bull && days[i].State != Bull {
				e.LeftBull, e.LeftBullDD = days[i].Date, from(i)
			}
			if e.EnteredBear.IsZero() && days[i].State == Bear {
				e.EnteredBear, e.BearDD = days[i].Date, from(i)
			}
		}
		for i := trough; i < len(days) && i <= trough+252; i++ {
			if days[i].State == Bull {
				e.Bull, e.BullGain = days[i].Date, (days[i].Close/days[trough].Close-1)*100
				e.SinceTrough = i - trough
				break
			}
		}
		out = append(out, e)
	}
	return out
}

// StateStat summarises what followed each state.
type StateStat struct {
	State     State
	Days      int
	Share     float64 // % of all days
	Forward   float64 // mean % return over the next forward days
	Positive  float64 // % of those returns above zero
	WorstNext float64 // worst single forward return
}

// Stats measures forward returns by state. These are in-sample and overlap
// heavily, so they describe the history and don't prove an edge.
func Stats(days []Day, forward int) []StateStat {
	out := make([]StateStat, 0, 3)
	for _, s := range []State{Bull, Neutral, Bear} {
		st := StateStat{State: s}
		count, sum, up := 0, 0.0, 0
		for i, d := range days {
			if d.State != s {
				continue
			}
			st.Days++
			if i+forward >= len(days) {
				continue
			}
			r := (days[i+forward].Close/d.Close - 1) * 100
			if count == 0 || r < st.WorstNext {
				st.WorstNext = r
			}
			count++
			sum += r
			if r > 0 {
				up++
			}
		}
		if len(days) > 0 {
			st.Share = float64(st.Days) / float64(len(days)) * 100
		}
		if count > 0 {
			st.Forward = sum / float64(count)
			st.Positive = float64(up) / float64(count) * 100
		}
		out = append(out, st)
	}
	return out
}
