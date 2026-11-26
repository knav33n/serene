package optbt

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"sync"
	"time"

	"serene/internal/cost"
	"serene/internal/data"
	"serene/internal/market"

	_ "github.com/duckdb/duckdb-go/v2"
)

// Options control a run.
type Options struct {
	Workers int
	Detail  bool                      // keep every DayResult with its trades (for --trades)
	Log     io.Writer                 // progress; nil = quiet
	OnDay   func(c *Cell, row DayRow) // called in date order per cell when Detail is set
	VIX     map[time.Time]float64     // India VIX 1m opens, for DayRow; may be nil
}

// Run simulates every cell over the config's window and returns one Acc per
// cell, indexed by Cell.ID.
func Run(ctx context.Context, cfg *Config, cells []Cell, opt Options) ([]*Acc, error) {
	from, err := market.ParseDate(cfg.From)
	if err != nil {
		return nil, err
	}
	to, err := market.ParseDate(cfg.To)
	if err != nil {
		return nil, err
	}
	costs, err := cost.Load(cfg.Costs)
	if err != nil {
		return nil, err
	}
	store, err := data.Open(cfg.Data)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	if opt.Workers <= 0 {
		opt.Workers = 4
	}
	accs := make([]*Acc, len(cells))
	for i := range accs {
		accs[i] = &Acc{}
	}
	byIndex := map[string][]*Cell{}
	var order []string
	for i := range cells {
		c := &cells[i]
		if _, ok := byIndex[c.Index]; !ok {
			order = append(order, c.Index)
		}
		byIndex[c.Index] = append(byIndex[c.Index], c)
	}
	for _, name := range order {
		if err := runIndex(ctx, store, costs, cfg, name, byIndex[name], accs, from, to, opt); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	return accs, nil
}

func runIndex(ctx context.Context, store *data.Store, costs *cost.Schedule, cfg *Config, name string,
	cells []*Cell, accs []*Acc, from, to market.Date, opt Options) error {
	info := indices[name]
	insts, err := store.Contracts(ctx, name)
	if err != nil {
		return err
	}
	lots := make(map[string]market.Instrument, len(insts))
	for _, in := range insts {
		lots[in.Key()] = in
	}
	all, err := store.Days(name, market.Date{Year: 2000, Month: 1, Day: 1}, market.Date{Year: 2100, Month: 1, Day: 1})
	if err != nil {
		return err
	}
	ords := make(map[market.Date]int, len(all))
	var days []market.Date
	for i, d := range all {
		ords[d] = i
		if !d.Before(from) && !to.Before(d) {
			days = append(days, d)
		}
	}
	if len(days) == 0 {
		return fmt.Errorf("no stored days in %s..%s", from, to)
	}
	// the day after the window, for STBT exits
	if i := ords[days[len(days)-1]]; i+1 < len(all) {
		days = append(days, all[i+1])
	}
	var events map[market.Date]uint8
	for _, c := range cells {
		if c.skipMask != 0 {
			if events, err = loadEvents(cfg.Events, all); err != nil {
				return err
			}
			break
		}
	}
	byDTE := map[int][]*Cell{}
	for _, c := range cells {
		byDTE[c.DTE] = append(byDTE[c.DTE], c)
	}
	if opt.Log != nil {
		fmt.Fprintf(opt.Log, "%s: %d cells, %d days %s..%s\n", name, len(cells), len(days)-1, days[0], to)
	}

	type loaded struct {
		d   *Day
		err error
	}
	// load days ahead on several goroutines, handed out in date order
	const loaders = 3
	futs := make(chan chan loaded, 2*loaders)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		defer close(futs)
		sem := make(chan struct{}, loaders)
		for _, d := range days {
			f := make(chan loaded, 1)
			select {
			case futs <- f:
			case <-stop:
				return
			}
			sem <- struct{}{}
			go func(d market.Date) {
				defer func() { <-sem }()
				dy, err := loadDay(ctx, store, name, info, d, lots, ords)
				f <- loaded{dy, err}
			}(d)
		}
	}()
	recv := func() (*Day, error) {
		f, ok := <-futs
		if !ok {
			return nil, nil
		}
		l := <-f
		if l.d != nil {
			l.d.Events = events[l.d.Date]
		}
		return l.d, l.err
	}
	envs := make([]*Env, opt.Workers)
	for i := range envs {
		envs[i] = &Env{Costs: costs, SlipBps: cfg.SlipBps, NakedPct: cfg.NakedMarginPct, Detail: opt.Detail}
	}
	cur, err := recv()
	if err != nil {
		return err
	}
	start := time.Now()
	var prevClose market.Paise
	// days are consecutive stored days, so next is always the following one
	for di := 0; cur != nil && !to.Before(cur.Date); di++ {
		next, err := recv()
		if err != nil {
			return err
		}
		run := append(append([]*Cell(nil), byDTE[-1]...), byDTE[cur.DTE]...)
		if cur.DTE < 0 {
			run = byDTE[-1]
		}
		if opt.Detail {
			for _, c := range run {
				if r, ok := envs[0].Run(c, cur, next); ok {
					accs[c.ID].Add(&r)
					if opt.OnDay != nil {
						opt.OnDay(c, dayRow(c, cur, next, prevClose, &r, opt.VIX))
					}
				}
			}
		} else {
			var wg sync.WaitGroup
			chunk := (len(run) + opt.Workers - 1) / opt.Workers
			for w := 0; w < opt.Workers && w*chunk < len(run); w++ {
				part := run[w*chunk : min(len(run), (w+1)*chunk)]
				wg.Add(1)
				go func(e *Env, part []*Cell) {
					defer wg.Done()
					for _, c := range part {
						if r, ok := e.Run(c, cur, next); ok {
							accs[c.ID].Add(&r)
						}
					}
				}(envs[w], part)
			}
			wg.Wait()
		}
		if opt.Log != nil && (di == 19 || di%100 == 99) {
			el := time.Since(start)
			left := time.Duration(float64(el) / float64(di+1) * float64(len(days)-1-di-1))
			fmt.Fprintf(opt.Log, "%s %s %d/%d elapsed %s eta %s\n", name, cur.Date, di+1, len(days)-1,
				el.Round(time.Second), left.Round(time.Second))
		}
		if cur.Close > 0 {
			prevClose = cur.Index.c[cur.Close-1]
		}
		cur = next
	}
	return nil
}

