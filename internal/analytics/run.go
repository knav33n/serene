package analytics

import (
	"context"
	"fmt"
	"runtime"
	"sync"

	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/engine"
	"serene/internal/margin"
	"serene/internal/market"
	"serene/internal/risk"
	"serene/internal/sim"
	"serene/internal/strategies"
	"serene/internal/strategy"
)

// Spec is one backtest.
type Spec struct {
	Strategy   string
	Underlying string
	Params     strategy.Params
	From, To   market.Date
	Capital    market.Paise
	Sim        sim.Config
	Margin     margin.Model
	Costs      *cost.Schedule
	// Closes holds official index closes by date. Nil settles expiries at the
	// last index bar, for data with no official closes.
	Closes map[market.Date]market.Paise
	// Risk sets pre-trade limits; nil runs with margin checks only. Each run
	// gets its own gate, so parallel sweeps do not share halts or order counts.
	Risk *risk.Limits
	// Regime is the market regime at each date's close, for strategies that
	// filter on it. It is shared read-only between parallel runs.
	Regime map[market.Date]string
}

// Outcome is a finished backtest.
type Outcome struct {
	Spec    Spec
	Result  engine.Result
	Metrics Metrics
}

// Run executes one backtest.
func Run(ctx context.Context, store *data.Store, s Spec) (Outcome, error) {
	contracts, err := store.Contracts(ctx, s.Underlying)
	if err != nil {
		return Outcome{}, err
	}
	feed, err := engine.NewStoreFeed(store, []string{s.Underlying}, s.From, s.To)
	if err != nil {
		return Outcome{}, err
	}
	if len(feed.TradingDays()) == 0 {
		return Outcome{}, fmt.Errorf("no %s data between %s and %s", s.Underlying, s.From, s.To)
	}
	strat, err := strategies.New(s.Strategy, s.Underlying, s.Params)
	if err != nil {
		return Outcome{}, err
	}
	if ru, ok := strat.(strategy.RegimeUser); ok && s.Regime != nil {
		ru.SetRegime(s.Regime)
	}
	b, err := sim.New(s.Sim)
	if err != nil {
		return Outcome{}, err
	}
	cfg := engine.Config{Contracts: contracts, Session: market.NSESession,
		Costs: s.Costs, Margin: s.Margin, Capital: s.Capital}
	if s.Closes != nil {
		idx := market.Index(market.NSE, s.Underlying).Key()
		cfg.IndexCloses = map[string]map[market.Date]market.Paise{idx: s.Closes}
	}
	if s.Risk != nil {
		if cfg.Risk, err = risk.New(*s.Risk); err != nil {
			return Outcome{}, err
		}
	}
	r, err := engine.New(cfg, feed, b, strat).Run(ctx)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Spec: s, Result: r, Metrics: Measure(r, b.Unfilled)}, nil
}

// SlippageRow is a backtest at scaled slippage.
type SlippageRow struct {
	Multiple float64
	Metrics  Metrics
}

// SlippageSensitivity reruns at multiples of the configured slippage. Bar data
// has no bid/ask, so the assumption can decide whether a strategy looks viable.
func SlippageSensitivity(ctx context.Context, store *data.Store, s Spec, multiples []float64) ([]SlippageRow, error) {
	rows := make([]SlippageRow, 0, len(multiples))
	for _, mult := range multiples {
		v := s
		v.Sim.SlippageBps *= mult
		v.Sim.MinTicks = int64(float64(s.Sim.MinTicks) * mult)
		o, err := Run(ctx, store, v)
		if err != nil {
			return nil, err
		}
		rows = append(rows, SlippageRow{mult, o.Metrics})
	}
	return rows, nil
}

// Trial is one parameter combination.
type Trial struct {
	Params  strategy.Params
	Metrics Metrics
}

// Grid expands parameter lists into every combination, in a fixed order.
func Grid(base strategy.Params, grid map[string][]float64) []strategy.Params {
	combos := []strategy.Params{{}}
	for k, v := range base {
		combos[0][k] = v
	}
	for _, name := range sortedKeys(grid) {
		var next []strategy.Params
		for _, c := range combos {
			for _, v := range grid[name] {
				p := strategy.Params{}
				for k, old := range c {
					p[k] = old
				}
				p[name] = v
				next = append(next, p)
			}
		}
		combos = next
	}
	return combos
}

