package strategies

import (
	"fmt"
	"math"
	"time"

	"serene/internal/indicator"
	"serene/internal/market"
	"serene/internal/strategy"
)

// rsiDivergence buys the at-the-money call or put when its own chart, on
// timeframe bars, shows a bullish RSI divergence: a swing low below the
// previous swing low of the day while the RSI at it is above the RSI at that
// earlier low. The signal is the close of the bar that confirms the second low
// (pivot_bars after it). The stop is that low; the exits are the stop, a
// target at target_r times the risk, an RSI exit level, a holding time and the
// time exit. Call and put trade independently, so a bearish divergence on the
// index shows up as a bullish one on the put.
//
// With chart 1 the divergence is read on the index instead: a bullish one
// buys the call and a bearish one (a higher swing high with a lower RSI, the
// RSI at the first high above 100-oversold) buys the put. The RSI runs on the
// index's own timeframe bars, carried across days. The stop is then the
// option's own low since the index's second extreme, and the RSI exit is the
// index RSI reaching rsi_exit for calls or 100-rsi_exit for puts.
//
// With hidden 1 it trades hidden divergence instead, a sign the trend goes
// on: a higher low with a lower RSI buys (the call, on the index chart), and
// on the index a lower high with a higher RSI buys the put. trend_ema then
// requires the close above that EMA for a buy, and below it for the put.
//
// On the index chart, confirm_ema makes a divergence wait for a close on the
// trade's side of that EMA of the index (above for a call, below for a put),
// on the signal bar or one of the next confirm_bars bars; otherwise it lapses.
//
// The strike, warm-up and fills work as in lower_high_break: the day's index
// open rounded to step in the nearest expiry, the RSI warmed on the previous
// afternoon's bars of that contract, stops and targets checked on 1-minute
// bars, and market orders filled in the next minute. Swing lows are only taken
// from today's bars.
type rsiDivergence struct {
	underlying string
	chart      int // 0 the option's own chart, 1 the index
	tf         market.Timeframe
	rsiN       int
	k          int
	minGap     int // bars between the two lows
	maxGap     int
	oversold   float64 // RSI at the first low must be below this; 0 off
	minDrop    float64 // second low at least this % below the first
	hidden     bool
	trendEMA   int // 0 off
	confirmEMA int // 0 off
	confirmN   int
	targetR    float64 // 0 off
	rsiExit    float64 // 0 off
	hold       time.Duration
	maxTrades  int // per leg per day; 0 no limit
	flat       int
	capital    float64
	allocPct   float64
	maxLots    int64
	step       float64
	warmFrom   int
	warmBand   int

	day      market.Date
	legs     map[string]*rdLeg
	chosen   bool
	warming  bool
	warmKeys map[string]bool
	warm     map[string][]market.Bar
	prevWarm map[string][]market.Bar

	// Index chart: timeframe bars carried across days, with today's
	// swing lows and highs as indexes into them.
	ir      *market.Resampler
	ibars   []market.Bar
	iday    int // first of today's bars
	ilows   []int
	ihighs  []int
	waiting map[market.Right]rdWait // divergences waiting for the EMA close
}

type rdLeg struct {
	inst market.Instrument
	tag  string
	r    *market.Resampler
	warm []market.Bar
	bars []market.Bar
	lows []int // swing lows of today's bars, by index

	trades     int
	open       bool
	entryAt    time.Time
	stop       market.Paise
	target     market.Paise
	nextStop   market.Paise
	nextTarget market.Paise
}

var rsiDivParams = []string{"chart", "timeframe", "rsi", "pivot_bars", "min_gap", "max_gap", "oversold", "min_drop_pct", "hidden", "trend_ema", "confirm_ema", "confirm_bars",
	"target_r", "rsi_exit", "hold_minutes", "max_trades", "exit_minute", "size_capital", "alloc_pct", "max_lots",
	"strike_step", "warm_from_minute", "warm_strikes"}

