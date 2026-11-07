package regime

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"os"
	"path/filepath"
)

//go:embed report.html.tmpl
var reportTemplate string

// Heat buckets a return from -3 to +3. Longer horizons need larger moves, so
// each return has its own scale.
var heatSteps = [][3]float64{{0.5, 1, 2}, {1, 2, 4}, {2, 4, 8}, {4, 8, 15}, {6, 12, 25}, {8, 16, 32}}

func heat(v float64, h int) int {
	if math.IsNaN(v) {
		return 0
	}
	n := 0
	for _, s := range heatSteps[h] {
		if math.Abs(v) >= s {
			n++
		}
	}
	if v < 0 {
		return -n
	}
	return n
}

type cell struct {
	Text  string
	Sort  float64
	Class string
}

type rowView struct {
	Name, Close, Quadrant, Trend, Above string
	Cells                               []cell
	FromHigh                            cell
	Flag                                string
	Rotated                             bool
}

// number is a float that encodes NaN as null, so charts can skip it.
type number float64

func (n number) MarshalJSON() ([]byte, error) {
	if math.IsNaN(float64(n)) || math.IsInf(float64(n), 0) {
		return []byte("null"), nil
	}
	return []byte(fmt.Sprintf("%.2f", float64(n))), nil
}

func views(rows []Row) []rowView {
	out := make([]rowView, 0, len(rows))
	for _, r := range rows {
		v := rowView{Name: short(r.Name), Close: fmt.Sprintf("%.2f", r.Close), Quadrant: r.Quadrant,
			Rotated: r.Quadrant != r.WasQuadrant && r.WasQuadrant != "", Trend: "n/a"}
		for h, c := range r.Change {
			text := "–"
			sort := -1e9
			if !math.IsNaN(c) {
				text, sort = fmt.Sprintf("%+.1f", c), c
				if h == 0 {
					text = fmt.Sprintf("%+.2f", c)
				}
			}
			v.Cells = append(v.Cells, cell{text, sort, fmt.Sprintf("h%d", heat(c, h)+3)})
		}
		v.FromHigh = cell{fmt.Sprintf("%.1f", r.FromHigh), r.FromHigh, ""}
		if r.Has200 {
			v.Above = "below"
			if r.Above200 {
				v.Above = "above"
			}
		} else {
			v.Above = "too new"
		}
		if r.Quadrant != "" {
			v.Trend = fmt.Sprintf("%+.1f / %+.1f", r.Trend, r.Momentum)
		}
		if r.Unusual {
			v.Flag = "Unusual"
		}
		out = append(out, v)
	}
	return out
}

type dayPoint struct {
	D string `json:"d"`
	C number `json:"c"`
	S int    `json:"s"`
	R string `json:"r"`
	B number `json:"b"`
}

type dot struct {
	N string `json:"n"`
	X number `json:"x"`
	Y number `json:"y"`
	Q string `json:"q"`
	U bool   `json:"u"`
}

type transView struct {
	Date, From, To, Drawdown, Close string
	Days                            int
}

// WriteReport renders a snapshot as a standalone HTML file.
func WriteReport(path string, s *Snapshot) error {
	t := s.Today
	view := struct {
		Title, Headline, State, Date, Market, Small, Breadth, PE string
		Target                                                   string
		Pending, Confirm                                         int
		Fresh                                                    []Transition
		Checks                                                   []Check
		Sectors, Broad                                           []rowView
		Horizons                                                 []string
		Transitions                                              []transView
		Warnings                                                 []string
		Days                                                     []dayPoint
		Dots                                                     []dot
		Bull, Bear                                               int
	}{Title: "Market " + s.Date.String(), Headline: s.Headline(), State: string(t.State), Date: s.Date.String(),
		Target: string(t.Target), Pending: t.Pending, Confirm: ConfirmDays, Fresh: s.Fresh, Checks: t.Checks,
		Sectors: views(s.Sectors), Broad: views(s.Broad), Warnings: s.Warnings, Bull: BullScore, Bear: BearScore}
	view.Market = fmt.Sprintf("%.2f (%s), %.1f%% from 1-year high", t.Close, signed(s.Market.Change[0]), t.Drawdown)
	if l := DrawdownLabel(t.Drawdown); l != "" {
		view.Market += ", " + l
	}
	view.Small = fmt.Sprintf("%.1f%% from 1-year high", t.SmallDD)
	if l := DrawdownLabel(t.SmallDD); l != "" {
		view.Small += ", " + l
	}
	view.Breadth = "n/a"
	if !math.IsNaN(t.Breadth) {
		view.Breadth = fmt.Sprintf("%.0f%% of %d sectors", t.Breadth, t.Sectors)
	}
	view.PE = "n/a"
	if !math.IsNaN(s.PE) {
		view.PE = fmt.Sprintf("%.1f", s.PE)
		if !math.IsNaN(s.PEPct) {
			view.PE += fmt.Sprintf(", percentile %.0f since %s", s.PEPct, peSince)
		}
	}
	for _, r := range Returns {
		view.Horizons = append(view.Horizons, r.Label)
	}
	for i := len(s.Transitions) - 1; i >= 0; i-- {
		c := s.Transitions[i]
		view.Transitions = append(view.Transitions, transView{c.Date.String(), string(c.From), string(c.To),
			fmt.Sprintf("%.1f%%", c.Drawdown), fmt.Sprintf("%.0f", c.Close), c.Days})
	}
	for _, d := range s.Days {
		view.Days = append(view.Days, dayPoint{d.Date.String(), number(d.Close), d.Score, string(d.State), number(d.Breadth)})
	}
	for _, r := range s.Sectors {
		if r.Quadrant != "" {
			view.Dots = append(view.Dots, dot{short(r.Name), number(r.Trend), number(r.Momentum), r.Quadrant, r.Unusual})
		}
	}
	data, err := json.Marshal(struct {
		Days []dayPoint `json:"days"`
		Dots []dot      `json:"dots"`
		Bull int        `json:"bull"`
		Bear int        `json:"bear"`
	}{view.Days, view.Dots, BullScore, BearScore})
	if err != nil {
		return err
	}
	tmpl, err := template.New("report").Parse(reportTemplate)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	err = tmpl.Execute(f, struct {
		V    any
		Data template.JS
	}{view, template.JS(data)})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
