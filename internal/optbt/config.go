// Package optbt backtests intraday and STBT short straddles and strangles on
// Fyers 1m bars over a full grid of StockMock-style settings: strike mode,
// round strikes, DTE, entry/exit time, wait-and-trade, stop, stop mode
// (per leg, move to cost, square off all), trailing stop, re-entry and hedge.
// Fills follow cmd/straddlebt so the two agree on shared cells.
package optbt

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is one grid. Every list is a parameter; cells are the cross-product
// of the lists, less combinations that cannot differ (see Expand).
type Config struct {
	Name string `yaml:"name"`
	From string `yaml:"from"`
	To   string `yaml:"to"`
	Data string `yaml:"data"` // Fyers store, default ~/INSIGHTS/fyers

	Index       []string  `yaml:"index"`
	DTE         []int     `yaml:"dte"` // trading days to the nearest expiry; -1 = any day
	Side        []string  `yaml:"side"`
	Strike      []string  `yaml:"strike"`
	Round       []int     `yaml:"round"`
	Entry       []string  `yaml:"entry"`
	Exit        []string  `yaml:"exit"`
	Hold        []string  `yaml:"hold"`
	Wait        []float64 `yaml:"wait"`
	SL          []float64 `yaml:"sl"`
	SLMode      []string  `yaml:"sl_mode"`
	Trail       []string  `yaml:"trail"`
	Reentry     []int     `yaml:"reentry"`
	ReentryMode []string  `yaml:"reentry_mode"`
	Hedge       []string  `yaml:"hedge"`
	SkipEvents  []string  `yaml:"skip_events"` // none, india (budget, rbi, election), all (+ fomc)

	SlipBps        float64 `yaml:"slip_bps"`
	Costs          string  `yaml:"costs"`
	NakedMarginPct float64 `yaml:"naked_margin_pct"`
	MDDMax         float64 `yaml:"mdd_max"`
	VIXDir         string  `yaml:"vix_dir"`
	Events         string  `yaml:"events"` // CSV of event days, default configs/events.csv
}

// LoadConfig reads a grid and fills defaults for unset lists and settings.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.defaults()
	return &c, nil
}

func (c *Config) defaults() {
	def := func(l *[]string, v ...string) {
		if len(*l) == 0 {
			*l = v
		}
	}
	def(&c.Index, "NIFTY")
	def(&c.Side, "both")
	def(&c.Strike, "atm")
	def(&c.Entry, "09:20")
	def(&c.Exit, "15:15")
	def(&c.Hold, "intraday")
	def(&c.SLMode, "leg")
	def(&c.Trail, "off")
	def(&c.ReentryMode, "cost")
	def(&c.Hedge, "none")
	def(&c.SkipEvents, "none")
	if len(c.DTE) == 0 {
		c.DTE = []int{-1}
	}
	if len(c.Round) == 0 {
		c.Round = []int{0}
	}
	if len(c.Wait) == 0 {
		c.Wait = []float64{0}
	}
	if len(c.SL) == 0 {
		c.SL = []float64{25}
	}
	if len(c.Reentry) == 0 {
		c.Reentry = []int{0}
	}
	if c.From == "" {
		c.From = "2022-01-01"
	}
	if c.To == "" {
		c.To = "2024-12-31"
	}
	if c.Data == "" {
		c.Data = os.Getenv("HOME") + "/INSIGHTS/fyers"
	}
	if c.SlipBps == 0 {
		c.SlipBps = 30
	}
	if c.Costs == "" {
		c.Costs = "configs/costs.yaml"
	}
	if c.NakedMarginPct == 0 {
		c.NakedMarginPct = 9
	}
	if c.MDDMax == 0 {
		c.MDDMax = 10
	}
	if c.Events == "" {
		c.Events = "configs/events.csv"
	}
	if c.VIXDir == "" {
		c.VIXDir = "data/indices/minute/nse"
	}
}

// Cell is one combination of parameters.
type Cell struct {
	ID          int
	Index       string
	DTE         int
	Side        string // both, ce, pe
	Strike      Rule
	Round       int // points; 0 = the index's strike step
	Entry, Exit int // minutes after 09:15
	STBT        bool
	Wait        float64 // %, − waits for a fall, + for a rise
	SL          float64 // %, 0 = none
	SLMode      string  // leg, cost, all
	TrailX      float64 // %, 0 = off
	TrailY      float64
	Reentry     int
	ReentryMode string // cost, reexec
	Hedge       Rule   // Kind "" = none
	Skip        string // event set whose days are not traded: none, india, all
	skipMask    uint8
}

// Name is a unique, readable cell id, also accepted by --cells.
func (c Cell) Name() string {
	var b strings.Builder
	dte := "dany"
	if c.DTE >= 0 {
		dte = fmt.Sprintf("d%d", c.DTE)
	}
	fmt.Fprintf(&b, "%s_%s_%s_%s", c.Index, dte, c.Side, c.Strike.Name)
	if c.Round > 0 {
		fmt.Fprintf(&b, "_r%d", c.Round)
	}
	hold := "i"
	if c.STBT {
		hold = "n"
	}
	fmt.Fprintf(&b, "_%s-%s%s", hhmm(c.Entry), hhmm(c.Exit), hold)
	if c.Wait != 0 {
		fmt.Fprintf(&b, "_w%g", c.Wait)
	}
	fmt.Fprintf(&b, "_sl%g", c.SL)
	if c.SL > 0 {
		b.WriteString("_" + c.SLMode)
		if c.TrailX > 0 {
			fmt.Fprintf(&b, "_t%gx%g", c.TrailX, c.TrailY)
		}
		if c.Reentry > 0 {
			fmt.Fprintf(&b, "_re%d%s", c.Reentry, c.ReentryMode)
		}
	}
	if c.Hedge.Kind != "" {
		b.WriteString("_h" + c.Hedge.Name)
	}
	if c.Skip != "" && c.Skip != "none" {
		b.WriteString("_x" + c.Skip)
	}
	return b.String()
}

