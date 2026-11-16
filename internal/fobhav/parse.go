// Package fobhav reads the exchanges' daily F&O bhavcopies for what the Fyers
// history service doesn't say: each expiry's lot size, which stocks ever had
// options, the strikes each expiry listed, and an unadjusted price to choose
// strikes around (Fyers' stock spot history is split-adjusted, so a 2021
// IRCTC strike of 3700 sits far from its adjusted spot of 740).
//
// NSE publishes two formats. The legacy foDDMONYYYYbhav.csv runs to
// 2024-07-05 and carries no lot size, so lots are worked out from futures
// turnover. The UDiFF BhavCopy_NSE_FO file, from 2024-07-08, states the lot
// in NewBrdLotQty. BSE's derivatives bhavcopy uses the same UDiFF layout.
package fobhav

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"serene/internal/market"
)

// Row is one contract's day in a bhavcopy.
type Row struct {
	Date    market.Date
	Future  bool // a future; otherwise an option
	Index   bool // on an index; otherwise on a stock
	Symbol  string
	Expiry  market.Date
	Strike  float64 // options only
	Right   string  // CE or PE; empty for futures
	High    float64
	Low     float64
	Close   float64
	Settle  float64
	Volume  float64 // contracts traded (legacy only)
	ValueRs float64 // turnover in rupees (legacy only)
	Lot     int64   // the lot in force (UDiFF only)
}

// ErrSchema means a file doesn't have the columns expected.
var ErrSchema = errors.New("unexpected bhavcopy layout")

// Parse reads a day's CSV in either format. Rows of instruments other than
// index and stock futures and options, such as India VIX futures, are left out.
func Parse(body []byte, want market.Date) ([]Row, error) {
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})))
	r.FieldsPerRecord = -1
	head, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	col := map[string]int{}
	for i, h := range head {
		if h = strings.TrimSpace(h); h != "" {
			col[h] = i
		}
	}
	_, isUDiFF := col["TckrSymb"]
	var names []string
	if isUDiFF {
		names = []string{"TradDt", "FinInstrmTp", "TckrSymb", "XpryDt", "StrkPric", "OptnTp", "HghPric", "LwPric",
			"ClsPric", "SttlmPric", "NewBrdLotQty"}
	} else {
		names = []string{"TIMESTAMP", "INSTRUMENT", "SYMBOL", "EXPIRY_DT", "STRIKE_PR", "OPTION_TYP", "HIGH", "LOW",
			"CLOSE", "SETTLE_PR", "CONTRACTS", "VAL_INLAKH"}
	}
	idx := make([]int, len(names))
	for i, n := range names {
		j, ok := col[n]
		if !ok {
			return nil, fmt.Errorf("%w: no %s column in %q", ErrSchema, n, head)
		}
		idx[i] = j
	}
	field := func(rec []string, i int) string {
		if idx[i] >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[idx[i]])
	}
	dateFormats := []string{"02-Jan-2006", "2-Jan-2006", "02-Jan-06"}
	if isUDiFF {
		dateFormats = []string{"2006-01-02"}
	}

	var out []Row
	for line := 2; ; line++ {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
			continue
		}
		var row Row
		switch field(rec, 1) {
		case "FUTIDX", "IDF":
			row.Future, row.Index = true, true
		case "FUTSTK", "STF":
			row.Future = true
		case "OPTIDX", "IDO":
			row.Index = true
		case "OPTSTK", "STO":
		default:
			continue
		}
		if row.Date, err = parseDate(field(rec, 0), dateFormats); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if row.Date != want {
			return nil, fmt.Errorf("file for %s has a row dated %s", want, row.Date)
		}
		if row.Expiry, err = parseDate(field(rec, 3), dateFormats); err != nil {
			return nil, fmt.Errorf("line %d expiry: %w", line, err)
		}
		row.Symbol = field(rec, 2)
		if row.Symbol == "" {
			return nil, fmt.Errorf("line %d has no symbol", line)
		}
		nums := []*float64{&row.Strike, nil, &row.High, &row.Low, &row.Close, &row.Settle}
		for k, p := range nums {
			if p == nil {
				continue
			}
			if *p, err = number(field(rec, 4+k)); err != nil {
				return nil, fmt.Errorf("line %d %s: %w", line, names[4+k], err)
			}
		}
		if !row.Future {
			row.Right = field(rec, 5)
			if row.Right != "CE" && row.Right != "PE" {
				return nil, fmt.Errorf("line %d: option type %q", line, row.Right)
			}
		} else {
			row.Strike = 0
		}
		if isUDiFF {
			lot, err := number(field(rec, 10))
			if err != nil {
				return nil, fmt.Errorf("line %d lot: %w", line, err)
			}
			row.Lot = int64(lot)
		} else {
			if row.Volume, err = number(field(rec, 10)); err != nil {
				return nil, fmt.Errorf("line %d contracts: %w", line, err)
			}
			lakh, err := number(field(rec, 11))
			if err != nil {
				return nil, fmt.Errorf("line %d value: %w", line, err)
			}
			row.ValueRs = lakh * 1e5
		}
		out = append(out, row)
	}
	if len(out) == 0 {
		return nil, errors.New("file has no F&O rows")
	}
	return out, nil
}