func newRSIDivergence(u string, p strategy.Params) (strategy.Strategy, error) {
	if err := p.Check(rsiDivParams...); err != nil {
		return nil, err
	}
	s := &rsiDivergence{underlying: u,
		chart:      int(p.Get("chart", 0)),
		tf:         market.Timeframe(p.Get("timeframe", 5)),
		rsiN:       int(p.Get("rsi", 14)),
		k:          int(p.Get("pivot_bars", 2)),
		minGap:     int(p.Get("min_gap", 3)),
		maxGap:     int(p.Get("max_gap", 24)),
		oversold:   p.Get("oversold", 40),
		minDrop:    p.Get("min_drop_pct", 0),
		hidden:     p.Get("hidden", 0) != 0,
		trendEMA:   int(p.Get("trend_ema", 0)),
		confirmEMA: int(p.Get("confirm_ema", 0)),
		confirmN:   int(p.Get("confirm_bars", 4)),
		targetR:    p.Get("target_r", 2),
		rsiExit:    p.Get("rsi_exit", 0),
		hold:       time.Duration(p.Get("hold_minutes", 0)) * time.Minute,
		maxTrades:  int(p.Get("max_trades", 0)),
		flat:       int(p.Get("exit_minute", 360)),
		capital:    p.Get("size_capital", 100000),
		allocPct:   p.Get("alloc_pct", 25),
		maxLots:    int64(p.Get("max_lots", 5)),
		step:       p.Get("strike_step", 100),
		warmFrom:   int(p.Get("warm_from_minute", 165)),
		warmBand:   int(p.Get("warm_strikes", 4)),
	}
	if s.hidden && s.oversold > 0 {
		return nil, fmt.Errorf("rsi_divergence: oversold applies to regular divergence only; set it to 0 with hidden")
	}
	if s.confirmEMA > 0 && s.chart != 1 {
		return nil, fmt.Errorf("rsi_divergence: confirm_ema needs chart 1")
	}
	if s.confirmEMA < 0 || s.confirmN < 0 || s.trendEMA < 0 || s.chart < 0 || s.chart > 1 || s.tf < 1 || s.rsiN < 2 || s.k < 1 || s.minGap < 1 || s.maxGap < s.minGap || s.oversold < 0 || s.minDrop < 0 ||
		s.targetR < 0 || s.rsiExit < 0 || s.maxTrades < 0 || s.maxLots < 1 || s.allocPct <= 0 || s.step <= 0 || s.flat < 1 {
		return nil, fmt.Errorf("rsi_divergence: bad parameters %+v", p)
	}
	return s, nil
}

func (s *rsiDivergence) Init(ctx strategy.Context) error {
	return ctx.Subscribe(indexKey(s.underlying), market.Minute1)
}

