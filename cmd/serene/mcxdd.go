package main

import (
	"fmt"
	"sort"
	"time"

	"serene/internal/market"
	"serene/internal/mcx"
)

// ddStats is a drawdown of one equity curve: the deepest fall from a peak,
// when it began, bottomed and recovered, and the longest time spent under a
// peak.
type ddStats struct {
	depth                   float64
	peak, trough, recovered time.Time // recovered is zero if never
	longest                 time.Duration
	longFrom, longTo        time.Time
	longOpen                bool // the longest stretch was still under water at the end
}

// drawdown marks trades to market on every minute close of the contract held
// (charges taken at exit) and measures the equity curve's drawdown. Closed is
// the same on closed-trade equity only.
func drawdown(minute []market.Bar, ts []mcx.Trade, mult float64) (mtm, closed ddStats) {
	ts = append([]mcx.Trade(nil), ts...)
	sort.Slice(ts, func(i, j int) bool { return ts[i].Entry.Before(ts[j].Entry) })
	type pt struct {
		t  time.Time
		eq float64
	}
	var mp, cp []pt
	var eq float64
	i := 0
	for _, b := range minute {
		for i < len(ts) && !b.Time.Before(ts[i].Exit) {
			eq += ts[i].Net.Rupees()
			mp = append(mp, pt{ts[i].Exit, eq})
			cp = append(cp, pt{ts[i].Exit, eq})
			i++
		}
		if i == len(ts) {
			break
		}
		tr := ts[i]
		if b.Key != tr.Key || b.Time.Before(tr.Entry) {
			continue
		}
		open := float64(tr.Side) * (b.Close - tr.In).Rupees() * mult
		mp = append(mp, pt{b.End, eq + open})
	}
	for ; i < len(ts); i++ {
		eq += ts[i].Net.Rupees()
		mp = append(mp, pt{ts[i].Exit, eq})
		cp = append(cp, pt{ts[i].Exit, eq})
	}
	measure := func(ps []pt) ddStats {
		var s ddStats
		if len(ps) == 0 {
			return s
		}
		peak, peakT := 0.0, ps[0].t
		var curPeakT time.Time // peak of the deepest drawdown so far, pending recovery
		pending := false
		for _, p := range ps {
			if p.eq >= peak {
				if d := p.t.Sub(peakT); d > s.longest {
					s.longest, s.longFrom, s.longTo, s.longOpen = d, peakT, p.t, false
				}
				if pending && peakT.Equal(curPeakT) {
					s.recovered = p.t
					pending = false
				}
				peak, peakT = p.eq, p.t
				continue
			}
			if peak-p.eq > s.depth {
				s.depth, s.peak, s.trough = peak-p.eq, peakT, p.t
				curPeakT, pending, s.recovered = peakT, true, time.Time{}
			}
		}
		last := ps[len(ps)-1].t
		if d := last.Sub(peakT); d > s.longest && ps[len(ps)-1].eq < peak {
			s.longest, s.longFrom, s.longTo, s.longOpen = d, peakT, last, true
		}
		return s
	}
	return measure(mp), measure(cp)
}

func ddLine(label string, s ddStats) string {
	day := func(t time.Time) string { return t.In(market.IST).Format("2006-01-02") }
	days := func(d time.Duration) int { return int(d.Hours()/24 + 0.5) }
	rec := "not recovered"
	if !s.recovered.IsZero() {
		rec = fmt.Sprintf("recovered %s (%d days peak→recovery)", day(s.recovered), days(s.recovered.Sub(s.peak)))
	}
	open := ""
	if s.longOpen {
		open = ", still under at end"
	}
	return fmt.Sprintf("  %-7s max DD ₹%.0f: peak %s → trough %s (%d days), %s; longest under water %d days (%s → %s%s)",
		label, s.depth, day(s.peak), day(s.trough), days(s.trough.Sub(s.peak)), rec,
		days(s.longest), day(s.longFrom), day(s.longTo), open)
}

// printDrawdowns prints mark-to-market and closed-trade drawdowns over the
// whole run and within each window, trades taken by entry date.
func printDrawdowns(minute []market.Bar, ts []mcx.Trade, mult float64, windows [][2]market.Date) {
	for _, w := range windows {
		var sub []mcx.Trade
		for _, tr := range ts {
			d := market.DateOf(tr.Entry)
			if !d.Before(w[0]) && !w[1].Before(d) {
				sub = append(sub, tr)
			}
		}
		var net float64
		for _, tr := range sub {
			net += tr.Net.Rupees()
		}
		m, c := drawdown(minute, sub, mult)
		fmt.Printf("  %s..%s: %d trades, net ₹%.0f\n", w[0], w[1], len(sub), net)
		fmt.Println(ddLine("MTM", m))
		fmt.Println(ddLine("closed", c))
	}
}
