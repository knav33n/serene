package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"serene/internal/kite"
	"serene/internal/market"
)

func TestTickWindow(t *testing.T) {
	d := market.Date{Year: 2026, Month: time.November, Day: 27}
	dir := t.TempDir()
	path := tickPath(dir, d, "NFO:NIFTY25DEC25800CE")
	if filepath.Base(path) != "NFO_NIFTY25DEC25800CE.csv" {
		t.Errorf("path %s", path)
	}
	write := func(rows map[time.Time]float64, order []time.Time) {
		f, err := openTickFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, at := range order {
			if err := f.c.Write(tickRow(at, kite.Tick{LTP: rows[at], Exchange: at})); err != nil {
				t.Fatal(err)
			}
		}
		f.c.Flush()
		f.w.Flush()
		f.f.Close()
	}
	at := func(h, m, s int) time.Time { return d.At(h, m).Add(time.Duration(s) * time.Second) }
	px := map[time.Time]float64{at(10, 2, 0): 90, at(10, 3, 0): 100, at(10, 4, 0): 101, at(10, 4, 30): 96.5, at(10, 6, 0): 102.1, at(10, 9, 0): 80}
	write(px, []time.Time{at(10, 2, 0), at(10, 3, 0), at(10, 4, 0)})
	// A restart appends without a second header.
	write(px, []time.Time{at(10, 4, 30), at(10, 6, 0), at(10, 9, 0)})
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(b)), "\n"); len(lines) != 7 || lines[0] != strings.Join(tickHeader, ",") {
		t.Fatalf("file %q", lines)
	}
}

func TestTickFixed(t *testing.T) {
	d := market.Date{Year: 2026, Month: time.November, Day: 27}
	ins := []kite.Instrument{
		{Token: 1, Exchange: "NFO", Type: "FUT", Name: "NIFTY", Symbol: "NIFTY26DECFUT", Expiry: market.Date{Year: 2026, Month: time.December, Day: 29}},
		{Token: 2, Exchange: "NFO", Type: "FUT", Name: "NIFTY", Symbol: "NIFTY26NOVFUT", Expiry: d},
		{Token: 3, Exchange: "NFO", Type: "FUT", Name: "NIFTY", Symbol: "NIFTY26OCTFUT", Expiry: market.Date{Year: 2026, Month: time.October, Day: 27}},
		{Token: 4, Exchange: "NFO", Type: "FUT", Name: "BANKNIFTY", Symbol: "BANKNIFTY26NOVFUT", Expiry: d},
		{Token: 5, Exchange: "NFO", Type: "CE", Name: "NIFTY", Symbol: "NIFTY26NOV26000CE", Expiry: d},
	}
	got := tickFixed(ins, d)
	if len(got) != 2 || got[niftyIndexToken].quote != "NSE:NIFTY 50" || got[2].quote != "NFO:NIFTY26NOVFUT" {
		t.Errorf("fixed %+v", got)
	}
	if p := tickPath("x", d, niftyIndexQuote); filepath.Base(p) != "NSE_NIFTY_50.csv" {
		t.Errorf("index path %s", p)
	}
}

// TestTickDayPath: the recorder keeps hma_crossover's day-file names, so a
// day it already chose (or one hma_crossover wrote) is read, not rechosen.
func TestTickDayPath(t *testing.T) {
	d := market.Date{Year: 2026, Month: time.November, Day: 27}
	if p := tickDayPath("data/paper", tickIndices[0], d); p != "data/paper/day-2026-11-27.json" {
		t.Errorf("NIFTY %s", p)
	}
	if p := tickDayPath("data/paper", tickIndices[1], d); p != "data/paper/day-SENSEX-2026-11-27.json" {
		t.Errorf("SENSEX %s", p)
	}
	dir := t.TempDir()
	want := tickDayState{Day: d, Index: "SENSEX", Strike: 85000, Legs: []tickLeg{{Right: market.Call, Token: 7, Symbol: "SENSEX25DEC85000CE", Strike: 85000}}}
	if err := writeJSONFile(tickDayPath(dir, tickIndices[1], d), want); err != nil {
		t.Fatal(err)
	}
	got, err := tickContracts(d, dir)
	if err != nil || len(got) != 1 || got[7].quote != "BFO:SENSEX25DEC85000CE" {
		t.Errorf("contracts %+v %v", got, err)
	}
}