func (s *rsiDivergence) OnBar(ctx strategy.Context, b market.Bar) error {
	if d := market.DateOf(b.Time); d != s.day {
		s.day, s.legs, s.chosen, s.warming = d, map[string]*rdLeg{}, false, false
		s.prevWarm, s.warm, s.warmKeys = s.warm, map[string][]market.Bar{}, map[string]bool{}
		if s.chart == 1 {
			if err := s.newIndexDay(); err != nil {
				return err
			}
		}
	}
	if b.Timeframe != market.Minute1 {
		return nil
	}
	minute := minutesIntoSession(b)
	if s.warmKeys[b.Key] && minute > s.warmFrom {
		s.warm[b.Key] = append(s.warm[b.Key], b)
	}
	if b.Key == indexKey(s.underlying) {
		if !s.chosen {
			s.chosen = true
			if err := s.choose(ctx, b); err != nil {
				return err
			}
		}
		if !s.warming && minute >= s.warmFrom {
			s.warming = true
			if err := s.subscribeWarm(ctx, b.Close); err != nil {
				return err
			}
		}
		if s.chart == 1 {
			done, err := s.ir.Add(b)
			if err != nil {
				return err
			}
			for _, tb := range done {
				s.ibars = append(s.ibars, tb)
				if err := s.onIndexBar(ctx, b, minutesIntoSession(tb)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	leg := s.legs[b.Key]
	if leg == nil {
		return nil
	}
	if err := s.checkExit(ctx, leg, b, minute, false); err != nil {
		return err
	}
	done, err := leg.r.Add(b)
	if err != nil {
		return err
	}
	for _, tb := range done {
		leg.bars = append(leg.bars, tb)
		if err := s.onTimeframe(ctx, leg, b, minutesIntoSession(tb)); err != nil {
			return err
		}
	}
	return nil
}

func (s *rsiDivergence) choose(ctx strategy.Context, first market.Bar) error {
	expiries := ctx.Expiries(s.underlying)
	if len(expiries) == 0 {
		return nil
	}
	strike := market.Rupees(math.Round(first.Open.Rupees()/s.step) * s.step)
	for _, q := range ctx.Chain(s.underlying, expiries[0]) {
		if q.Instrument.Strike != strike {
			continue
		}
		key := q.Instrument.Key()
		r, err := market.NewResampler(market.NSESession, s.tf)
		if err != nil {
			return err
		}
		leg := &rdLeg{inst: q.Instrument, r: r, warm: resampled(s.prevWarm[key], s.tf),
			tag: fmt.Sprintf("rsid-%s-%s", s.day, q.Instrument.Right)}
		if err := ctx.Subscribe(key, market.Minute1); err != nil {
			return err
		}
		if last, ok := ctx.Last(key); ok && market.DateOf(last.Time) == s.day && !last.Time.After(first.Time) {
			if _, err := r.Add(last); err != nil {
				return err
			}
		}
		s.legs[key] = leg
	}
	return nil
}

func (s *rsiDivergence) subscribeWarm(ctx strategy.Context, spot market.Paise) error {
	if s.chart == 1 {
		return nil // the index RSI carries over on its own
	}
	atm := math.Round(spot.Rupees()/s.step) * s.step
	for i, exp := range ctx.Expiries(s.underlying) {
		if i == 2 {
			break
		}
		for _, q := range ctx.Chain(s.underlying, exp) {
			if math.Abs(q.Instrument.Strike.Rupees()-atm) > float64(s.warmBand)*s.step+0.5 {
				continue
			}
			key := q.Instrument.Key()
			if err := ctx.Subscribe(key, market.Minute1); err != nil {
				return err
			}
			s.warmKeys[key] = true
		}
	}
	return nil
}

// checkExit sells an open leg on its stop, target, holding time, time exit,
// or, when rsiOut is set, the RSI exit decided on the timeframe close.
func (s *rsiDivergence) checkExit(ctx strategy.Context, leg *rdLeg, b market.Bar, minute int, rsiOut bool) error {
	if !leg.open || ctx.Pending() > 0 {
		return nil
	}
	stopped := leg.stop > 0 && b.Low < leg.stop
	hit := leg.target > 0 && b.High >= leg.target
	held := s.hold > 0 && b.End.Sub(leg.entryAt) >= s.hold
	if !stopped && !hit && !held && !rsiOut && minute < s.flat {
		return nil
	}
	for _, p := range ctx.Positions() {
		if p.Instrument.Key() == leg.inst.Key() && p.Qty > 0 {
			return ignoreRejection(ctx.Submit(leg.tag, market.Leg{Instrument: p.Instrument, Side: market.Sell, Lots: p.Qty / p.Instrument.LotSize}))
		}
	}
	return nil
}

func (s *rsiDivergence) rsi(leg *rdLeg) []float64 {
	series := append(append([]market.Bar(nil), leg.warm...), leg.bars...)
	return indicator.RSI(series, s.rsiN)[len(leg.warm):]
}

// isLow reports whether bar p's low is strictly below the k bars each side.
func isLow(bars []market.Bar, p, k int) bool {
	if p-k < 0 || p+k >= len(bars) {
		return false
	}
	for j := p - k; j <= p+k; j++ {
		if j != p && bars[j].Low <= bars[p].Low {
			return false
		}
	}
	return true
}

// onTimeframe records the swing low the newest bar confirms and enters a flat
// leg on a bullish divergence, or exits an open one on the RSI exit level.
func (s *rsiDivergence) onTimeframe(ctx strategy.Context, leg *rdLeg, b market.Bar, minute int) error {
	i := len(leg.bars) - 1
	rsi := s.rsi(leg)
	if s.chart == 1 {
		return nil
	}
	if leg.open {
		if s.rsiExit > 0 && !math.IsNaN(rsi[i]) && rsi[i] >= s.rsiExit {
			return s.checkExit(ctx, leg, b, minute, true)
		}
		return nil
	}
	p := i - s.k
	if !isLow(leg.bars, p, s.k) {
		return nil
	}
	var q = -1
	if n := len(leg.lows); n > 0 {
		q = leg.lows[n-1]
	}
	leg.lows = append(leg.lows, p)
	if q < 0 || ctx.Pending() > 0 || minute >= s.flat || (s.maxTrades > 0 && leg.trades >= s.maxTrades) {
		return nil
	}
	if !s.diverges(rsi, p, q, leg.bars[p].Low.Rupees(), leg.bars[q].Low.Rupees(), false) {
		return nil
	}
	if s.trendEMA > 0 {
		series := append(append([]market.Bar(nil), leg.warm...), leg.bars...)
		ema := indicator.EMA(series, s.trendEMA)[len(leg.warm):]
		if math.IsNaN(ema[i]) || leg.bars[i].Close.Rupees() <= ema[i] {
			return nil
		}
	}
	return s.enter(ctx, leg, leg.bars[i].Close, leg.bars[p].Low)
}

// enter buys a leg at market, sized on close, with the stop and a target at
// target_r times the risk to it.
func (s *rsiDivergence) enter(ctx strategy.Context, leg *rdLeg, close, stop market.Paise) error {
	risk := close - stop
	if risk <= 0 || close <= 0 {
		return nil
	}
	cost := close.Rupees() * float64(leg.inst.LotSize)
	lots := min(s.maxLots, max(1, int64(s.capital*s.allocPct/100/cost)))
	leg.nextStop, leg.nextTarget = stop, 0
	if s.targetR > 0 {
		leg.nextTarget = close + market.Paise(math.Round(float64(risk)*s.targetR))
	}
	return ignoreRejection(ctx.Submit(leg.tag, market.Leg{Instrument: leg.inst, Side: market.Buy, Lots: lots}))
}

// newIndexDay starts the day's index bars, keeping enough of the past ones
// for the RSI to be settled at the open.
func (s *rsiDivergence) newIndexDay() error {
	r, err := market.NewResampler(market.NSESession, s.tf)
	if err != nil {
		return err
	}
	if keep := max(10*s.rsiN, 4*s.trendEMA); len(s.ibars) > keep {
		s.ibars = append([]market.Bar(nil), s.ibars[len(s.ibars)-keep:]...)
	}
	s.ir, s.iday, s.ilows, s.ihighs, s.waiting = r, len(s.ibars), nil, nil, map[market.Right]rdWait{}
	return nil
}

// isHigh reports whether bar p's high is strictly above the k bars each side.
func isHigh(bars []market.Bar, p, k int) bool {
	if p-k < 0 || p+k >= len(bars) {
		return false
	}
	for j := p - k; j <= p+k; j++ {
		if j != p && bars[j].High >= bars[p].High {
			return false
		}
	}
	return true
}

// legFor returns today's call or put.
func (s *rsiDivergence) legFor(r market.Right) *rdLeg {
	for _, l := range s.legs {
		if l.inst.Right == r {
			return l
		}
	}
	return nil
}

// onIndexBar handles a new index timeframe bar: the RSI exits, and an entry
// on a bullish (call) or bearish (put) divergence it confirms.
func (s *rsiDivergence) onIndexBar(ctx strategy.Context, b market.Bar, minute int) error {
	i := len(s.ibars) - 1
	rsi := indicator.RSI(s.ibars, s.rsiN)
	if s.rsiExit > 0 && !math.IsNaN(rsi[i]) {
		if l := s.legFor(market.Call); l != nil && l.open && rsi[i] >= s.rsiExit {
			if err := s.checkExit(ctx, l, b, minute, true); err != nil {
				return err
			}
		}
		if l := s.legFor(market.Put); l != nil && l.open && rsi[i] <= 100-s.rsiExit {
			if err := s.checkExit(ctx, l, b, minute, true); err != nil {
				return err
			}
		}
	}
	p := i - s.k
	if p < s.iday {
		return s.confirm(ctx, minute)
	}
	up, down := true, true
	if s.trendEMA > 0 {
		ema := indicator.EMA(s.ibars, s.trendEMA)[i]
		c := s.ibars[i].Close.Rupees()
		up, down = !math.IsNaN(ema) && c > ema, !math.IsNaN(ema) && c < ema
	}
	if isLow(s.ibars[s.iday:], p-s.iday, s.k) {
		q := -1
		if n := len(s.ilows); n > 0 {
			q = s.ilows[n-1]
		}
		s.ilows = append(s.ilows, p)
		if q >= 0 && up && s.diverges(rsi, p, q, s.ibars[p].Low.Rupees(), s.ibars[q].Low.Rupees(), false) {
			s.waiting[market.Call] = rdWait{p: p, until: i + s.confirmN}
		}
	}
	if isHigh(s.ibars[s.iday:], p-s.iday, s.k) {
		q := -1
		if n := len(s.ihighs); n > 0 {
			q = s.ihighs[n-1]
		}
		s.ihighs = append(s.ihighs, p)
		if q >= 0 && down && s.diverges(rsi, p, q, s.ibars[p].High.Rupees(), s.ibars[q].High.Rupees(), true) {
			s.waiting[market.Put] = rdWait{p: p, until: i + s.confirmN}
		}
	}
	return s.confirm(ctx, minute)
}

// rdWait is a divergence on the index waiting for its confirmation.
type rdWait struct{ p, until int }

// confirm enters each waiting divergence whose confirmation the newest index
// bar gives, and drops those past their last bar. Without confirm_ema every
// divergence enters at once.
func (s *rsiDivergence) confirm(ctx strategy.Context, minute int) error {
	i := len(s.ibars) - 1
	ema := math.NaN()
	if s.confirmEMA > 0 {
		ema = indicator.EMA(s.ibars, s.confirmEMA)[i]
	}
	c := s.ibars[i].Close.Rupees()
	for _, r := range []market.Right{market.Call, market.Put} {
		w, ok := s.waiting[r]
		if !ok {
			continue
		}
		if s.confirmEMA > 0 && !(r == market.Call && c > ema) && !(r == market.Put && c < ema) {
			if i >= w.until {
				delete(s.waiting, r)
			}
			continue
		}
		delete(s.waiting, r)
		if err := s.enterOnIndex(ctx, r, w.p, minute); err != nil {
			return err
		}
	}
	return nil
}

// diverges tests the extreme at p against the earlier one at q: a lower low
// with a higher RSI, or for highs a higher high with a lower RSI. Hidden
// divergence is the reverse: a higher low with a lower RSI, or a lower high
// with a higher RSI.
func (s *rsiDivergence) diverges(rsi []float64, p, q int, at, before float64, high bool) bool {
	if gap := p - q; gap < s.minGap || gap > s.maxGap || math.IsNaN(rsi[p]) || math.IsNaN(rsi[q]) {
		return false
	}
	if s.hidden {
		if high {
			return at < before*(1-s.minDrop/100) && rsi[p] > rsi[q]
		}
		return at > before*(1+s.minDrop/100) && rsi[p] < rsi[q]
	}
	if high {
		return at > before*(1+s.minDrop/100) && rsi[p] < rsi[q] && (s.oversold == 0 || rsi[q] > 100-s.oversold)
	}
	return at < before*(1-s.minDrop/100) && rsi[p] > rsi[q] && (s.oversold == 0 || rsi[q] < s.oversold)
}

// enterOnIndex buys the call or put, with the stop at the option's own low
// since the index extreme at p.
func (s *rsiDivergence) enterOnIndex(ctx strategy.Context, r market.Right, p, minute int) error {
	leg := s.legFor(r)
	if leg == nil || leg.open || ctx.Pending() > 0 || minute >= s.flat || (s.maxTrades > 0 && leg.trades >= s.maxTrades) {
		return nil
	}
	var stop market.Paise
	from := s.ibars[p].Time
	for _, ob := range leg.bars {
		if !ob.Time.Before(from) && (stop == 0 || ob.Low < stop) {
			stop = ob.Low
		}
	}
	last, ok := ctx.Last(leg.inst.Key())
	if !ok || stop == 0 || market.DateOf(last.Time) != s.day {
		return nil
	}
	return s.enter(ctx, leg, last.Close, stop)
}

func (s *rsiDivergence) OnFill(_ strategy.Context, f market.Fill) error {
	leg := s.legs[f.Instrument.Key()]
	if leg == nil {
		return nil
	}
	if f.Kind == market.Traded && f.Side == market.Buy {
		leg.open, leg.entryAt, leg.trades = true, f.Time, leg.trades+1
		leg.stop, leg.target = leg.nextStop, leg.nextTarget
		return nil
	}
	leg.open, leg.stop, leg.target = false, 0, 0
	return nil
}