// Expand returns every cell of the grid. Combinations that cannot behave
// differently are dropped: no stop means no stop mode, trail or re-entry;
// one side means no cost/all modes; no re-entry means one re-entry mode; an
// intraday exit must come after the entry.
func (c *Config) Expand() ([]Cell, error) {
	var strikes, hedges []Rule
	for _, s := range c.Strike {
		r, err := ParseRule(s)
		if err != nil {
			return nil, err
		}
		strikes = append(strikes, r)
	}
	for _, s := range c.Hedge {
		if s == "none" {
			hedges = append(hedges, Rule{})
			continue
		}
		r, err := ParseRule(s)
		if err != nil {
			return nil, err
		}
		if r.Kind != KindOTM && r.Kind != KindCP {
			return nil, fmt.Errorf("hedge %q: only otm<pts> or cp<₹>", s)
		}
		hedges = append(hedges, r)
	}
	type trail struct{ x, y float64 }
	var trails []trail
	for _, s := range c.Trail {
		if s == "off" {
			trails = append(trails, trail{})
			continue
		}
		a, b, ok := strings.Cut(s, "/")
		x, err1 := strconv.ParseFloat(a, 64)
		y, err2 := strconv.ParseFloat(b, 64)
		if !ok || err1 != nil || err2 != nil || x <= 0 || y <= 0 {
			return nil, fmt.Errorf("trail %q: want X/Y", s)
		}
		trails = append(trails, trail{x, y})
	}
	var entries, exits []int
	for _, s := range c.Entry {
		m, err := parseHM(s)
		if err != nil {
			return nil, err
		}
		if m <= 0 {
			return nil, fmt.Errorf("entry %s: the 09:15 candle is not traded", s)
		}
		entries = append(entries, m)
	}
	for _, s := range c.Exit {
		m, err := parseHM(s)
		if err != nil {
			return nil, err
		}
		exits = append(exits, m)
	}
	for _, s := range c.Side {
		if s != "both" && s != "ce" && s != "pe" {
			return nil, fmt.Errorf("side %q", s)
		}
	}
	for _, s := range c.SLMode {
		if s != "leg" && s != "cost" && s != "all" {
			return nil, fmt.Errorf("sl_mode %q", s)
		}
	}
	for _, s := range c.ReentryMode {
		if s != "cost" && s != "reexec" {
			return nil, fmt.Errorf("reentry_mode %q", s)
		}
	}
	for _, s := range c.Hold {
		if s != "intraday" && s != "stbt" {
			return nil, fmt.Errorf("hold %q", s)
		}
	}
	for _, s := range c.SkipEvents {
		if _, ok := skipSets[s]; !ok {
			return nil, fmt.Errorf("skip_events %q: want none, india or all", s)
		}
	}
	for _, s := range c.Index {
		if _, ok := indices[s]; !ok {
			return nil, fmt.Errorf("index %q not supported", s)
		}
	}
	var cells []Cell
	seen := map[string]bool{}
	add := func(x Cell) {
		n := x.Name()
		if seen[n] {
			return
		}
		seen[n] = true
		x.ID = len(cells)
		cells = append(cells, x)
	}
	for _, ix := range c.Index {
		for _, dte := range c.DTE {
			for _, side := range c.Side {
				for _, st := range strikes {
					for _, rd := range c.Round {
						for _, en := range entries {
							for _, ex := range exits {
								for _, hold := range c.Hold {
									stbt := hold == "stbt"
									if !stbt && ex <= en {
										continue
									}
									for _, w := range c.Wait {
										for _, sl := range c.SL {
											for _, mode := range c.SLMode {
												for _, tr := range trails {
													for _, re := range c.Reentry {
														for _, rm := range c.ReentryMode {
															for _, h := range hedges {
																for _, sk := range c.SkipEvents {
																	x := Cell{Skip: sk, skipMask: skipSets[sk], Index: ix, DTE: dte, Side: side, Strike: st, Round: rd,
																		Entry: en, Exit: ex, STBT: stbt, Wait: w, SL: sl, SLMode: mode,
																		TrailX: tr.x, TrailY: tr.y, Reentry: re, ReentryMode: rm, Hedge: h}
																	if sl == 0 {
																		x.SLMode, x.TrailX, x.TrailY, x.Reentry = "leg", 0, 0, 0
																	}
																	if side != "both" {
																		x.SLMode = "leg"
																	}
																	if x.Reentry == 0 {
																		x.ReentryMode = "cost"
																	}
																	add(x)
																}
															}
														}
													}
												}
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	return cells, nil
}

// parseHM returns minutes after 09:15.
func parseHM(s string) (int, error) {
	h, m, ok := strings.Cut(s, ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil {
		return 0, fmt.Errorf("time %q: want HH:MM", s)
	}
	v := hh*60 + mm - (9*60 + 15)
	if v < 0 || v >= slots {
		return 0, fmt.Errorf("time %q outside 09:15–15:55", s)
	}
	return v, nil
}

func hhmm(m int) string {
	t := 9*60 + 15 + m
	return fmt.Sprintf("%02d%02d", t/60, t%60)
}
