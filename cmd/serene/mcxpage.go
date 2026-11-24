package main

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"serene/internal/kite"
	"serene/internal/live"
	"serene/internal/market"
	"serene/internal/mcx"
)

// Hearth's Gold & Silver page (POST /api/mcx): the live MCX strategies'
// closed trades from their report, what they hold now from their state files,
// and every MCX gold or silver position Zerodha shows, with totals per metal.

type mcxPageTrade struct {
	Metal    string  `json:"metal"` // gold or silver
	Strategy string  `json:"strategy"`
	Contract string  `json:"contract"`
	Side     string  `json:"side"`
	Lots     int64   `json:"lots"`
	Entry    float64 `json:"entry"`
	EntryAt  string  `json:"entry_at"`
	Exit     float64 `json:"exit"`
	ExitAt   string  `json:"exit_at"`
	Reason   string  `json:"reason"`
	Gross    float64 `json:"gross"`
	Charges  float64 `json:"charges"`
	Net      float64 `json:"net"`
}

type mcxPagePosition struct {
	Metal    string  `json:"metal"`
	Strategy string  `json:"strategy,omitempty"` // empty: not a live strategy's (a button or manual trade)
	Contract string  `json:"contract"`
	Product  string  `json:"product"`
	Side     string  `json:"side"`
	Lots     int64   `json:"lots"`
	Entry    float64 `json:"entry"`
	EntryAt  string  `json:"entry_at,omitempty"`
	LTP      float64 `json:"ltp"`
	PnL      float64 `json:"pnl"` // the strategy's own entry for its lots; Kite's P&L otherwise
}

type mcxPageStrategy struct {
	Name    string `json:"name"`
	Metal   string `json:"metal"`
	Since   string `json:"since"`
	Status  string `json:"status"`
	Halted  string `json:"halted,omitempty"`
	Working string `json:"working,omitempty"` // an order not yet filled
}

type mcxPageMetal struct {
	Realised float64 `json:"realised"`
	Open     float64 `json:"open"`
	Net      float64 `json:"net"`
	Trades   int     `json:"trades"`
	Won      int     `json:"won"`
	Lots     int64   `json:"lots"` // open, signed
}

type mcxPage struct {
	Metals     map[string]*mcxPageMetal `json:"metals"`
	Strategies []mcxPageStrategy        `json:"strategies"`
	Positions  []mcxPagePosition        `json:"positions"`
	Trades     []mcxPageTrade           `json:"trades"`
	Errors     []string                 `json:"errors,omitempty"`
	At         string                   `json:"at"`
}

// mcxMetal is gold or silver for an MCX symbol or underlying, or "".
func mcxMetal(s string) string {
	switch {
	case strings.HasPrefix(s, "GOLD"):
		return "gold"
	case strings.HasPrefix(s, "SILVER"):
		return "silver"
	}
	return ""
}

// mcxSymbolName is the underlying of a Kite MCX future symbol: GOLDM27JANFUT → GOLDM.
func mcxSymbolName(sym string) string {
	i := strings.IndexFunc(sym, func(r rune) bool { return r >= '0' && r <= '9' })
	if i < 0 {
		return sym
	}
	return sym[:i]
}

