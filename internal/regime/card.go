package regime

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed card.html.tmpl
var cardTemplate string

// Card size in CSS pixels. It's rendered at twice this so text stays sharp when
// Slack scales it on a phone.
const (
	cardWidth, cardHeight = 1000, 740
	cardAlertHeight       = 46
	cardMovers            = 6
	cardDays              = 252
	chartW, chartH        = 440, 150
)

type cardBar struct {
	Name, Value string
	Width       float64 // % of the track; 50 reaches the edge from the centre
	Up, Unusual bool
}

type cardBand struct {
	X, W float64
	Bull bool
}

type cardCheck struct {
	Name, Glyph, Class string
}

// sparkline draws the market's last year with regime bands.
func sparkline(days []Day) (path string, bands []cardBand, lo, hi, first, last float64) {
	if len(days) > cardDays {
		days = days[len(days)-cardDays:]
	}
	lo, hi = math.Inf(1), math.Inf(-1)
	for _, d := range days {
		lo, hi = min(lo, d.Close), max(hi, d.Close)
	}
	x := func(i int) float64 { return float64(i) / float64(max(1, len(days)-1)) * chartW }
	y := func(v float64) float64 { return 6 + (hi-v)/max(hi-lo, 1)*(chartH-12) }
	var b strings.Builder
	for i, d := range days {
		cmd := "L"
		if i == 0 {
			cmd = "M"
		}
		fmt.Fprintf(&b, "%s%.1f %.1f", cmd, x(i), y(d.Close))
	}
	for i := 0; i < len(days); {
		j := i
		for j+1 < len(days) && days[j+1].State == days[i].State {
			j++
		}
		if days[i].State != Neutral {
			bands = append(bands, cardBand{x(i), max(1, x(j)-x(i)), days[i].State == Bull})
		}
		i = j + 1
	}
	return b.String(), bands, lo, hi, days[0].Close, days[len(days)-1].Close
}

func cardBars(rows []Row, scale float64) []cardBar {
	out := make([]cardBar, 0, len(rows))
	for _, r := range rows {
		c := r.Change[0]
		out = append(out, cardBar{short(r.Name), fmt.Sprintf("%+.2f%%", c), math.Abs(c) / scale * 50, c >= 0, r.Unusual})
	}
	return out
}

func cardChecks(cs []Check) []cardCheck {
	out := make([]cardCheck, 0, len(cs))
	for _, c := range cs {
		v := cardCheck{c.Name, "0", "flat"}
		switch {
		case !c.Available:
			v.Glyph, v.Class = "–", "flat"
		case c.Score > 0:
			v.Glyph, v.Class = "+", "plus"
		case c.Score < 0:
			v.Glyph, v.Class = "−", "minus"
		}
		out = append(out, v)
	}
	return out
}

// CardHeight grows the card for each alert it carries.
func CardHeight(s *Snapshot) int {
	alerts := len(s.Fresh)
	if s.Today.Pending > 0 {
		alerts++
	}
	return cardHeight + alerts*cardAlertHeight
}

