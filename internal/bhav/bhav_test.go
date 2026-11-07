package bhav

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"calm/internal/market"
)

func date(t *testing.T, s string) market.Date {
	t.Helper()
	d, err := market.ParseDate(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func zipped(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(body)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func fixture(t *testing.T, day string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", day+".csv"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestURL(t *testing.T) {
	cases := map[string]string{
		"1994-11-03": base + "/historical/EQUITIES/1994/NOV/cm03NOV1994bhav.csv.zip",
		"2024-07-05": base + "/historical/EQUITIES/2024/JUL/cm05JUL2024bhav.csv.zip",
		"2024-07-08": base + "/cm/BhavCopy_NSE_CM_0_0_0_20240708_F_0000.csv.zip",
		"2026-11-06": base + "/cm/BhavCopy_NSE_CM_0_0_0_20261106_F_0000.csv.zip",
	}
	for d, want := range cases {
		if got := URL(date(t, d)); got != want {
			t.Errorf("%s: got %s, want %s", d, got, want)
		}
	}
}

func TestParseEveryFormat(t *testing.T) {
	type check struct {
		symbol, series, isin string
		open, close, prev    market.Paise
		volume, trades       int64
		turnover             float64
	}
	cases := map[string]check{
		// The first file: one-digit day, no previous close, no ISIN or trades.
		"1994-11-03": {"ABB", "EQ", "", 71500, 71500, 0, 100, 0, 71500},
		"2011-01-03": {"20MICRONS", "EQ", "", 4550, 4665, 4515, 26971, 0, 1243030.75},
		"2024-07-05": {"RELIANCE", "EQ", "INE002A01018", 310765, 317725, 310805, 6134855, 261494, 19323716027.9},
		"2026-11-06": {"RELIANCE", "EQ", "INE002A01018", 129500, 132000, 131340, 22768301, 327475, 29865210015.30},
	}
	for day, want := range cases {
		rows, err := Parse(fixture(t, day), date(t, day))
		if err != nil {
			t.Fatalf("%s: %v", day, err)
		}
		var got *market.EquityDay
		for i := range rows {
			if rows[i].Symbol == want.symbol {
				got = &rows[i]
			}
		}
		if got == nil {
			t.Fatalf("%s: no %s row in %+v", day, want.symbol, rows)
		}
		if got.Series != want.series || got.ISIN != want.isin || got.Open != want.open || got.Close != want.close ||
			got.PrevClose != want.prev || got.Volume != want.volume || got.Trades != want.trades ||
			got.Turnover != want.turnover {
			t.Errorf("%s: got %+v, want %+v", day, *got, want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	good := fixture(t, "2024-07-05")
	if _, err := Parse(good, date(t, "2024-07-04")); err == nil {
		t.Error("accepted a file dated another day")
	}
	lines := strings.SplitN(string(good), "\n", 3)
	dup := lines[0] + "\n" + lines[1] + "\n" + lines[1] + "\n"
	if _, err := Parse([]byte(dup), date(t, "2024-07-05")); err == nil {
		t.Error("accepted a security listed twice")
	}
	renamed := strings.Replace(string(good), "CLOSE,", "CLS,", 1)
	if _, err := Parse([]byte(renamed), date(t, "2024-07-05")); !errors.Is(err, ErrSchema) {
		t.Errorf("renamed column: got %v, want ErrSchema", err)
	}
}

func TestDownload(t *testing.T) {
	day := date(t, "2024-07-05")
	file := zipped(t, "cm05JUL2024bhav.csv", fixture(t, "2024-07-05"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/2024-07-05":
			w.Write(file)
		case "/2024-07-06": // served empty, as some early dates are
		case "/2024-07-03":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New()
	c.Interval, c.Backoff, c.Tries = 0, time.Millisecond, 2
	c.URL = func(d market.Date) string { return srv.URL + "/" + d.String() }
	dir := t.TempDir()
	cfg := Config{Dir: dir, From: day.AddDays(-1), To: day.AddDays(2), Log: t.Logf}

	res, err := Download(context.Background(), cfg, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.New) != 1 || res.New[0] != day {
		t.Fatalf("new: %v", res.New)
	}
	missing, _ := loadMissing(dir)
	if !missing["2024-07-04"] || !missing["2024-07-06"] {
		t.Errorf("settled 404s not remembered: %v", missing)
	}

	// A second run fetches nothing new and finds the stored file unchanged.
	c.Requests = 0
	res, err = Download(context.Background(), cfg, c)
	if err != nil || len(res.New)+len(res.Changed) != 0 || c.Requests != 1 {
		t.Fatalf("second run: %+v, %d requests, %v", res, c.Requests, err)
	}

	// A server failure stops the run with an error.
	cfg.From = day.AddDays(-2)
	if _, err := Download(context.Background(), cfg, c); err == nil {
		t.Error("server failure not reported")
	}
}
