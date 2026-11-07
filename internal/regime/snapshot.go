package regime

import (
	"fmt"
	"math"
	"strings"
	"time"

	"calm/internal/market"
)

// peSince is where P/E percentiles start. NSE moved its index P/E to
// consolidated earnings then, which broke comparability with earlier readings.
var peSince = market.Date{Year: 2021, Month: time.April, Day: 1}

// Snapshot is everything the report says about one date.
type Snapshot struct {
	Date        market.Date
	Today       Day
	Days        []Day
	Transitions []Transition
	Fresh       []Transition // confirmed since the last report sent
	Sectors     []Row
	Broad       []Row
	Top, Bottom []Row
	Unusual     []Row
	Rotated     []Row
	Market      Row
	PE, PEPct   float64
	Warnings    []string
}

// Build makes the snapshot for the panel's last date on or before asOf. Fresh
// lists changes after lastSent, which may be several days back if the job was off.
func Build(p *Panel, asOf, lastSent market.Date, sectorGroup, broadGroup string) (*Snapshot, error) {
	i := -1
	for j, d := range p.Dates {
		if !asOf.Before(d) {
			i = j
		}
	}
	if i < 0 {
		return nil, fmt.Errorf("no index data on or before %s", asOf)
	}
	days := Timeline(p)
	if len(days) == 0 {
		return nil, fmt.Errorf("no history for %s", Market)
	}
	d := p.Dates[i]
	last := -1
	for j, x := range days {
		if x.Date == d {
			last = j
		}
	}
	if last < 0 {
		return nil, fmt.Errorf("%s is before the regime history starts", d)
	}
	days = days[:last+1]
	s := &Snapshot{Date: d, Today: days[last], Days: days, Transitions: Transitions(days),
		Sectors: Sectors(p, i, sectorGroup), Broad: Sectors(p, i, broadGroup)}
	for _, t := range s.Transitions {
		if lastSent.Before(t.Date) {
			s.Fresh = append(s.Fresh, t)
		}
	}
	for _, r := range s.Broad {
		if r.Name == Market {
			s.Market = r
		}
	}
	n := min(5, len(s.Sectors))
	s.Top = s.Sectors[:n]
	for j := len(s.Sectors) - 1; j >= len(s.Sectors)-n; j-- {
		s.Bottom = append(s.Bottom, s.Sectors[j])
	}
	for _, r := range s.Sectors {
		if r.Unusual {
			s.Unusual = append(s.Unusual, r)
		}
		// Sectors near the centre flip on tiny moves, so only clear arrivals in
		// Leading or Lagging are called out.
		if (r.Quadrant == Leading || r.Quadrant == Lagging) && r.WasQuadrant != "" && r.Quadrant != r.WasQuadrant &&
			math.Abs(r.Trend) >= minRotation {
			s.Rotated = append(s.Rotated, r)
		}
	}
	from := 0
	for j, x := range p.Dates {
		if x.Before(peSince) {
			from = j + 1
		}
	}
	s.PEPct, s.PE = PEPercentile(p, Market, from, i)
	return s, nil
}

func signed(v float64) string {
	if math.IsNaN(v) {
		return "n/a"
	}
	return fmt.Sprintf("%+.2f%%", v)
}

func moves(rows []Row) string {
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("%s %s", short(r.Name), signed(r.Change[0])))
	}
	return strings.Join(parts, ", ")
}

// short drops the prefix every sector shares. A size index keeps it, since
// "Nifty 50" means nothing as "50".
func short(name string) string {
	for _, p := range []string{"Nifty ", "NIFTY "} {
		if s, ok := strings.CutPrefix(name, p); ok && (s[0] < '0' || s[0] > '9') {
			return s
		}
	}
	return name
}

// Headline names the regime in one line.
func (s *Snapshot) Headline() string {
	t := s.Today
	out := fmt.Sprintf("%s, score %+d of 7", t.State, t.Score)
	if t.Direction != "" {
		out += ", " + t.Direction
	}
	return out
}

// Brief is the line that goes with the card, which holds everything else.
func (s *Snapshot) Brief() string {
	var b strings.Builder
	for _, c := range s.Fresh {
		fmt.Fprintf(&b, ":rotating_light: *REGIME CHANGE: %s to %s, confirmed %s*\n", c.From, c.To, c.Date)
	}
	fmt.Fprintf(&b, "*Regime: %s*, close of %s", s.Headline(), s.Date)
	if t := s.Today; t.Pending > 0 {
		fmt.Fprintf(&b, "\nWatch: %s signal, day %d of %d to confirm", t.Target, t.Pending, ConfirmDays)
	}
	for _, w := range s.Warnings {
		fmt.Fprintf(&b, "\n:warning: %s", w)
	}
	return b.String()
}

// Summary is the short text for Slack and the terminal, in Slack's markup.
func (s *Snapshot) Summary() string {
	t := s.Today
	var b strings.Builder
	for _, c := range s.Fresh {
		fmt.Fprintf(&b, ":rotating_light: *REGIME CHANGE: %s to %s, confirmed %s*\n", c.From, c.To, c.Date)
	}
	fmt.Fprintf(&b, "*Market close %s*\n*Regime: %s*\n", s.Date, s.Headline())
	if t.Pending > 0 {
		fmt.Fprintf(&b, "Watch: %s signal, day %d of %d to confirm\n", t.Target, t.Pending, ConfirmDays)
	}
	fmt.Fprintf(&b, "Nifty 500 %.2f (%s), %.1f%% from 1-year high", t.Close, signed(s.Market.Change[0]), t.Drawdown)
	if l := DrawdownLabel(t.Drawdown); l != "" {
		fmt.Fprintf(&b, " (*%s*)", l)
	}
	fmt.Fprintf(&b, "\nSmall caps %.1f%% from high", t.SmallDD)
	if l := DrawdownLabel(t.SmallDD); l != "" {
		fmt.Fprintf(&b, " (*%s*)", l)
	}
	if !math.IsNaN(t.Breadth) {
		fmt.Fprintf(&b, "\nBreadth: %.0f%% of %d sectors above 200-day average", t.Breadth, t.Sectors)
	}
	fmt.Fprintf(&b, "\n*Top:* %s\n*Bottom:* %s", moves(s.Top), moves(s.Bottom))
	if len(s.Unusual) > 0 {
		fmt.Fprintf(&b, "\n*Unusual:* %s", moves(s.Unusual))
	}
	if len(s.Rotated) > 0 {
		parts := make([]string, 0, len(s.Rotated))
		for _, r := range s.Rotated {
			parts = append(parts, fmt.Sprintf("%s %s to %s", short(r.Name), r.WasQuadrant, r.Quadrant))
		}
		fmt.Fprintf(&b, "\n*Rotation this week:* %s", strings.Join(parts, ", "))
	}
	for _, w := range s.Warnings {
		fmt.Fprintf(&b, "\n:warning: %s", w)
	}
	return b.String()
}
