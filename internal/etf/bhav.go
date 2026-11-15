package etf

import (
	"database/sql"
	"strings"
	"time"

	"serene/internal/market"
)

// bhavDays returns the fund's bhavcopy days under all its ISINs and aliases,
// adjusted for splits to today's basis.
func (f *Fetcher) bhavDays(e ETF) ([]Bar, error) {
	rows, err := f.db.Query(`SELECT date, open, high, low, close, prev_close, volume
		FROM read_parquet('` + f.BhavGlob + `', hive_partitioning = false)
		WHERE series = 'EQ' AND (isin IN (` + quote(e.ISINs) + `) OR (symbol IN (` + quote(e.Symbols) + `) AND coalesce(isin, '') = ''))
		ORDER BY date, volume DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bar
	var prev []float64
	for rows.Next() {
		var d time.Time
		var o, h, l, c, pc, v sql.NullInt64
		if err := rows.Scan(&d, &o, &h, &l, &c, &pc, &v); err != nil {
			return nil, err
		}
		t := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, market.IST)
		if len(out) > 0 && out[len(out)-1].T.Equal(t) {
			continue // two listings on one day: keep the busier
		}
		out = append(out, Bar{t, rupees(o), rupees(h), rupees(l), rupees(c), v.Int64, "bhav"})
		prev = append(prev, rupees(pc))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	adjustSplits(out, prev)
	return out, nil
}

func rupees(paise sql.NullInt64) float64 { return float64(paise.Int64) / 100 }

func quote(ss []string) string {
	if len(ss) == 0 {
		return "''"
	}
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return strings.Join(q, ",")
}

// splitRatios are the ratios funds split by.
var splitRatios = []float64{2, 3, 4, 5, 10, 20, 25, 50, 100, 200, 250, 500, 1000}

// snap returns the split ratio within tol of r, or 0.
func snap(r, tol float64) float64 {
	for _, n := range splitRatios {
		if r >= 1.5 && abs(r-n) < tol*n {
			return n
		}
	}
	return 0
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// adjustSplits scales bars, oldest first, to the basis of the last. prev is
// each day's prev_close. NSE marks a split by quoting the day's prev_close at
// the new basis, so the day before's close over it is the ratio; rounding to
// the tick can make a 1:100 split read 99.4. A split onto a new ISIN carries
// the old, unadjusted prev_close instead, so there an overnight fall to a
// fifth or less of the price counts as a split too: no ETF trades like that.
func adjustSplits(bars []Bar, prev []float64) {
	f := 1.0
	for i := len(bars) - 1; i >= 0; i-- {
		raw := bars[i].Close
		if f != 1 {
			b := &bars[i]
			b.Open, b.High, b.Low, b.Close = b.Open/f, b.High/f, b.Low/f, b.Close/f
			b.Volume = int64(float64(b.Volume) * f)
			b.Source = "bhav_adj"
		}
		if i == 0 || bars[i-1].Close <= 0 {
			continue
		}
		before := bars[i-1].Close
		switch {
		case prev[i] > 0 && snap(before/prev[i], 0.04) > 0:
			f *= snap(before/prev[i], 0.04)
		case prev[i] > 0 && snap(prev[i]/before, 0.04) > 0:
			f /= snap(prev[i]/before, 0.04) // a consolidation
		case raw > 0 && before/raw >= 5 && snap(before/raw, 0.2) > 0:
			f *= snap(before/raw, 0.2)
		}
	}
}

// fill adds the bhav days broker lacks. Broker prices are split-adjusted; bhav
// is adjusted too, but an unseen split shows as bhav/broker near a whole
// ratio at the next day both have, so each added day is scaled by that.
func fill(broker, bh []Bar) ([]Bar, int) {
	if len(broker) == 0 {
		return bh, len(bh)
	}
	have := map[market.Date]float64{}
	for _, b := range broker {
		have[market.DateOf(b.T)] = b.Close
	}
	ratio := 1.0 // for days after the last overlap
	scale := make([]float64, len(bh))
	for i := len(bh) - 1; i >= 0; i-- {
		if k, ok := have[market.DateOf(bh[i].T)]; ok && k > 0 {
			ratio = 1
			if n := snap(bh[i].Close/k, 0.05); n > 0 {
				ratio = n
			}
		}
		scale[i] = ratio
	}
	out, n := broker, 0
	for i, b := range bh {
		if _, ok := have[market.DateOf(b.T)]; ok {
			continue
		}
		if s := scale[i]; s != 1 {
			b.Open, b.High, b.Low, b.Close = b.Open/s, b.High/s, b.Low/s, b.Close/s
			b.Volume = int64(float64(b.Volume) * s)
			b.Source = "bhav_adj"
		}
		out = append(out, b)
		n++
	}
	return out, n
}
