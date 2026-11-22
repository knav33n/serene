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

// paperDayState is the day's watched contracts, chosen once at the open and
// kept so every later run watches the same two.
type paperDayState struct {
	Day    market.Date `json:"day"`
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
func paperDay(ctx context.Context, kc *kite.Client, d market.Date, now time.Time, dir string, step float64) (*paperDayState, error) {
	path := filepath.Join(dir, "day-"+d.String()+".json")
	var st paperDayState
	if ok, err := readJSONFile(path, &st); err != nil || ok {
		return &st, err
	}
	cs, err := kc.Candles(ctx, niftyIndexToken, "minute", d.At(9, 15), d.At(9, 16), false)
	if err != nil {
		return nil, fmt.Errorf("NIFTY 50 candles: %w", err)
	}
	if len(cs) == 0 || !cs[0].Time.Equal(d.At(9, 15)) {
		if now.After(d.At(9, 25)) {
			return nil, fmt.Errorf("Kite has no NIFTY 50 bar at 09:15 on %s", d)
		}
		return nil, nil
	}
	st = paperDayState{Day: d, Open: cs[0].Open, Strike: math.Round(cs[0].Open/step) * step}
	ins, err := kc.Instruments(ctx, "NFO")
	if err != nil {
		return nil, fmt.Errorf("NFO instruments: %w", err)
	}
	for _, in := range ins {
		if in.Name == "NIFTY" && (in.Type == "CE" || in.Type == "PE") && !in.Expiry.Before(d) &&
			(st.Expiry.IsZero() || in.Expiry.Before(st.Expiry)) {
			st.Expiry = in.Expiry
		}
	}
	for _, in := range ins {
		if in.Name != "NIFTY" || in.Expiry != st.Expiry || in.Strike != st.Strike || (in.Type != "CE" && in.Type != "PE") {
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
		return nil, fmt.Errorf("NIFTY %v CE/PE for %s not in Kite's NFO list", st.Strike, st.Expiry)
	}
	if err := writeJSONFile(path, st); err != nil {
		return nil, err
	}
	return &st, nil
}

func paperName(in market.Instrument) string {
	return fmt.Sprintf("NIFTY %s %s (exp %s)", in.Strike, in.Right, in.Expiry)
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
