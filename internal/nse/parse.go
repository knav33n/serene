package nse

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"calm/internal/market"
)

// ErrSchema means the file's columns changed, so nothing in it can be trusted.
var ErrSchema = errors.New("unexpected columns in index file")

var header = []string{"Index Name", "Index Date", "Open Index Value", "High Index Value", "Low Index Value",
	"Closing Index Value", "Points Change", "Change(%)", "Volume", "Turnover (Rs. Cr.)", "P/E", "P/B", "Div Yield"}

// number reads a field where "-" and blank mean no value. NSE writes .38 and
// -.15 without a leading zero, which ParseFloat accepts.
func number(s string) (float64, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	if s == "" || s == "-" {
		return math.NaN(), nil
	}
	return strconv.ParseFloat(s, 64)
}

func price(v float64) market.Paise {
	if math.IsNaN(v) {
		return 0
	}
	return market.Rupees(v)
}

func parseDate(s string) (market.Date, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"02-01-2006", "02-Jan-2006", "02-Jan-06", "02/01/2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return market.Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}, nil
		}
	}
	return market.Date{}, fmt.Errorf("unreadable date %q", s)
}

// sameDay allows for three April 2023 files written month first, such as
// 04-06-2023 in the file for 6 April. Their closes join their neighbours exactly.
func sameDay(d, want market.Date) bool {
	return d == want || (int(d.Month) == want.Day && d.Day == int(want.Month) && d.Year == want.Year)
}

// Parse reads a day's file. It rejects a file whose first row is dated other
// than the day requested. Rows with no close are skipped, because they mean the
// index wasn't computed that day. Rows carrying an older date are skipped too,
// as they are stale values rather than that day's close. Name is left equal to
// Source for the catalog to resolve.
func Parse(raw []byte, want market.Date) ([]market.IndexDay, error) {
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(raw, utf8BOM)))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	first, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	for i := range first {
		first[i] = strings.TrimSpace(first[i])
	}
	if !slices.Equal(first, header) {
		return nil, fmt.Errorf("%w: %q", ErrSchema, first)
	}
	var out []market.IndexDay
	checked := false
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
			continue
		}
		if len(rec) != len(header) {
			return nil, fmt.Errorf("%w: row has %d fields: %q", ErrSchema, len(rec), rec)
		}
		name := strings.TrimSpace(rec[0])
		d, err := parseDate(rec[1])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if !checked {
			if !sameDay(d, want) {
				return nil, fmt.Errorf("file for %s is dated %s", want, d)
			}
			checked = true
		}
		if !sameDay(d, want) {
			continue
		}
		v := make([]float64, 0, 11)
		for _, f := range rec[2:] {
			n, err := number(f)
			if err != nil {
				return nil, fmt.Errorf("%s: field %q: %w", name, f, err)
			}
			v = append(v, n)
		}
		o, h, l, cl, pts, vol, turnover, pe, pb, yld := v[0], v[1], v[2], v[3], v[4], v[6], v[7], v[8], v[9], v[10]
		if math.IsNaN(cl) || cl <= 0 {
			continue
		}
		row := market.IndexDay{Date: want, Name: name, Source: name, Open: price(o), High: price(h), Low: price(l),
			Close: market.Rupees(cl), TurnoverCr: turnover, PE: pe, PB: pb, Yield: yld}
		if !math.IsNaN(pts) {
			row.PrevClose = row.Close - market.Rupees(pts)
		}
		if !math.IsNaN(vol) {
			row.Volume = int64(vol)
		}
		out = append(out, row)
	}
	if !checked {
		return nil, errors.New("file has no rows")
	}
	return out, nil
}
