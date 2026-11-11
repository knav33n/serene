package mcx

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"serene/internal/dow"
	"serene/internal/kite"
	"serene/internal/market"
)

// PulseNames are the commodities in the hourly pulse; the rest trade too
// thinly to read structure from.
var PulseNames = []string{"CRUDEOIL", "NATURALGAS", "GOLD", "SILVER", "COPPER", "ZINC", "ALUMINIUM"}

// Pulse settings: swings come from 15-minute bars, with a reversal of
// SwingMultiple times their median range over the lookback.
const (
	pulseLookback = 7 * 24 * time.Hour
	swingFrame    = 15 * time.Minute
	SwingMultiple = 2.5
)

// Series is one commodity's most traded future and its recent minute bars.
type Series struct {
	Name, Symbol string
	Bars         []market.Bar
}

// FetchRecent returns the last week of 1-minute bars for each commodity's
// most traded future, choosing between its two nearest expiries by volume in
// the latest session.
func FetchRecent(ctx context.Context, kc *kite.Client, names []string, now time.Time) ([]Series, error) {
	all, err := kc.Instruments(ctx, "MCX")
	if err != nil {
		return nil, fmt.Errorf("instrument list: %w", err)
	}
	byName := map[string][]kite.Instrument{}
	for _, in := range all {
		if in.Type == "FUT" && !in.Expiry.IsZero() && slices.Contains(names, in.Name) {
			byName[in.Name] = append(byName[in.Name], in)
		}
	}
	var out []Series
	for _, name := range names {
		futs := byName[name]
		if len(futs) == 0 {
			return nil, fmt.Errorf("no MCX future for %s", name)
		}
		sort.Slice(futs, func(i, j int) bool { return futs[i].Expiry.Before(futs[j].Expiry) })
		var best Series
		var bestVol int64 = -1
		for _, f := range futs[:min(2, len(futs))] {
			cs, err := kc.Candles(ctx, f.Token, "minute", now.Add(-pulseLookback), now, false)
			if err != nil {
				return nil, fmt.Errorf("%s minute: %w", f.Symbol, err)
			}
			bars := toBars(f.Symbol, market.Minute1, cs)
			if v := lastSessionVolume(bars); v > bestVol {
				best, bestVol = Series{Name: name, Symbol: f.Symbol, Bars: bars}, v
			}
		}
		out = append(out, best)
	}
	return out, nil
}

func lastSessionVolume(bars []market.Bar) int64 {
	if len(bars) == 0 {
		return 0
	}
	d := market.DateOf(bars[len(bars)-1].Time)
	var v int64
	for i := len(bars) - 1; i >= 0 && market.DateOf(bars[i].Time) == d; i-- {
		v += bars[i].Volume
	}
	return v
}

// CleanStaleOpens fixes a Kite fault: when a session opens late, as on a
// holiday with only the evening session, its first minute can carry the
// previous session's opening price as its open, high or low. A price equal to
// that and far from the bar's close is replaced by the bar's own prices.
func CleanStaleOpens(bars []market.Bar) []market.Bar {
	out := slices.Clone(bars)
	if len(out) < 2 {
		return out
	}
	far := 5 * medianRange(out)
	var prevOpen market.Paise
	for i := range out {
		if i > 0 && market.DateOf(out[i].Time) == market.DateOf(out[i-1].Time) {
			continue
		}
		b := out[i]
		if i > 0 && abs(prevOpen-b.Close) > far {
			next := b.Close
			if i+1 < len(out) {
				next = out[i+1].Open
			}
			if b.Open == prevOpen {
				b.Open = b.Close
			}
			if b.High == prevOpen {
				b.High = max(b.Open, b.Close, next)
			}
			if b.Low == prevOpen {
				b.Low = min(b.Open, b.Close, next)
			}
			out[i] = b
		}
		prevOpen = bars[i].Open
	}
	return out
}

func abs(p market.Paise) market.Paise {
	if p < 0 {
		return -p
	}
	return p
}

func medianRange(bars []market.Bar) market.Paise {
	rs := make([]market.Paise, len(bars))
	for i, b := range bars {
		rs[i] = b.High - b.Low
	}
	slices.Sort(rs)
	return rs[len(rs)/2]
}

// Bucket merges bars into bars of length d, aligned to IST clock time.
func Bucket(bars []market.Bar, d time.Duration) []market.Bar {
	var out []market.Bar
	for _, b := range bars {
		t := b.Time.In(market.IST)
		start := t.Truncate(d) // IST is a whole number of minutes from UTC
		if d >= time.Hour {
			start = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, market.IST)
		}
		if n := len(out); n > 0 && out[n-1].Time.Equal(start) {
			o := &out[n-1]
			o.High, o.Low, o.Close = max(o.High, b.High), min(o.Low, b.Low), b.Close
			o.Volume += b.Volume
			o.OI = b.OI
			o.End = b.End
			continue
		}
		nb := b
		nb.Time = start
		out = append(out, nb)
	}
	return out
}

