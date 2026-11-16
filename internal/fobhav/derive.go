package fobhav

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"serene/internal/market"
)

// Lot sources, most exact first.
const (
	SourceNSE     = "nse_udiff"   // NewBrdLotQty in NSE's UDiFF file
	SourceBSE     = "bse_udiff"   // NewBrdLotQty in BSE's file
	SourceMkt     = "nse_mkt"     // traded quantity / contracts in NSE's market activity report
	SourceDerived = "nse_derived" // estimated from legacy futures turnover: approximate
	SourceManual  = "manual"      // a published size for a period no file covers
)

// Expiry is one option expiry of an underlying as the bhavcopies show it.
type Expiry struct {
	Expiry    string    `json:"expiry"`
	Lot       int64     `json:"lot,omitempty"`
	LotSource string    `json:"lot_source,omitempty"`
	Calls     []float64 `json:"calls,omitempty"`
	Puts      []float64 `json:"puts,omitempty"`
	Future    bool      `json:"future"` // a future expired the same day
}

// Symbol is everything the bhavcopies say about one underlying.
type Symbol struct {
	Symbol   string `json:"symbol"` // the exchange's symbol, e.g. M&M
	Index    bool   `json:"index"`
	Exchange string `json:"exchange"` // NSE or BSE
	First    string `json:"first"`    // first and last days with options
	Last     string `json:"last"`
	// Closes is each day's near-month future close: an unadjusted price, so
	// strikes before a split are chosen around the price they traded at.
	Closes   map[string]float64 `json:"closes"`
	Expiries []Expiry           `json:"expiries"` // oldest first
}

// Summary reports a Build.
type Summary struct {
	Files, Symbols, Stocks int
	// Checked and Mismatched compare the market activity lots with UDiFF lots
	// on expiries both saw, around July 2024.
	Checked, Mismatched int
	Mismatches          []string
	// Estimated compares the turnover estimate with the market activity lot,
	// to say how far a fallback estimate can be trusted.
	Estimated, EstimateOff int
	BySource               map[string]int
	NoLot                  int // option expiries left without a lot
}

// manualLots are BSE lot sizes before BSE's archive starts (2024-01): the
// sizes SENSEX and BANKEX options relaunched with in 2023.
var manualLots = map[string]int64{"SENSEX": 10, "BANKEX": 15}

type acc struct {
	sym      Symbol
	first    market.Date
	last     market.Date
	strikes  map[market.Date]map[float64]uint8 // 1 call, 2 put
	futures  map[market.Date]bool
	near     map[market.Date][2]float64 // day -> (expiry as days, close) of the nearest future
	udiff    map[market.Date]map[int64]int
	mkt      map[market.Date]map[int64]int
	interval map[market.Date]*lotInterval
}

func count(m map[market.Date]map[int64]int, e market.Date, lot int64) {
	c := m[e]
	if c == nil {
		c = map[int64]int{}
		m[e] = c
	}
	c[lot]++
}

// lotInterval narrows a legacy lot: turnover = lot x contracts x VWAP, and
// VWAP lies within the day's low and high, so each day bounds the lot.
type lotInterval struct {
	lo, hi float64
	n      int // days bounding the interval
	empty  bool
	points []float64
}

func (l *lotInterval) add(r Row) {
	if r.Volume < 1 || r.ValueRs <= 0 || r.Low <= 0 || r.High < r.Low {
		return
	}
	lo, hi := r.ValueRs/(r.Volume*r.High), r.ValueRs/(r.Volume*r.Low)
	px := r.Settle
	if px <= 0 {
		px = r.Close
	}
	if px > 0 {
		l.points = append(l.points, r.ValueRs/(r.Volume*px))
	}
	// Allow for turnover being rounded to the paisa-lakh.
	lo, hi = lo*0.9995, hi*1.0005
	l.n++
	if l.n == 1 {
		l.lo, l.hi = lo, hi
		return
	}
	l.lo, l.hi = max(l.lo, lo), min(l.hi, hi)
	if l.lo > l.hi {
		l.empty = true
	}
}

// size estimates the lot: the median day's estimate, kept within the
// interval every day allows. It is approximate, so it is a last resort.
func (l *lotInterval) size() int64 {
	if len(l.points) == 0 {
		return 0
	}
	p := append([]float64(nil), l.points...)
	sort.Float64s(p)
	est := p[len(p)/2]
	if l.n > 0 && !l.empty {
		est = min(max(est, l.lo), l.hi)
	}
	return int64(math.Round(est))
}

