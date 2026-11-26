package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"serene/internal/kite"
	"serene/internal/market"
)

// niftyIndexToken is Kite's instrument token for the NIFTY 50 index.
const niftyIndexToken = 256265

// hmaIndex is an index hma_crossover watches: its Kite index token, the
// exchange of its options on Kite and Fyers, the strike rounding, and the
// largest order quantity the exchange accepts.
type hmaIndex struct {
	Name     string
	Token    int64
	Exchange market.Exchange // Kite's: NFO, BFO
	FyersSeg string          // Fyers' symbol master: NSE_FO, BSE_FO
	Step     float64
	Freeze   int64
}

// hmaIndices: SENSEX added 2026-11-26, same HMA lengths as NIFTY.
var hmaIndices = []hmaIndex{
	{Name: "NIFTY", Token: niftyIndexToken, Exchange: market.NFO, FyersSeg: "NSE_FO", Step: 100, Freeze: 1755},
	{Name: "SENSEX", Token: 265, Exchange: market.BFO, FyersSeg: "BSE_FO", Step: 100, Freeze: 1000},
}

// hmaIndexOf is the index an option symbol (Kite's) is on.
func hmaIndexOf(symbol string) hmaIndex {
	for _, x := range hmaIndices[1:] {
		if strings.HasPrefix(symbol, x.Name) {
			return x
		}
	}
	return hmaIndices[0]
}

// paperDayState is the day's watched contracts, chosen once at the open and
// kept so every later run watches the same two.
type paperDayState struct {
	Day    market.Date `json:"day"`
	Index  string      `json:"index,omitempty"` // NIFTY when empty (files before 2026-11-26)
	Open   float64     `json:"index_open"`
	Strike float64     `json:"strike"`
	Expiry market.Date `json:"expiry"`
	Lot    int64       `json:"lot_size"`
	Legs   []struct {
		Right  market.Right `json:"right"`
		Token  int64        `json:"token"`
		Symbol string       `json:"symbol"`
	} `json:"legs"`
}

// paperDay loads or makes the day's contract choice. It is nil, with no
// error, until the index's 09:15 bar exists.
func paperDay(ctx context.Context, kc *kite.Client, x hmaIndex, d market.Date, now time.Time, dir string) (*paperDayState, error) {
	name := "day-" + d.String() + ".json"
	if x.Name != "NIFTY" {
		name = "day-" + x.Name + "-" + d.String() + ".json"
	}
	path := filepath.Join(dir, name)
	var st paperDayState
	if ok, err := readJSONFile(path, &st); err != nil || ok {
		st.Index = x.Name
		return &st, err
	}
	cs, err := kc.Candles(ctx, x.Token, "minute", d.At(9, 15), d.At(9, 16), false)
	if err != nil {
		return nil, fmt.Errorf("%s index candles: %w", x.Name, err)
	}
	if len(cs) == 0 || !cs[0].Time.Equal(d.At(9, 15)) {
		if now.After(d.At(9, 25)) {
			return nil, fmt.Errorf("Kite has no %s index bar at 09:15 on %s", x.Name, d)
		}
		return nil, nil
	}
	st = paperDayState{Day: d, Index: x.Name, Open: cs[0].Open, Strike: math.Round(cs[0].Open/x.Step) * x.Step}
	ins, err := kc.Instruments(ctx, string(x.Exchange))
	if err != nil {
		return nil, fmt.Errorf("%s instruments: %w", x.Exchange, err)
	}
	for _, in := range ins {
		if in.Name == x.Name && (in.Type == "CE" || in.Type == "PE") && !in.Expiry.Before(d) &&
			(st.Expiry.IsZero() || in.Expiry.Before(st.Expiry)) {
			st.Expiry = in.Expiry
		}
	}
	for _, in := range ins {
		if in.Name != x.Name || in.Expiry != st.Expiry || in.Strike != st.Strike || (in.Type != "CE" && in.Type != "PE") {
			continue
		}
		st.Lot = in.LotSize
		st.Legs = append(st.Legs, struct {
			Right  market.Right `json:"right"`
			Token  int64        `json:"token"`
			Symbol string       `json:"symbol"`
		}{market.Right(in.Type), in.Token, in.Symbol})
	}
	if len(st.Legs) != 2 || st.Lot <= 0 {
		return nil, fmt.Errorf("%s %v CE/PE for %s not in Kite's %s list", x.Name, st.Strike, st.Expiry, x.Exchange)
	}
	if err := writeJSONFile(path, st); err != nil {
		return nil, err
	}
	return &st, nil
}

func paperName(in market.Instrument) string {
	return fmt.Sprintf("%s %s %s (exp %s)", in.Underlying, in.Strike, in.Right, in.Expiry)
}

func readJSONFile(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
