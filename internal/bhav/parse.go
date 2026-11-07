package bhav

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"calm/internal/market"
)

// ErrSchema means the file's columns changed, so nothing in it can be trusted.
var ErrSchema = errors.New("unexpected columns in bhavcopy")

// layout names each format's columns. Empty means the format lacks the field.
type layout struct {
	date, symbol, series, isin, open, high, low, close, last, prev, volume, turnover, trades string
	dateFormats                                                                              []string
}

var (
	legacy = layout{date: "TIMESTAMP", symbol: "SYMBOL", series: "SERIES", isin: "ISIN", open: "OPEN", high: "HIGH",
		low: "LOW", close: "CLOSE", last: "LAST", prev: "PREVCLOSE", volume: "TOTTRDQTY", turnover: "TOTTRDVAL",
		trades: "TOTALTRADES", dateFormats: []string{"2-Jan-2006", "2-Jan-06"}}
	udiff = layout{date: "TradDt", symbol: "TckrSymb", series: "SctySrs", isin: "ISIN", open: "OpnPric", high: "HghPric",
		low: "LwPric", close: "ClsPric", last: "LastPric", prev: "PrvsClsgPric", volume: "TtlTradgVol",
		turnover: "TtlTrfVal", trades: "TtlNbOfTxsExctd", dateFormats: []string{"2006-01-02"}}
)

// optional fields may be missing from a format's older files.
var optional = map[string]bool{"ISIN": true, "TOTALTRADES": true}

// Unzip returns the one CSV inside a downloaded file.
func Unzip(raw []byte) ([]byte, error) {
	z, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}
	if len(z.File) != 1 {
		return nil, fmt.Errorf("zip holds %d files, want 1", len(z.File))
	}
	f, err := z.File[0].Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func number(s string) (float64, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	if s == "" || s == "-" {
		return 0, nil
	}
	return strconv.ParseFloat(s, 64)
}

// Parse reads a day's CSV in either format. It rejects a file dated other than
// the day requested, and one listing a security twice.
func Parse(csvData []byte, want market.Date) ([]market.EquityDay, error) {
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(csvData, []byte{0xEF, 0xBB, 0xBF})))
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
	lay := legacy
	if _, ok := col[udiff.symbol]; ok {
		lay = udiff
	}
	need := []string{lay.date, lay.symbol, lay.series, lay.isin, lay.open, lay.high, lay.low, lay.close, lay.last,
		lay.prev, lay.volume, lay.turnover, lay.trades}
	idx := make([]int, len(need))
	for i, name := range need {
		j, ok := col[name]
		switch {
		case ok:
			idx[i] = j
		case optional[name]:
			idx[i] = -1
		default:
			return nil, fmt.Errorf("%w: no %s column in %q", ErrSchema, name, head)
		}
	}
	field := func(rec []string, i int) string {
		if idx[i] < 0 || idx[i] >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[idx[i]])
	}

	var out []market.EquityDay
	seen := map[[2]string]bool{}
	for line := 2; ; line++ {
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
		if len(rec) < len(col) {
			return nil, fmt.Errorf("%w: line %d has %d fields: %q", ErrSchema, line, len(rec), rec)
		}
		d, err := parseDate(field(rec, 0), lay.dateFormats)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if d != want {
			return nil, fmt.Errorf("file for %s has a row dated %s", want, d)
		}
		v := make([]float64, 9)
		for k := range v {
			if v[k], err = number(field(rec, 4+k)); err != nil {
				return nil, fmt.Errorf("line %d: %s: %w", line, need[4+k], err)
			}
		}
		row := market.EquityDay{Date: d, Symbol: field(rec, 1), Series: field(rec, 2), ISIN: field(rec, 3),
			Open: market.Rupees(v[0]), High: market.Rupees(v[1]), Low: market.Rupees(v[2]), Close: market.Rupees(v[3]),
			Last: market.Rupees(v[4]), PrevClose: market.Rupees(v[5]), Volume: int64(math.Round(v[6])), Turnover: v[7],
			Trades: int64(math.Round(v[8]))}
		if row.Symbol == "" {
			return nil, fmt.Errorf("line %d has no symbol", line)
		}
		key := [2]string{row.Symbol, row.Series}
		if seen[key] {
			return nil, fmt.Errorf("%s %s is listed twice", row.Symbol, row.Series)
		}
		seen[key] = true
		out = append(out, row)
	}
	if len(out) == 0 {
		return nil, errors.New("file has no rows")
	}
	return out, nil
}

func parseDate(s string, formats []string) (market.Date, error) {
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return market.Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}, nil
		}
	}
	return market.Date{}, fmt.Errorf("unreadable date %q", s)
}
