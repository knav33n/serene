package optbt

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
)

// Row is one cell's line in the grid report.
type Row struct {
	Cell *Cell
	S    Summary
	Pass bool // MDD % under the limit
}

// Rank summarises every cell and sorts: passing MDD first, then by t.
func Rank(cells []Cell, accs []*Acc, mddMax float64, minN int) []Row {
	var rows []Row
	for i := range cells {
		s := accs[i].Summary()
		if s.N < minN {
			continue
		}
		rows = append(rows, Row{&cells[i], s, s.MDDPct < mddMax})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Pass != rows[j].Pass {
			return rows[i].Pass
		}
		return rows[i].S.T > rows[j].S.T
	})
	return rows
}

func years(rows []Row) []int {
	set := map[int]bool{}
	for _, r := range rows {
		for y := range r.S.ROI {
			set[y] = true
		}
	}
	var ys []int
	for y := range set {
		ys = append(ys, y)
	}
	sort.Ints(ys)
	return ys
}

func ff(x float64, p int) string {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return ""
	}
	return strconv.FormatFloat(x, 'f', p, 64)
}

// WriteGrid writes one CSV row per cell, metrics first in the order of the
// strategy-metrics standard, then the cell's parameters.
func WriteGrid(w io.Writer, rows []Row) error {
	ys := years(rows)
	cw := csv.NewWriter(w)
	head := []string{"cell", "pass_mdd", "mdd_pct", "rec_ratio", "expectancy", "exp_r", "win_pct"}
	for _, y := range ys {
		head = append(head, fmt.Sprintf("roi_%d", y))
	}
	head = append(head, "roi_min", "roi_cv", "n", "t", "pf", "net_slip", "net", "gross", "avg_win", "avg_loss",
		"worst", "best", "capital", "rom_pct", "mdd", "mdd_days", "rec_days", "ce_sl_pct", "pe_sl_pct", "both_sl_pct",
		"reentries_per_day", "max_win_run", "max_loss_run",
		"index", "dte", "side", "strike", "round", "entry", "exit", "hold", "wait", "sl", "sl_mode", "trail",
		"reentry", "reentry_mode", "hedge")
	if err := cw.Write(head); err != nil {
		return err
	}
	for _, r := range rows {
		s, c := r.S, r.Cell
		rec := []string{c.Name(), strconv.FormatBool(r.Pass), ff(s.MDDPct, 2), ff(s.RecRatio(), 2),
			ff(s.Avg, 0), ff(s.ExpR, 3), ff(s.Win, 1)}
		for _, y := range ys {
			v, ok := s.ROI[y]
			if !ok {
				rec = append(rec, "")
				continue
			}
			rec = append(rec, ff(v, 1))
		}
		hold, trail, hedge := "intraday", "off", "none"
		if c.STBT {
			hold = "stbt"
		}
		if c.TrailX > 0 {
			trail = fmt.Sprintf("%g/%g", c.TrailX, c.TrailY)
		}
		if c.Hedge.Kind != "" {
			hedge = c.Hedge.Name
		}
		rec = append(rec, ff(s.ROIMin, 1), ff(s.ROICV, 2), strconv.Itoa(s.N), ff(s.T, 2), ff(s.PF, 2),
			ff(s.NetSlip, 0), ff(s.Net, 0), ff(s.Gross, 0), ff(s.AvgWin, 0), ff(-s.AvgLoss, 0),
			ff(s.Worst, 0), ff(s.Best, 0), ff(s.Capital, 0), ff(s.ROM, 3), ff(s.MDD, 0),
			strconv.Itoa(s.MDDDays), strconv.Itoa(s.RecDays), ff(s.CESL, 1), ff(s.PESL, 1), ff(s.BothSL, 1),
			ff(s.ReentPerDay, 2), strconv.Itoa(s.MaxWinRun), strconv.Itoa(s.MaxLossRun),
			c.Index, strconv.Itoa(c.DTE), c.Side, c.Strike.Name, strconv.Itoa(c.Round), hhmm(c.Entry), hhmm(c.Exit),
			hold, ff(c.Wait, 1), ff(c.SL, 1), c.SLMode, trail, strconv.Itoa(c.Reentry), c.ReentryMode, hedge)
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// DayRow is one day of a single cell, StockMock-style.
type DayRow struct {
	R          *DayResult
	VIXLo      float64 // India VIX range over the day; 0 = unknown
	VIXHi      float64
	Gap        float64 // index open − previous close, points
	SpotChange float64 // index at exit − at entry, points
}

// WriteDays writes the per-day table for one cell, then one line per trade.
func WriteDays(w io.Writer, rows []DayRow) error {
	cw := csv.NewWriter(w)
	cw.Write([]string{"date", "dte", "expiry", "net_slip", "net", "gross", "max_profit", "max_loss", "margin",
		"vix_lo", "vix_hi", "gap", "spot", "spot_change", "ce_sl", "pe_sl", "reentries", "trades"})
	for _, d := range rows {
		r := d.R
		var legs string
		for i, t := range r.Trades {
			if i > 0 {
				legs += " | "
			}
			side := "S"
			if t.Hedge {
				side = "B"
			}
			legs += fmt.Sprintf("%s %s%s %s@%s→%s@%s %s", side, t.Inst.Strike, t.Inst.Right,
				t.EntryT.Format("15:04"), t.Entry, t.ExitT.Format("01-02 15:04"), t.Exit, t.Reason)
		}
		cw.Write([]string{r.Date.String(), strconv.Itoa(r.DTE), r.Expiry.String(), ff(r.NetSlip, 0), ff(r.Net, 0),
			ff(r.Gross, 0), ff(r.MaxProfit, 0), ff(r.MaxLoss, 0), ff(r.Margin, 0), ff(d.VIXLo, 2), ff(d.VIXHi, 2),
			ff(d.Gap, 1), r.Spot.String(), ff(d.SpotChange, 1), strconv.FormatBool(r.CESL), strconv.FormatBool(r.PESL),
			strconv.Itoa(r.Reentries), legs})
	}
	cw.Flush()
	return cw.Error()
}
