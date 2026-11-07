package data

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"calm/internal/market"
)

func bar(key string, t time.Time, c market.Paise) market.Bar {
	return market.Bar{Key: key, Timeframe: market.Minute1, Time: t, End: t.Add(time.Minute),
		Open: c, High: c + 5, Low: c - 5, Close: c, Volume: 100, OI: 7}
}

func TestBarsRoundTripAndMerge(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d, _ := market.ParseDate("2026-11-06")

	first := []market.Bar{bar("B", d.At(9, 15), 100), bar("A", d.At(9, 15), 200), bar("A", d.At(9, 16), 210)}
	if err := s.PutBars(ctx, "NIFTY", first); err != nil {
		t.Fatal(err)
	}
	// Re-download: one correction, one new row.
	second := []market.Bar{bar("A", d.At(9, 16), 999), bar("A", d.At(9, 17), 220)}
	if err := s.PutBars(ctx, "NIFTY", second); err != nil {
		t.Fatal(err)
	}

	got, err := s.DayBars(ctx, "NIFTY", d)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d bars, want 4: %+v", len(got), got)
	}
	// Ordered by time then key.
	if got[0].Key != "A" || got[1].Key != "B" {
		t.Errorf("bad order: %s %s", got[0].Key, got[1].Key)
	}
	if got[2].Close != 999 {
		t.Errorf("correction lost: %d", got[2].Close)
	}
	if !got[0].Time.Equal(d.At(9, 15)) || got[0].Time.Location() != market.IST {
		t.Errorf("time not preserved: %v", got[0].Time)
	}
	if got[0].High != 205 || got[0].OI != 7 || got[0].Volume != 100 {
		t.Errorf("fields not preserved: %+v", got[0])
	}
	if m, _ := filepath.Glob(filepath.Join(s.root, "bars", "*", "*.tmp")); len(m) != 0 {
		t.Errorf("temp files left: %v", m)
	}
}

func TestBarsSplitByISTDate(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(t.TempDir())
	defer s.Close()
	d1, _ := market.ParseDate("2026-11-05")
	d2, _ := market.ParseDate("2026-11-06")
	if err := s.PutBars(ctx, "NIFTY", []market.Bar{bar("A", d1.At(9, 15), 1), bar("A", d2.At(9, 15), 2)}); err != nil {
		t.Fatal(err)
	}
	days, err := s.Days("NIFTY", d1, d2)
	if err != nil || len(days) != 2 || days[0] != d1 || days[1] != d2 {
		t.Fatalf("days %v err %v", days, err)
	}
	days, _ = s.Days("NIFTY", d2, d2)
	if len(days) != 1 {
		t.Fatalf("range filter failed: %v", days)
	}
}

func TestPutBarsValidation(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(t.TempDir())
	defer s.Close()
	d, _ := market.ParseDate("2026-11-06")
	if err := s.PutBars(ctx, "../evil", nil); err == nil {
		t.Error("path traversal accepted")
	}
	b := bar("A", d.At(9, 15), 1)
	b.Timeframe = market.Minute5
	if err := s.PutBars(ctx, "NIFTY", []market.Bar{b}); err == nil {
		t.Error("non-1m bar accepted")
	}
}

func TestContractsKeepLotSizePerContract(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(t.TempDir())
	defer s.Close()
	oldExp, _ := market.ParseDate("2025-11-25")
	newExp, _ := market.ParseDate("2026-11-09")
	// The lot size changed between these expiries.
	cs := []market.Instrument{
		market.Option(market.NFO, "NIFTY", oldExp, market.Rupees(24000), market.Call, 75),
		market.Option(market.NFO, "NIFTY", newExp, market.Rupees(24000), market.Call, 65),
	}
	if err := s.PutContracts(ctx, "NIFTY", cs); err != nil {
		t.Fatal(err)
	}
	got, err := s.Contracts(ctx, "NIFTY")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != cs[0] || got[1] != cs[1] {
		t.Fatalf("got %+v", got)
	}
	bad := market.Option(market.NFO, "NIFTY", newExp, market.Rupees(25000), market.Put, 0)
	if err := s.PutContracts(ctx, "NIFTY", []market.Instrument{bad}); err == nil {
		t.Error("contract without lot size accepted")
	}
}

func TestEquityGaps(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d := func(day int) market.Date { return market.Date{Year: 2026, Month: 11, Day: day} }
	row := func(sym, isin string, day int, open, close float64) market.EquityDay {
		return market.EquityDay{Date: d(day), Symbol: sym, Series: "EQ", ISIN: isin, Open: market.Rupees(open),
			High: market.Rupees(open), Low: market.Rupees(close), Close: market.Rupees(close), Volume: 1}
	}
	rows := []market.EquityDay{
		row("BONUS", "INE1", 2, 100, 100), row("BONUS", "INE1", 3, 50, 51), // a 1:1 bonus
		row("CRASH", "INE2", 2, 100, 100), row("CRASH", "INE2", 3, 90, 96), // fell at the open, recovered
		row("SPLIT", "INE3", 2, 100, 100), row("SPLIT", "INE4", 3, 99, 99), // new ISIN, gap hidden
		row("OLD", "INE5", 2, 100, 100), row("OLD", "INE5", 3, 50, 50), // before the scan
	}
	rows[6].Date, rows[7].Date = d(1), d(2)
	if err := s.ReplaceEquityYear(ctx, 2026, rows); err != nil {
		t.Fatal(err)
	}
	got, err := s.EquityGaps(ctx, d(3))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"BONUS", "SPLIT"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
