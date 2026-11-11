package nse

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"serene/internal/data"
	"serene/internal/market"
)

func date(t *testing.T, s string) market.Date {
	t.Helper()
	d, err := market.ParseDate(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func sample(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const head = "Index Name,Index Date,Open Index Value,High Index Value,Low Index Value,Closing Index Value,Points Change,Change(%),Volume,Turnover (Rs. Cr.),P/E,P/B,Div Yield\n"

func TestParseRealFiles(t *testing.T) {
	for _, tc := range []struct {
		file, day, first string
		close            market.Paise
	}{
		{"2013-04-01.csv", "2013-04-01", "CNX Nifty", 570440},
		{"2016-04-01.csv", "2016-04-01", "Nifty 50", 771305},
		{"2026-11-06.csv", "2026-11-06", "Nifty 50", 2503035},
	} {
		rows, err := Parse(sample(t, tc.file), date(t, tc.day))
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if rows[0].Source != tc.first || rows[0].Close != tc.close {
			t.Errorf("%s: first row %+v", tc.file, rows[0])
		}
	}
}

func TestParseValues(t *testing.T) {
	rows, err := Parse(sample(t, "2026-11-06.csv"), date(t, "2026-11-06"))
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]market.IndexDay{}
	for _, r := range rows {
		by[r.Source] = r
	}
	n := by["Nifty 50"]
	if n.Open != 2453820 || n.High != 2503035 || n.Low != 2438805 || n.PrevClose != 2453660 ||
		n.Volume != 490973304 || n.PE != 20.65 || n.PB != 3.07 || n.Yield != 1.41 || n.TurnoverCr != 49834.48 {
		t.Errorf("Nifty 50: %+v", n)
	}
	vix, ok := by["India VIX"]
	if !ok || vix.Volume != 0 || !math.IsNaN(vix.PE) {
		t.Errorf("VIX should have no volume or P/E: %+v", vix)
	}
	if w := by["Nifty Waves"]; !math.IsNaN(w.PE) || math.IsNaN(w.PB) {
		t.Errorf("Waves has no P/E but has a P/B: %+v", w)
	}
}

func TestParseOddFields(t *testing.T) {
	raw := head +
		"A,01-04-2013,5697.35,5720.95,5675.9,5704.4,-.25,-.15,100,1,.38,.5,-\n" +
		"Empty,01-04-2013,-,-,-,0,0,-,-,-,-,-,-\n" +
		"NoPts,01-04-2013,-,-,-,12.5,-,-,-,-,-,-,-\n" +
		"Stale,31-03-2013,1,1,1,1,0,0,1,1,1,1,1\n"
	rows, err := Parse([]byte(raw), date(t, "2013-04-01"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want A and NoPts, got %+v", rows)
	}
	if a := rows[0]; a.PE != 0.38 || a.PrevClose != 570465 || !math.IsNaN(a.Yield) {
		t.Errorf("A: %+v", a)
	}
	if p := rows[1]; p.PrevClose != 0 || p.Open != 0 || p.Close != 1250 {
		t.Errorf("NoPts: %+v", p)
	}
}

func TestParseOtherDateForms(t *testing.T) {
	for raw, day := range map[string]string{
		"09/06/2014": "2014-06-09", // 2014 and 2015 files use slashes
		"04-06-2023": "2023-04-06", // three April 2023 files are month first
	} {
		rows, err := Parse([]byte(head+"A,"+raw+",1,1,1,1,0,0,1,1,1,1,1\n"), date(t, day))
		if err != nil || rows[0].Date != date(t, day) {
			t.Errorf("%s: %+v %v", raw, rows, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	d := date(t, "2013-04-01")
	row := "A,01-04-2013,1,1,1,1,0,0,1,1,1,1,1\n"
	for name, raw := range map[string]string{
		"wrong date":     head + "A,02-04-2013,1,1,1,1,0,0,1,1,1,1,1\n",
		"no rows":        head,
		"short row":      head + "A,01-04-2013,1\n",
		"bad number":     head + "A,01-04-2013,x,1,1,1,0,0,1,1,1,1,1\n",
		"unreadable day": head + "A,someday,1,1,1,1,0,0,1,1,1,1,1\n",
	} {
		if _, err := Parse([]byte(raw), d); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	changed := strings.Replace(head, "P/E", "PE Ratio", 1) + row
	if _, err := Parse([]byte(changed), d); !errors.Is(err, ErrSchema) {
		t.Errorf("changed columns: got %v", err)
	}
}

func catalog(t *testing.T, yaml string) *Catalog {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCatalog(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCatalog(t *testing.T) {
	c := catalog(t, `indices:
  - {name: Nifty Bank, group: sectoral, breadth: true, aliases: [CNX Bank]}
  - {name: NIFTY Midcap 100, group: broad}
`)
	rows := []market.IndexDay{{Source: "CNX BANK", Name: "CNX BANK"}, {Source: "Nifty  Midcap 100", Name: "Nifty  Midcap 100"}, {Source: "New", Name: "New"}}
	rows, unknown, err := c.Apply(rows)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Name != "Nifty Bank" || rows[1].Name != "NIFTY Midcap 100" || rows[2].Name != "New" {
		t.Errorf("names: %+v", rows)
	}
	if len(unknown) != 1 || unknown[0] != "New" {
		t.Errorf("unknown: %v", unknown)
	}
	if c.Group("Nifty Bank") != GroupSectoral {
		t.Error("group")
	}
	dup := []market.IndexDay{{Source: "CNX Bank"}, {Source: "Nifty Bank"}}
	if _, _, err := c.Apply(dup); err == nil {
		t.Error("two names for one index on one day must fail")
	}
	// A name repeated in one file keeps its first row.
	rep, _, err := c.Apply([]market.IndexDay{{Source: "Nifty Bank", Close: 1}, {Source: "Nifty Bank", Close: 2}})
	if err != nil || len(rep) != 1 || rep[0].Close != 1 {
		t.Errorf("repeat: %+v %v", rep, err)
	}
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("indices:\n  - {name: A, aliases: [X]}\n  - {name: B, aliases: [X]}\n"), 0o644)
	if _, err := LoadCatalog(p); err == nil {
		t.Error("an alias claimed twice must fail")
	}
}

// fake serves files by name from a map.
func fake(files map[string]string, status map[string]int, hits *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if s, ok := status[name]; ok {
			w.WriteHeader(s)
			return
		}
		body, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, body)
	}))
}

func client(urls ...string) *Client {
	return &Client{Bases: urls, Tries: 2, Backoff: time.Millisecond, HTTP: http.DefaultClient}
}

func day(s string) string { return head + "Nifty 50," + s + ",1,1,1,1,0,0,1,1,1,1,1\n" }

func TestFetchFallsBackAndRetries(t *testing.T) {
	var a, b atomic.Int64
	d := date(t, "2026-11-06")
	broken := fake(nil, map[string]int{fileName(d): 503}, &a)
	defer broken.Close()
	good := fake(map[string]string{fileName(d): day("06-11-2026")}, nil, &b)
	defer good.Close()
	if _, err := client(broken.URL, good.URL).Fetch(context.Background(), d, false); err != nil {
		t.Fatal(err)
	}
	if a.Load() != 2 || b.Load() != 1 {
		t.Errorf("hits %d %d", a.Load(), b.Load())
	}
}

func TestFetchNotPublishedVsFailure(t *testing.T) {
	var n atomic.Int64
	d := date(t, "2026-11-06")
	none := fake(nil, nil, &n)
	defer none.Close()
	down := fake(nil, map[string]int{fileName(d): 503}, &n)
	defer down.Close()
	ctx := context.Background()
	if _, err := client(none.URL, none.URL).Fetch(ctx, d, false); !errors.Is(err, ErrNotPublished) {
		t.Errorf("both 404: %v", err)
	}
	// A mirror being down must not pass for a holiday.
	if _, err := client(none.URL, down.URL).Fetch(ctx, d, false); err == nil || errors.Is(err, ErrNotPublished) {
		t.Errorf("one 404, one down: %v", err)
	}
	before := n.Load()
	client(none.URL, none.URL).Fetch(ctx, d, true)
	if n.Load()-before != 1 {
		t.Error("firstOnly must ask one mirror")
	}
	// niftyindices answers a missing file with 200 and an HTML page.
	html := fake(map[string]string{fileName(d): "<!DOCTYPE html><html></html>"}, nil, &n)
	defer html.Close()
	if _, err := client(html.URL).Fetch(ctx, d, false); !errors.Is(err, ErrNotPublished) {
		t.Errorf("HTML page: %v", err)
	}
	// An HTML page from one mirror must not hide the other's outage.
	if _, err := client(html.URL, down.URL).Fetch(ctx, d, false); err == nil || errors.Is(err, ErrNotPublished) {
		t.Errorf("HTML and down: %v", err)
	}
	junk := fake(map[string]string{fileName(d): "garbage"}, nil, &n)
	defer junk.Close()
	if _, err := client(junk.URL).Fetch(ctx, d, false); err == nil || errors.Is(err, ErrNotPublished) {
		t.Errorf("junk must be a failure: %v", err)
	}
}

func TestDownloadResumesRechecksAndRebuilds(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mon, tue := date(t, "2024-03-04"), date(t, "2024-03-05")
	files := map[string]string{fileName(mon): day("04-03-2024"), fileName(tue): day("05-03-2024")}
	var hits atomic.Int64
	srv := fake(files, nil, &hits)
	defer srv.Close()
	cfg := Config{Dir: dir, From: date(t, "2024-03-02"), To: date(t, "2024-03-06"), Log: func(string, ...any) {}}

	res, err := Download(ctx, cfg, client(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.New) != 2 || len(res.Changed) != 0 {
		t.Fatalf("first run: %+v", res)
	}

	// The second run asks only for the recheck, and notices a rewritten file.
	files[fileName(tue)] = strings.Replace(day("05-03-2024"), ",1,1,1,1,0,0", ",2,2,2,2,0,0", 1)
	hits.Store(0)
	res, err = Download(ctx, cfg, client(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.New) != 0 || len(res.Changed) != 1 || res.Changed[0] != tue {
		t.Fatalf("second run: %+v", res)
	}
	if hits.Load() != 2 {
		t.Errorf("known gaps were asked for again: %d requests", hits.Load())
	}

	store, err := data.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat := catalog(t, "indices:\n  - {name: Nifty 50, group: broad}\n")
	unknown, err := Rebuild(ctx, dir, cat, store, []int{2024})
	if err != nil || len(unknown) != 0 {
		t.Fatalf("rebuild: %v %v", unknown, err)
	}
	rows, err := store.IndexDays(ctx, mon, tue)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Date != mon || rows[1].Close != market.Rupees(2) {
		t.Errorf("rows: %+v", rows)
	}
	// A rebuild replaces the year, so it can't leave duplicates.
	if _, err := Rebuild(ctx, dir, cat, store, []int{2024}); err != nil {
		t.Fatal(err)
	}
	if rows, _ := store.IndexDays(ctx, mon, tue); len(rows) != 2 {
		t.Errorf("rebuild duplicated rows: %d", len(rows))
	}
}

func TestDownloadStopsOnFailure(t *testing.T) {
	d := date(t, "2024-03-05")
	var n atomic.Int64
	srv := fake(nil, map[string]int{fileName(d): 500}, &n)
	defer srv.Close()
	dir := t.TempDir()
	cfg := Config{Dir: dir, From: d, To: d, Log: func(string, ...any) {}}
	if _, err := Download(context.Background(), cfg, client(srv.URL)); err == nil {
		t.Fatal("want an error")
	}
	if m, _ := loadMissing(dir); m[d.String()] {
		t.Error("a server failure must not be remembered as a gap")
	}
}

func TestExpectedDate(t *testing.T) {
	for today, want := range map[string]string{
		"2026-11-07": "2026-11-06", // Saturday reports Friday
		"2026-11-08": "2026-11-06",
		"2026-11-09": "2026-11-06", // Monday still reports Friday
		"2026-11-10": "2026-11-09",
	} {
		if got := ExpectedDate(date(t, today)); got != date(t, want) {
			t.Errorf("%s: %s", today, got)
		}
	}
}

func TestSentRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if s, err := LoadSent(dir); err != nil || s != (Sent{}) {
		t.Fatal(s, err)
	}
	if err := SaveSent(dir, Sent{Reported: "2026-11-06"}); err != nil {
		t.Fatal(err)
	}
	if s, _ := LoadSent(dir); s.Reported != "2026-11-06" {
		t.Error(s)
	}
}

func TestSentNext(t *testing.T) {
	d := date(t, "2026-11-06")
	for _, tc := range []struct {
		s    Sent
		want Step
	}{
		{Sent{}, SendAll},
		{Sent{Reported: "2026-11-05", Attached: "2026-11-05"}, SendAll},
		{Sent{Reported: "2026-11-06", Attached: "2026-11-05"}, AttachOnly},
		{Sent{Reported: "2026-11-06", Attached: "2026-11-06"}, Done},
	} {
		if got := tc.s.Next(d); got != tc.want {
			t.Errorf("%+v: %d", tc.s, got)
		}
	}
}

func TestContinuity(t *testing.T) {
	a, b, c, e := date(t, "2024-03-04"), date(t, "2024-03-05"), date(t, "2024-03-06"), date(t, "2024-03-07")
	row := func(d market.Date, src string, cl, prev market.Paise) market.IndexDay {
		return market.IndexDay{Date: d, Name: "X", Source: src, Close: cl, PrevClose: prev}
	}
	rows := []market.IndexDay{
		row(a, "Old", 10000, 9900),
		row(b, "New", 10100, 10000), // a clean rename
		row(c, "New", 50000, 25000), // joined to the wrong series
		row(e, "New", 50100, 0),     // no published change, so nothing to compare
	}
	got := Continuity(rows, []market.Date{a, b, c, e}, 0.05)
	if len(got) != 1 || got[0].Date != c || got[0].Stored != 10100 || got[0].Said != 25000 || got[0].Gap {
		t.Errorf("%+v", got)
	}
	gap := Continuity([]market.IndexDay{row(a, "A", 100, 100), row(c, "A", 120, 110)}, []market.Date{a, b, c}, 0.05)
	if len(gap) != 1 || !gap[0].Gap {
		t.Errorf("%+v", gap)
	}
}