func lastIndex(d *Day, slot int) market.Paise {
	for s := min(slot, slots) - 1; s >= 0; s-- {
		if d.Index.has[s] {
			return d.Index.c[s]
		}
	}
	return 0
}

func dayRow(c *Cell, d, next *Day, prevClose market.Paise, r *DayResult, vix map[time.Time]float64) DayRow {
	row := DayRow{R: r}
	if prevClose > 0 && d.Index.has[0] {
		row.Gap = (d.Index.o[0] - prevClose).Rupees()
	}
	end := lastIndex(d, c.Exit)
	if c.STBT && next != nil {
		end = lastIndex(next, c.Exit)
	}
	if end > 0 {
		row.SpotChange = (end - r.Spot).Rupees()
	}
	base := d.Date.At(9, 15)
	for s := 0; s < d.Close; s++ {
		v, ok := vix[base.Add(time.Duration(s)*time.Minute)]
		if !ok {
			continue
		}
		if row.VIXLo == 0 || v < row.VIXLo {
			row.VIXLo = v
		}
		row.VIXHi = max(row.VIXHi, v)
	}
	return row
}

// LoadVIX reads India VIX 1m opens keyed by bar start (IST).
func LoadVIX(dir string) (map[time.Time]float64, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(fmt.Sprintf(`select strftime(ts, '%%Y-%%m-%%d %%H:%%M'), open from '%s/key=India%%20VIX/bars.parquet'`, dir))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[time.Time]float64{}
	for rows.Next() {
		var ts string
		var o float64
		if err := rows.Scan(&ts, &o); err != nil {
			return nil, err
		}
		t, err := time.ParseInLocation("2006-01-02 15:04", ts, market.IST)
		if err != nil {
			return nil, err
		}
		m[t] = o
	}
	return m, rows.Err()
}