func mcxPageData(ctx context.Context, kc func(context.Context) (*kite.Client, error), liveDir, report string) (*mcxPage, error) {
	p := &mcxPage{Metals: map[string]*mcxPageMetal{"gold": {}, "silver": {}}, At: time.Now().In(market.IST).Format("2006-01-02 15:04:05")}
	trades, err := readMCXReport(report)
	if err != nil {
		p.Errors = append(p.Errors, err.Error())
	}
	for _, t := range trades {
		m := p.Metals[t.Metal]
		if m == nil {
			continue
		}
		m.Realised += t.Net
		m.Trades++
		if t.Net > 0 {
			m.Won++
		}
	}
	sort.Slice(trades, func(i, j int) bool { return trades[i].ExitAt > trades[j].ExitAt })
	p.Trades = trades

	// What each live strategy holds, by the contract's Kite symbol.
	type held struct {
		strat string
		st    live.MCXState
	}
	bySym := map[string][]held{}
	names := make([]string, 0, len(hsStrategies))
	for n := range hsStrategies {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		var st mcxLiveState
		ok, err := readJSONFile(filepath.Join(liveDir, "mcx-"+n+".json"), &st)
		if err != nil {
			p.Errors = append(p.Errors, err.Error())
			continue
		}
		s := mcxPageStrategy{Name: n, Metal: mcxMetal(hsStrategies[n])}
		switch {
		case !ok:
			s.Status = "not started: the first live run starts it"
		case st.Qty != 0:
			s.Status = fmt.Sprintf("%s %d lot %s since %s", sideWord(sign64(st.Qty)), abs64(st.Qty), kiteSymbol(st.Key),
				st.EntryTime.In(market.IST).Format("02 Jan 15:04"))
			bySym[kiteSymbol(st.Key)] = append(bySym[kiteSymbol(st.Key)], held{n, st.MCXState})
		default:
			s.Status = "flat: waiting for the next cross"
		}
		if ok {
			s.Since = st.Since.In(market.IST).Format("02 Jan 2006 15:04")
			s.Halted = st.Halted
			if o := st.Order; o != nil {
				s.Working = fmt.Sprintf("%s %d %s @ ₹%s (%s)", o.Side, o.Qty, kiteSymbol(o.Key), o.Price, o.Why)
			}
		}
		p.Strategies = append(p.Strategies, s)
	}

	c, err := kc(ctx)
	if err == nil {
		var pos []kite.Position
		if pos, err = c.Positions(ctx); err == nil {
			for _, k := range pos {
				metal := mcxMetal(k.Symbol)
				if k.Exchange != "MCX" || k.Qty == 0 || metal == "" {
					continue
				}
				left := k.Qty
				for _, h := range bySym[k.Symbol] {
					if k.Product != "NRML" || sign64(left) != sign64(h.st.Qty) || abs64(left) < abs64(h.st.Qty) {
						continue
					}
					mult := float64(mcx.Specs[mcxSymbolName(k.Symbol)].Multiplier)
					pnl := (k.Last - h.st.Entry.Rupees()) * float64(h.st.Qty) * mult
					p.Positions = append(p.Positions, mcxPagePosition{Metal: metal, Strategy: h.strat, Contract: k.Symbol, Product: k.Product,
						Side: sideWord(sign64(h.st.Qty)), Lots: abs64(h.st.Qty), Entry: h.st.Entry.Rupees(),
						EntryAt: h.st.EntryTime.In(market.IST).Format("02 Jan 15:04"), LTP: k.Last, PnL: pnl})
					left -= h.st.Qty
				}
				if left != 0 {
					// Kite's P&L is for the whole position; scale it to what's left.
					pnl := k.PnL * float64(left) / float64(k.Qty)
					p.Positions = append(p.Positions, mcxPagePosition{Metal: metal, Contract: k.Symbol, Product: k.Product,
						Side: sideWord(sign64(left)), Lots: abs64(left), Entry: k.Average, LTP: k.Last, PnL: pnl})
				}
			}
		}
	}
	if err != nil {
		p.Errors = append(p.Errors, "Zerodha positions: "+err.Error())
	}
	for _, x := range p.Positions {
		m := p.Metals[x.Metal]
		m.Open += x.PnL
		m.Lots += int64(map[string]int{"long": 1, "short": -1}[x.Side]) * x.Lots
	}
	for _, m := range p.Metals {
		m.Net = m.Realised + m.Open
	}
	return p, nil
}

// readMCXReport reads the live MCX closed trades (mcxLiveHeader); a missing file is none.
func readMCXReport(path string) ([]mcxPageTrade, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	for _, h := range mcxLiveHeader {
		if _, ok := col[h]; !ok {
			return nil, fmt.Errorf("%s: no %s column", path, h)
		}
	}
	var out []mcxPageTrade
	for _, r := range rows[1:] {
		get := func(k string) string { return r[col[k]] }
		num := func(k string) float64 { v, _ := strconv.ParseFloat(get(k), 64); return v }
		lots, _ := strconv.ParseInt(get("lots"), 10, 64)
		out = append(out, mcxPageTrade{Metal: mcxMetal(get("contract")), Strategy: get("strategy"), Contract: get("contract"),
			Side: get("side"), Lots: lots, Entry: num("entry"), EntryAt: get("entry_time"), Exit: num("exit"), ExitAt: get("exit_time"),
			Reason: get("reason"), Gross: num("gross"), Charges: num("charges"), Net: num("net")})
	}
	return out, nil
}

// kiteSymbol is a serene MCX future key's Kite symbol: MCX:GOLDM:2027-01-05:FUT → GOLDM27JANFUT.
func kiteSymbol(key string) string {
	in, err := market.ParseKey(key)
	if err != nil {
		return key
	}
	t := time.Date(in.Expiry.Year, in.Expiry.Month, in.Expiry.Day, 0, 0, 0, 0, time.UTC)
	return in.Underlying + strings.ToUpper(t.Format("06Jan")) + "FUT"
}

func sign64(q int64) market.Side {
	switch {
	case q > 0:
		return market.Buy
	case q < 0:
		return market.Sell
	}
	return 0
}

func abs64(q int64) int64 {
	if q < 0 {
		return -q
	}
	return q
}
