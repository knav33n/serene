package analytics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"calm/internal/engine"
	"calm/internal/market"
	"calm/internal/strategy"
)

func date(t *testing.T, s string) market.Date {
	t.Helper()
	d, err := market.ParseDate(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestHoldoutRefusesUnlessUnsealed(t *testing.T) {
	dir := t.TempDir()
	sealedDir := filepath.Join(dir, "real")
	file := filepath.Join(dir, "holdout.yaml")
	if err := os.WriteFile(file, []byte(sealedDir+`: "2026-05-01"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := LoadHoldouts(file)
	if err != nil {
		t.Fatal(err)
	}
	if sealed, err := h.Check(sealedDir, date(t, "2026-04-30"), false); sealed || err != nil {
		t.Fatalf("before the holdout: sealed=%v err=%v", sealed, err)
	}
	if _, err := h.Check(sealedDir, date(t, "2026-05-01"), false); err == nil || !strings.Contains(err.Error(), "--unseal") {
		t.Fatalf("reaching the holdout was allowed: %v", err)
	}
	// The same directory spelt differently is still sealed.
	if _, err := h.Check(sealedDir+"/./", date(t, "2026-06-01"), false); err == nil {
		t.Fatal("an unclean path bypassed the holdout")
	}
	if sealed, err := h.Check(sealedDir, date(t, "2026-06-01"), true); !sealed || err != nil {
		t.Fatalf("unseal: sealed=%v err=%v", sealed, err)
	}
	if sealed, err := h.Check(filepath.Join(dir, "synthetic"), date(t, "2026-06-01"), false); sealed || err != nil {
		t.Fatalf("an unlisted directory: sealed=%v err=%v", sealed, err)
	}
	if _, err := LoadHoldouts(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("a missing holdout file must be an error")
	}
}

func TestLedgerCountsTrials(t *testing.T) {
	path := LedgerPath(t.TempDir())
	if n, _, err := TrialCount(path, "s", "NIFTY"); n != 0 || err != nil {
		t.Fatalf("empty ledger: %d, %v", n, err)
	}
	spec := Spec{Strategy: "s", Underlying: "NIFTY", Params: strategy.Params{"a": 1},
		From: date(t, "2025-01-01"), To: date(t, "2025-06-30")}
	other := spec
	other.Strategy = "other"
	now := time.Date(2026, 11, 7, 9, 0, 0, 0, market.IST)
	recs := []TrialRecord{
		Record("backtest", spec, Metrics{Trades: 3}, 1, false, "abc", now),
		Record("walkforward", spec, Metrics{}, 12, true, "abc", now),
		Record("backtest", other, Metrics{}, 1, false, "abc", now),
	}
	if err := AppendTrials(path, recs[:2]); err != nil {
		t.Fatal(err)
	}
	if err := AppendTrials(path, recs[2:]); err != nil {
		t.Fatal(err)
	}
	n, unsealed, err := TrialCount(path, "s", "NIFTY")
	if n != 13 || unsealed != 1 || err != nil {
		t.Fatalf("count = %d unsealed %d, %v; want 13 and 1", n, unsealed, err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"from":"2025-01-01"`) {
		t.Fatalf("dates should be written as text: %s", b)
	}
}

func TestRejectionsGroupByReason(t *testing.T) {
	at := time.Date(2026, 11, 2, 10, 0, 0, 0, market.IST)
	rows := rejections([]engine.Decision{
		{Time: at, Verdict: engine.Accepted},
		{Time: at, Verdict: engine.Rejected, Reason: "no_price", Detail: "no price for A"},
		{Time: at.Add(time.Minute), Verdict: engine.Rejected, Reason: "margin", Detail: "margin ₹1"},
		{Time: at.Add(2 * time.Minute), Verdict: engine.Rejected, Reason: "margin", Detail: "margin ₹2"},
	})
	if len(rows) != 2 || rows[0].Reason != "margin" || rows[0].Count != 2 || rows[0].Example != "margin ₹1" ||
		rows[0].First != "2026-11-02 10:01" || rows[1].Count != 1 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestManifestWrites(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "run.yaml")
	os.WriteFile(cfg, []byte("strategy: s\n"), 0o644)
	sha, err := FileSHA256(cfg)
	if err != nil || len(sha) != 64 {
		t.Fatalf("sha %q, %v", sha, err)
	}
	o := Outcome{Spec: Spec{Strategy: "s", Underlying: "NIFTY", From: date(t, "2025-01-01"), To: date(t, "2025-02-01")},
		Metrics: Metrics{Trades: 4, Rejected: 1}}
	m := NewManifest(o, Provenance{Trials: 3}, cfg, sha, "c0575", "data/upstox", time.Now())
	path := filepath.Join(dir, "out", "r.json")
	if err := WriteManifest(path, m); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	for _, want := range []string{`"config_sha256": "` + sha, `"trials": 3`, `"from": "2025-01-01"`, `"revision": "`, `"rejected": 1`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("manifest missing %s:\n%s", want, b)
		}
	}
}
