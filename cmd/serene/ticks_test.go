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