// Sweep backtests every combination in parallel and returns trials in grid
// order, so results do not depend on scheduling.
func Sweep(ctx context.Context, store *data.Store, s Spec, combos []strategy.Params) ([]Trial, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := make([]Trial, len(combos))
	jobs := make(chan int)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				v := s
				v.Params = combos[i]
				o, err := Run(ctx, store, v)
				if err != nil {
					once.Do(func() { firstErr = err; cancel() })
					continue
				}
				out[i] = Trial{combos[i], o.Metrics}
			}
		}()
	}
	for i := range combos {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return out, firstErr
}

// Best returns the trial with the highest score. Trials that never traded are
// skipped, because they score zero rather than negative and would beat every
// losing combination.
func Best(trials []Trial, metric string) (Trial, bool) {
	var best Trial
	found := false
	for _, t := range trials {
		if t.Metrics.Trades == 0 {
			continue
		}
		if !found || t.Metrics.Score(metric) > best.Metrics.Score(metric) {
			best, found = t, true
		}
	}
	return best, found
}

// Window is one walk-forward step.
type Window struct {
	TrainFrom, TrainTo market.Date
	TestFrom, TestTo   market.Date
	Params             strategy.Params
	InSample           Metrics
	OutOfSample        Metrics
}

// WalkForward chooses parameters on each training window, then tests them on
// the following unseen window.
type WalkForward struct {
	Windows []Window
	// Combined is every test window joined together. Only these results were
	// not used to choose parameters.
	Combined Metrics
	Trials   int
}

// RunWalkForward rolls a train and test window over the available days.
func RunWalkForward(ctx context.Context, store *data.Store, s Spec, combos []strategy.Params,
	metric string, trainDays, testDays int) (WalkForward, error) {
	days, err := store.Days(s.Underlying, s.From, s.To)
	if err != nil {
		return WalkForward{}, err
	}
	if trainDays < 1 || testDays < 1 {
		return WalkForward{}, fmt.Errorf("walk-forward needs positive window sizes")
	}
	if len(days) < trainDays+testDays {
		return WalkForward{}, fmt.Errorf("walk-forward needs %d days, have %d", trainDays+testDays, len(days))
	}
	wf := WalkForward{Trials: len(combos)}
	joined := engine.Result{Capital: s.Capital}
	equity := s.Capital
	var unfilled int
	peak := s.Capital
	for start := 0; start+trainDays+testDays <= len(days); start += testDays {
		w := Window{
			TrainFrom: days[start], TrainTo: days[start+trainDays-1],
			TestFrom: days[start+trainDays], TestTo: days[start+trainDays+testDays-1],
		}
		train := s
		train.From, train.To = w.TrainFrom, w.TrainTo
		trials, err := Sweep(ctx, store, train, combos)
		if err != nil {
			return wf, fmt.Errorf("train %s: %w", w.TrainFrom, err)
		}
		best, ok := Best(trials, metric)
		if !ok {
			continue // nothing traded in this window
		}
		w.Params, w.InSample = best.Params, best.Metrics

		test := s
		test.Params = best.Params
		test.From, test.To = w.TestFrom, w.TestTo
		o, err := Run(ctx, store, test)
		if err != nil {
			return wf, fmt.Errorf("test %s: %w", w.TestFrom, err)
		}
		w.OutOfSample = o.Metrics
		wf.Windows = append(wf.Windows, w)

		// Each test starts from the same capital, so P&L joins additively.
		for _, p := range o.Result.Equity {
			joined.Equity = append(joined.Equity, engine.EquityPoint{Date: p.Date, Equity: equity + p.Equity - s.Capital})
		}
		joined.Trades = append(joined.Trades, o.Result.Trades...)
		joined.Charges += o.Result.Charges
		joined.Rejected += o.Result.Rejected
		unfilled += o.Metrics.Unfilled
		equity += o.Metrics.Net
		peak = max(peak, equity)
		joined.MaxDrawdown = max(joined.MaxDrawdown, o.Result.MaxDrawdown, peak-equity)
	}
	wf.Combined = Measure(joined, unfilled)
	return wf, nil
}