// WriteCard renders the snapshot as a fixed-size page for a screenshot.
func WriteCard(path string, s *Snapshot) error {
	t := s.Today
	var top, bottom []Row
	n := min(cardMovers, len(s.Sectors))
	top = s.Sectors[:n]
	bottom = s.Sectors[len(s.Sectors)-n:]
	scale := 0.01
	for _, r := range append(append([]Row{}, top...), bottom...) {
		scale = max(scale, math.Abs(r.Change[0]))
	}
	spark, bands, lo, hi, first, last := sparkline(s.Days)
	v := struct {
		State, Headline, Date, Direction   string
		Score                              string
		Marker                             float64
		Watch                              string
		Fresh                              []Transition
		Market, MarketSub, Small, SmallSub string
		Breadth, BreadthSub, PE, PESub     string
		Top, Bottom                        []cardBar
		Spark                              string
		Bands                              []cardBand
		Lo, Hi, YearChange                 string
		ChartW, ChartH                     int
		Checks                             []cardCheck
		Leading, Lagging                   []string
		Width, Height                      int
		AnyUnusual                         bool
	}{State: string(t.State), Date: s.Date.At(12, 0).Format("Mon 2 Jan 2006"), Direction: t.Direction,
		Score: fmt.Sprintf("%+d", t.Score), Marker: float64(t.Score+7) / 14 * 100, Fresh: s.Fresh,
		Market: fmt.Sprintf("%.0f", t.Close), MarketSub: fmt.Sprintf("%s today, %.1f%% from high", signed(s.Market.Change[0]), t.Drawdown),
		Small: fmt.Sprintf("%.1f%%", t.SmallDD), SmallSub: "from 1-year high",
		Breadth: "n/a", BreadthSub: "sectors above 200-day", PE: "n/a",
		Top: cardBars(top, scale), Bottom: cardBars(bottom, scale), Spark: spark, Bands: bands,
		Lo: fmt.Sprintf("%.0f", lo), Hi: fmt.Sprintf("%.0f", hi), YearChange: fmt.Sprintf("%+.1f%%", (last/first-1)*100),
		ChartW: chartW, ChartH: chartH, Checks: cardChecks(t.Checks), Width: cardWidth, Height: CardHeight(s)}
	if t.Pending > 0 {
		v.Watch = fmt.Sprintf("Watch: %s signal, day %d of %d to confirm", t.Target, t.Pending, ConfirmDays)
	}
	if l := DrawdownLabel(t.Drawdown); l != "" {
		v.MarketSub += ", " + l
	}
	if l := DrawdownLabel(t.SmallDD); l != "" {
		v.SmallSub += ", " + l
	}
	if !math.IsNaN(t.Breadth) {
		v.Breadth = fmt.Sprintf("%.0f%%", t.Breadth)
		v.BreadthSub = fmt.Sprintf("of %d above 200-day average", t.Sectors)
	}
	if !math.IsNaN(s.PE) {
		v.PE = fmt.Sprintf("%.1f", s.PE)
		v.PESub = "Nifty 500"
		if !math.IsNaN(s.PEPct) {
			v.PESub = fmt.Sprintf("percentile %.0f since 2021", s.PEPct)
		}
	}
	for _, r := range s.Rotated {
		if r.Quadrant == Leading {
			v.Leading = append(v.Leading, short(r.Name))
		} else {
			v.Lagging = append(v.Lagging, short(r.Name))
		}
	}
	for _, b := range append(append([]cardBar{}, v.Top...), v.Bottom...) {
		v.AnyUnusual = v.AnyUnusual || b.Unusual
	}
	tmpl, err := template.New("card").Parse(cardTemplate)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return tmpl.Execute(f, v)
}

// browser finds a Chromium-family browser.
func browser() (string, error) {
	names := []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"}
	if v := os.Getenv("CHROME_BIN"); v != "" {
		names = append([]string{v}, names...)
	}
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p, nil
		}
	}
	return "", errors.New("no Chrome or Chromium found")
}

// RenderPNG screenshots a card page with a headless browser. A throwaway
// profile keeps it clear of any browser you have open.
func RenderPNG(ctx context.Context, htmlPath, pngPath string, height int) error {
	bin, err := browser()
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(htmlPath)
	if err != nil {
		return err
	}
	out, err := filepath.Abs(pngPath)
	if err != nil {
		return err
	}
	profile, err := os.MkdirTemp("", "calm-card-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(profile)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--headless=new", "--disable-gpu", "--hide-scrollbars", "--no-first-run",
		"--force-device-scale-factor=2", "--user-data-dir="+profile,
		fmt.Sprintf("--window-size=%d,%d", cardWidth, height), "--screenshot="+out, "file://"+abs)
	os.Remove(out)
	if b, err := cmd.CombinedOutput(); err != nil {
		tail := string(b)
		return fmt.Errorf("render card: %w: %s", err, tail[max(0, len(tail)-200):])
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		return errors.New("render card: no image written")
	}
	return nil
}
