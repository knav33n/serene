package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKiteSymbol(t *testing.T) {
	if got := kiteSymbol("MCX:GOLDM:2027-01-05:FUT"); got != "GOLDM27JANFUT" {
		t.Fatal(got)
	}
	if got := mcxSymbolName("SILVERM27FEBFUT"); got != "SILVERM" {
		t.Fatal(got)
	}
	if mcxMetal("GOLDM27JANFUT") != "gold" || mcxMetal("SILVERM") != "silver" || mcxMetal("CRUDEOIL") != "" {
		t.Fatal("metal")
	}
}

func TestReadMCXReport(t *testing.T) {
	p := filepath.Join(t.TempDir(), "live_mcx.csv")
	if ts, err := readMCXReport(p); err != nil || ts != nil {
		t.Fatal(ts, err)
	}
	rows := strings.Join(mcxLiveHeader, ",") + "\n" +
		"goldm_hmasma_10m,GOLDM27JANFUT,short,2026-11-24 10:00:00,123000,2026-11-24 12:00:00,122500,1,cross,5000,62.5,4937.5\n"
	if err := os.WriteFile(p, []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, err := readMCXReport(p)
	if err != nil || len(ts) != 1 {
		t.Fatal(ts, err)
	}
	if x := ts[0]; x.Metal != "gold" || x.Side != "short" || x.Net != 4937.5 || x.Lots != 1 || x.Exit != 122500 {
		t.Fatalf("%+v", x)
	}
}