// Pulse is one commodity's hour.
type Pulse struct {
	Name, Symbol string
	Hour         market.Bar
	PrevClose    market.Paise // close of the previous session
	Threshold    market.Paise
	Trend        dow.Trend
	High, Low    *dow.Swing // latest confirmed swings at the hour's end
	Running      dow.Swing
	New          []dow.Swing // swings confirmed during the hour
	Breaks       []dow.Break
}

// Analyze reads s's structure at the end of the hour that starts at hour. It
// reports false when the contract didn't trade in that hour.
func Analyze(s Series, hour time.Time) (Pulse, bool) {
	end := hour.Add(time.Hour)
	var bars []market.Bar
	for _, b := range CleanStaleOpens(s.Bars) {
		if b.Time.Before(end) {
			bars = append(bars, b)
		}
	}
	p := Pulse{Name: s.Name, Symbol: s.Symbol}
	var in []market.Bar
	for _, b := range bars {
		if !b.Time.Before(hour) {
			in = append(in, b)
		}
	}
	if len(in) == 0 {
		return p, false
	}
	p.Hour = Bucket(in, time.Hour)[0]
	today := market.DateOf(hour)
	for i := len(bars) - 1; i >= 0; i-- {
		if market.DateOf(bars[i].Time).Before(today) {
			p.PrevClose = bars[i].Close
			break
		}
	}
	q := Bucket(bars, swingFrame)
	// A quarter still running at the hour's end can't confirm a swing yet.
	if n := len(q); n > 0 && q[n-1].End.After(end) {
		q = q[:n-1]
	}
	p.Threshold = market.Paise(SwingMultiple * float64(medianRange(q)))
	swings, running := dow.Swings(q, p.Threshold)
	known := dow.At(swings, end)
	p.Running = running // bars stop at end, so this is the leg as of then
	eff := dow.WithRunning(known, p.Running)
	p.Trend = dow.TrendOf(eff)
	p.High, p.Low = dow.Last(eff)
	for _, sw := range known {
		if sw.Confirmed.After(hour) {
			p.New = append(p.New, sw)
		}
	}
	p.Breaks = dow.Breaks(q, swings, hour)
	return p, true
}

// Price formats rupees without trailing zeros.
func Price(p market.Paise) string {
	return strconv.FormatFloat(p.Rupees(), 'f', -1, 64)
}

func pct(a, b market.Paise) string {
	if b == 0 {
		return "—"
	}
	return fmt.Sprintf("%+.2f%%", (a.Rupees()-b.Rupees())/b.Rupees()*100)
}

// swingText marks a running leg's extreme, not yet a confirmed swing, with *.
func swingText(s dow.Swing) string {
	t := string(s.Label) + " " + Price(s.Price)
	if s.Confirmed.IsZero() {
		t += "*"
	}
	return t
}

// Format writes the pulse as a Slack message.
func Format(ps []Pulse, hour time.Time) string {
	var sb strings.Builder
	h := hour.In(market.IST)
	fmt.Fprintf(&sb, "*MCX hourly · %s–%s IST · %s*\n", h.Format("15:04"), h.Add(time.Hour).Format("15:04"), h.Format("Mon 2 Jan"))
	sb.WriteString("```\n")
	fmt.Fprintf(&sb, "%-10s %8s %7s %7s  %-15s %-6s %s\n", "", "last", "day", "hour", "hour range", "trend", "last swings")
	for _, p := range ps {
		sw := "—"
		if p.High != nil && p.Low != nil {
			sw = swingText(*p.High) + " / " + swingText(*p.Low)
		}
		fmt.Fprintf(&sb, "%-10s %8s %7s %7s  %-15s %-6s %s\n", p.Name, Price(p.Hour.Close), pct(p.Hour.Close, p.PrevClose),
			pct(p.Hour.Close, p.Hour.Open), Price(p.Hour.Low)+"–"+Price(p.Hour.High), p.Trend, sw)
	}
	sb.WriteString("```\n")
	var events []string
	for _, p := range ps {
		var ev []string
		for _, b := range p.Breaks {
			dir, side := "↓ broke swing low", "down"
			if b.Up {
				dir, side = "↑ broke swing high", "up"
			}
			kind := "continuation"
			if b.ChangeOfCharacter {
				kind = "*change of character " + side + "*"
			}
			ev = append(ev, fmt.Sprintf("%s %s at %s (%s)", dir, Price(b.Level), b.Time.In(market.IST).Format("15:04"), kind))
		}
		for _, s := range p.New {
			ev = append(ev, fmt.Sprintf("new %s %s (%s)", s.Label, Price(s.Price), s.Time.In(market.IST).Format("15:04")))
		}
		if len(ev) > 0 {
			events = append(events, fmt.Sprintf("• *%s* (%s): %s", p.Name, p.Symbol, strings.Join(ev, "; ")))
		}
	}
	if len(events) == 0 {
		sb.WriteString("No swing breaks or new swings this hour.\n")
	} else {
		sb.WriteString(strings.Join(events, "\n") + "\n")
	}
	sb.WriteString("_Swings: 15m zigzag, reversal 2.5× median 15m range; * = leg still running past the last swing. day = vs previous session close._")
	return sb.String()
}