func number(s string) (float64, error) {
	s = strings.ReplaceAll(s, ",", "")
	if s == "" || s == "-" {
		return 0, nil
	}
	return strconv.ParseFloat(s, 64)
}

// parseDate reads a date in any of the formats; month names match in any case.
func parseDate(s string, formats []string) (market.Date, error) {
	for _, f := range formats {
		if t, err := time.ParseInLocation(f, s, market.IST); err == nil {
			return market.DateOf(t), nil
		}
	}
	return market.Date{}, fmt.Errorf("unreadable date %q", s)
}

// MktLot is one future's lot on one day, from a market activity report.
type MktLot struct {
	Symbol string
	Index  bool
	Expiry market.Date
	Lot    int64
}

// ParseMkt reads the foDDMMYYYY.csv inside a market activity zip: futures with
// their traded quantity and contracts. Untraded futures are left out.
func ParseMkt(raw []byte, day market.Date) ([]MktLot, error) {
	z, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}
	want := fmt.Sprintf("fo%02d%02d%04d.csv", day.Day, int(day.Month), day.Year)
	var f *zip.File
	for _, zf := range z.File {
		if strings.EqualFold(zf.Name, want) {
			f = zf
		}
	}
	if f == nil {
		return nil, fmt.Errorf("%w: no %s in the zip", ErrSchema, want)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	r := csv.NewReader(rc)
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	head, err := r.Read()
	if err != nil {
		return nil, err
	}
	col := map[string]int{}
	for i, h := range head {
		col[strings.TrimSpace(h)] = i
	}
	need := []string{"INSTRUMENT", "SYMBOL", "EXP_DATE", "TRD_QTY", "NO_OF_CONT"}
	for _, n := range need {
		if _, ok := col[n]; !ok {
			return nil, fmt.Errorf("%w: no %s column in %q", ErrSchema, n, head)
		}
	}
	var out []MktLot
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		get := func(n string) string {
			if i := col[n]; i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		inst := get("INSTRUMENT")
		if inst != "FUTSTK" && inst != "FUTIDX" {
			continue // footnotes and other instruments
		}
		qty, err1 := number(get("TRD_QTY"))
		n, err2 := number(get("NO_OF_CONT"))
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("%s %s: unreadable quantity", get("SYMBOL"), get("EXP_DATE"))
		}
		if n < 1 || qty <= 0 {
			continue
		}
		lot := qty / n
		if lot != float64(int64(lot)) {
			continue // a quantity not a whole number of lots: unreliable row
		}
		e, err := parseDate(get("EXP_DATE"), []string{"02/01/2006"})
		if err != nil {
			return nil, err
		}
		out = append(out, MktLot{Symbol: get("SYMBOL"), Index: inst == "FUTIDX", Expiry: e, Lot: int64(lot)})
	}
	return out, nil
}
