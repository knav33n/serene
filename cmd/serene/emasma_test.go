package main

import (
	"context"
	"os"
	"testing"
	"time"

	"serene/internal/cost"
	"serene/internal/market"
)

// The replay on local MCX bars agrees with itself at 1 tick vs 3 ticks and
// produces a verdict; skipped without data/mcx.
func TestESReplay(t *testing.T) {
	if _, err := os.Stat("../../data/mcx/bars/underlying=GOLDM"); err != nil {
		t.Skip("no local MCX bars")
	}
	costs, err := cost.Load("../../configs/costs_mcx.yaml")
	if err != nil {
		t.Fatal(err)
	}
	saved := esForward
	defer func() { esForward = saved }()
	esForward = market.Date{Year: 2026, Month: time.October, Day: 1}
	var rs []esResult
	for _, s := range esStrategies {
		r, err := esReplay(context.Background(), "../../data/mcx", costs, s.name, s.metal)
		if err != nil {
			t.Fatal(err)
		}
		r.maxDD = s.maxDD
		if r.trades == 0 || r.net3 >= r.net1 || r.mtm.depth <= 0 {
			t.Fatalf("%s: %+v", s.name, r)
		}
		t.Logf("%s: %d trades net1 %.0f net3 %.0f dd %.0f %s", r.name, r.trades, r.net1, r.net3, r.mtm.depth, r.open)
		rs = append(rs, r)
	}
	t.Log(esVerdict(rs))
}