func newAcc(r Row, exch string) *acc {
	return &acc{sym: Symbol{Symbol: r.Symbol, Index: r.Index, Exchange: exch, Closes: map[string]float64{}},
		strikes: map[market.Date]map[float64]uint8{}, futures: map[market.Date]bool{},
		near: map[market.Date][2]float64{}, udiff: map[market.Date]map[int64]int{},
		mkt: map[market.Date]map[int64]int{}, interval: map[market.Date]*lotInterval{}}
}

func days(d market.Date) float64 { return float64(d.At(0, 0).Unix() / 86400) }

func (a *acc) add(r Row) {
	if r.Lot > 0 {
		count(a.udiff, r.Expiry, r.Lot)
	}
	if r.Future {
		a.futures[r.Expiry] = true
		if px := r.Close; px > 0 && !r.Expiry.Before(r.Date) {
			if n, ok := a.near[r.Date]; !ok || days(r.Expiry) < n[0] {
				a.near[r.Date] = [2]float64{days(r.Expiry), px}
			}
		}
		if r.Lot == 0 {
			l := a.interval[r.Expiry]
			if l == nil {
				l = &lotInterval{}
				a.interval[r.Expiry] = l
			}
			l.add(r)
		}
		return
	}
	if a.first.IsZero() || r.Date.Before(a.first) {
		a.first = r.Date
	}
	if a.last.Before(r.Date) {
		a.last = r.Date
	}
	m := a.strikes[r.Expiry]
	if m == nil {
		m = map[float64]uint8{}
		a.strikes[r.Expiry] = m
	}
	if r.Right == "CE" {
		m[r.Strike] |= 1
	} else {
		m[r.Strike] |= 2
	}
}

func mode(m map[int64]int) int64 {
	var best int64
	n := -1
	for k, c := range m {
		if c > n || c == n && k > best {
			best, n = k, c
		}
	}
	return best
}

// finish turns the accumulated days into a Symbol, filling each option
// expiry's lot from the most exact source that covers it.
func (a *acc) finish(sum *Summary) Symbol {
	s := a.sym
	s.First, s.Last = a.first.String(), a.last.String()
	for d, n := range a.near {
		s.Closes[d.String()] = n[1]
	}
	var futs []market.Date
	for e := range a.futures {
		futs = append(futs, e)
	}
	sort.Slice(futs, func(i, j int) bool { return futs[i].Before(futs[j]) })
	// A future expiry's lot: the market activity report's, else the estimate.
	futLot := map[market.Date]int64{}
	futSrc := map[market.Date]string{}
	for _, e := range futs {
		est := int64(0)
		if l := a.interval[e]; l != nil {
			est = l.size()
		}
		if m := a.mkt[e]; len(m) > 0 {
			futLot[e], futSrc[e] = mode(m), SourceMkt
			if est > 0 {
				sum.Estimated++
				if est != futLot[e] {
					sum.EstimateOff++
				}
			}
			if u := a.udiff[e]; len(u) > 0 {
				sum.Checked++
				if mode(u) != futLot[e] {
					sum.Mismatched++
					sum.Mismatches = append(sum.Mismatches,
						fmt.Sprintf("%s %s: market activity %d, udiff %d", s.Symbol, e, futLot[e], mode(u)))
				}
			}
		} else if est > 0 {
			futLot[e], futSrc[e] = est, SourceDerived
		}
	}

	var exps []market.Date
	for e := range a.strikes {
		exps = append(exps, e)
	}
	sort.Slice(exps, func(i, j int) bool { return exps[i].Before(exps[j]) })
	for _, e := range exps {
		x := Expiry{Expiry: e.String(), Future: a.futures[e]}
		for k, bits := range a.strikes[e] {
			if bits&1 != 0 {
				x.Calls = append(x.Calls, k)
			}
			if bits&2 != 0 {
				x.Puts = append(x.Puts, k)
			}
		}
		sort.Float64s(x.Calls)
		sort.Float64s(x.Puts)
		switch {
		case len(a.udiff[e]) > 0:
			x.Lot, x.LotSource = mode(a.udiff[e]), SourceNSE
			if s.Exchange == "BSE" {
				x.LotSource = SourceBSE
			}
		default:
			// A weekly expiry takes the lot of the monthly future expiring on or
			// after it: NSE revises lots from a monthly expiry onward.
			i := sort.Search(len(futs), func(i int) bool { return !futs[i].Before(e) })
			if i < len(futs) && futLot[futs[i]] > 0 {
				x.Lot, x.LotSource = futLot[futs[i]], futSrc[futs[i]]
			}
		}
		if x.Lot == 0 {
			sum.NoLot++
		} else {
			sum.BySource[x.LotSource]++
		}
		s.Expiries = append(s.Expiries, x)
	}
	return s
}

