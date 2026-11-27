package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"serene/internal/kite"
	"serene/internal/market"
)

// niftyIndexToken is Kite's instrument token for the NIFTY 50 index.
const niftyIndexToken = 256265

// tickIndex is an index whose options the tick recorder follows: its Kite
// index token, its options' Kite exchange and the strike step.
type tickIndex struct {
	Name     string
	Token    int64
	Exchange market.Exchange // NFO, BFO
	Step     float64
}

// tickIndices: the contracts hma_crossover watched (deleted 2026-11-27),
// still recorded at the user's request.
var tickIndices = []tickIndex{
	{Name: "NIFTY", Token: niftyIndexToken, Exchange: market.NFO, Step: 100},
	{Name: "SENSEX", Token: 265, Exchange: market.BFO, Step: 100},
}

// tickStrikeOffsets: the strikes recorded around the 09:15 open's ATM, in
// steps: ATM, −100 and +100, CE and PE.
var tickStrikeOffsets = []float64{-1, 0, 1}

// tickDayState is an index's recorded contracts for a day, chosen once from
// its 09:15 bar and kept in data/paper/day-[INDEX-]<day>.json (the format
// hma_crossover wrote, so its older files still read).
type tickDayState struct {
	Day    market.Date `json:"day"`
	Index  string      `json:"index,omitempty"` // NIFTY when empty
	Open   float64     `json:"index_open"`
	Strike float64     `json:"strike"` // ATM
	Expiry market.Date `json:"expiry"`
	Lot    int64       `json:"lot_size"`
	Legs   []tickLeg   `json:"legs"`
}

// tickLeg is one recorded contract.
type tickLeg struct {
	Right  market.Right `json:"right"`
	Token  int64        `json:"token"`
	Symbol string       `json:"symbol"`
	Strike float64      `json:"strike,omitempty"`
}

// tickDayPath is x's day file for d in dir.
func tickDayPath(dir string, x tickIndex, d market.Date) string {
	name := "day-" + d.String() + ".json"
	if x.Name != "NIFTY" {
		name = "day-" + x.Name + "-" + d.String() + ".json"
	}
	return filepath.Join(dir, name)
}

// tickDay loads or makes x's contract choice for d: the nearest expiry's
// CE and PE at the 09:15 open rounded to Step, and ±Step. It is nil, with
// no error, until Kite has the index's 09:15 bar (from about 09:16).
func tickDay(ctx context.Context, kc *kite.Client, x tickIndex, d market.Date, now time.Time, dir string) (*tickDayState, error) {
	path := tickDayPath(dir, x, d)
	var st tickDayState
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
	st = tickDayState{Day: d, Index: x.Name, Open: cs[0].Open, Strike: math.Round(cs[0].Open/x.Step) * x.Step}
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
	for _, off := range tickStrikeOffsets {
		k := st.Strike + off*x.Step
		n := len(st.Legs)
		for _, in := range ins {
			if in.Name != x.Name || in.Expiry != st.Expiry || in.Strike != k || (in.Type != "CE" && in.Type != "PE") {
				continue
			}
			st.Lot = in.LotSize
			st.Legs = append(st.Legs, tickLeg{Right: market.Right(in.Type), Token: in.Token, Symbol: in.Symbol, Strike: k})
		}
		if len(st.Legs) != n+2 || st.Lot <= 0 {
			return nil, fmt.Errorf("%s %v CE/PE for %s not in Kite's %s list", x.Name, k, st.Expiry, x.Exchange)
		}
	}
	if err := writeJSONFile(path, st); err != nil {
		return nil, err
	}
	return &st, nil
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