// Build reads every stored bhavcopy and writes one JSON file per underlying
// into <root>/bhav_fo/derived, plus <root>/universe.csv: every stock that had
// options on any day from `from`, with its first and last such day.
func Build(root string, from market.Date, log func(string, ...any)) (Summary, error) {
	sum := Summary{BySource: map[string]int{}}
	nse, err := listRaw(nseDir(root), ".csv.zip", true)
	if err != nil {
		return sum, err
	}
	bse, err := listRaw(bseDir(root), ".csv", false)
	if err != nil {
		return sum, err
	}
	accs := map[string]*acc{}
	read := func(files []rawFile, exch string) error {
		for i, f := range files {
			if f.Date.Before(from) {
				continue
			}
			rows, err := f.rows()
			if err != nil {
				return err
			}
			for _, r := range rows {
				if exch == "BSE" && manualLots[r.Symbol] == 0 {
					continue // only SENSEX and BANKEX are wanted from BSE
				}
				a := accs[r.Symbol]
				if a == nil {
					a = newAcc(r, exch)
					accs[r.Symbol] = a
				}
				a.add(r)
			}
			sum.Files++
			if (i+1)%250 == 0 {
				log("%s: read %d of %d files, up to %s", exch, i+1, len(files), f.Date)
			}
		}
		return nil
	}
	if err := read(nse, "NSE"); err != nil {
		return sum, err
	}
	if err := read(bse, "BSE"); err != nil {
		return sum, err
	}
	mkt, err := listRaw(mktDir(root), ".zip", false)
	if err != nil {
		return sum, err
	}
	for _, f := range mkt {
		if f.Date.Before(from) {
			continue
		}
		b, err := os.ReadFile(f.Path)
		if err != nil {
			return sum, err
		}
		lots, err := ParseMkt(b, f.Date)
		if err != nil {
			return sum, fmt.Errorf("%s: %w", f.Path, err)
		}
		for _, l := range lots {
			if a := accs[l.Symbol]; a != nil {
				count(a.mkt, l.Expiry, l.Lot)
			}
		}
	}
	log("market activity: read %d files", len(mkt))

	dir := filepath.Join(root, "bhav_fo", "derived")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return sum, err
	}
	var latest market.Date
	if len(nse) > 0 {
		latest = nse[len(nse)-1].Date
	}
	var uni [][]string
	names := make([]string, 0, len(accs))
	for n := range accs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		a := accs[n]
		if a.first.IsZero() {
			continue // futures only
		}
		s := a.finish(&sum)
		if err := writeJSON(filepath.Join(dir, n+".json"), s); err != nil {
			return sum, err
		}
		sum.Symbols++
		if !s.Index {
			sum.Stocks++
			live := !a.last.Before(latest.AddDays(-7))
			uni = append(uni, []string{n, s.First, s.Last, strconv.FormatBool(live)})
		}
	}
	if err := writeUniverse(filepath.Join(root, "universe.csv"), uni); err != nil {
		return sum, err
	}
	return sum, nil
}

func writeUniverse(path string, rows [][]string) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	w.Write([]string{"symbol", "first", "last", "live"})
	w.WriteAll(rows)
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads one underlying's derived file. ok is false when there is none.
func Load(root, symbol string) (*Symbol, bool, error) {
	b, err := os.ReadFile(filepath.Join(root, "bhav_fo", "derived", symbol+".json"))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var s Symbol
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, false, fmt.Errorf("%s: %w", symbol, err)
	}
	return &s, true, nil
}

// ManualLot is the size to assume for an underlying's expiries before any
// file covers it; ok is false when there is none.
func ManualLot(symbol string) (size int64, ok bool) {
	n, ok := manualLots[symbol]
	return n, ok
}

// ReadUniverse reads the symbols of a universe.csv that Build wrote.
func ReadUniverse(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, err
	}
	var out []string
	for i, r := range recs {
		if i == 0 || len(r) == 0 {
			continue
		}
		out = append(out, r[0])
	}
	return out, nil
}

// ExpiryDate parses an Expiry's date.
func (x Expiry) Date() (market.Date, error) { return market.ParseDate(x.Expiry) }

// CloseRange returns, for strike selection, a closes map in the shape the
// ingest band expects: day -> (low, high), both the near future's close.
func (s *Symbol) CloseRange() map[string][2]float64 {
	out := make(map[string][2]float64, len(s.Closes))
	for d, px := range s.Closes {
		out[d] = [2]float64{px, px}
	}
	return out
}
