package fobhav

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"serene/internal/bhav"
	"serene/internal/market"
)

// UDiFFFrom is NSE's first UDiFF F&O file.
var UDiFFFrom = market.Date{Year: 2024, Month: time.July, Day: 8}

// BSEFrom is about where BSE's derivatives bhavcopy archive begins.
var BSEFrom = market.Date{Year: 2024, Month: time.January, Day: 1}

// NSEURL is where NSE publishes a day's F&O bhavcopy.
func NSEURL(d market.Date) string {
	const base = "https://nsearchives.nseindia.com/content"
	if d.Before(UDiFFFrom) {
		mon := strings.ToUpper(d.Month.String()[:3])
		return fmt.Sprintf("%s/historical/DERIVATIVES/%d/%s/fo%02d%s%dbhav.csv.zip", base, d.Year, mon, d.Day, mon, d.Year)
	}
	return fmt.Sprintf("%s/fo/BhavCopy_NSE_FO_0_0_0_%04d%02d%02d_F_0000.csv.zip", base, d.Year, int(d.Month), d.Day)
}

// BSEURL is where BSE publishes a day's derivatives bhavcopy, unzipped.
func BSEURL(d market.Date) string {
	return fmt.Sprintf("https://www.bseindia.com/download/Bhavcopy/Derivative/BhavCopy_BSE_FO_0_0_0_%04d%02d%02d_F_0000.CSV",
		d.Year, int(d.Month), d.Day)
}

// Dirs are where the raw files live under the Fyers store.
func nseDir(root string) string { return filepath.Join(root, "bhav_fo", "nse") }
func bseDir(root string) string { return filepath.Join(root, "bhav_fo", "bse") }

// DownloadNSE fetches every NSE F&O bhavcopy in [from, to] not yet stored,
// using the cash-market downloader's resume and recheck rules.
func DownloadNSE(ctx context.Context, root string, from, to market.Date, log func(string, ...any)) error {
	c := bhav.New()
	c.URL = NSEURL
	_, err := bhav.Download(ctx, bhav.Config{Dir: nseDir(root), From: from, To: to, Log: log}, c)
	return err
}

func bsePath(root string, d market.Date) string {
	return filepath.Join(bseDir(root), "raw", fmt.Sprint(d.Year), d.String()+".csv")
}

// DownloadBSE fetches one BSE file a week from BSEFrom: each lists every live
// SENSEX and BANKEX contract with its lot, and lots change rarely, so a weekly
// sample sees every expiry. A week whose Monday has no file tries the next day.
func DownloadBSE(ctx context.Context, root string, to market.Date, log func(string, ...any)) error {
	hc := &http.Client{Timeout: 60 * time.Second}
	got := 0
	for week := BSEFrom; !to.Before(week); week = week.AddDays(7) {
		for d := week; d.Before(week.AddDays(5)) && !to.Before(d); d = d.AddDays(1) {
			if _, err := os.Stat(bsePath(root, d)); err == nil {
				break
			}
			body, err := fetchBSE(ctx, hc, d)
			if errors.Is(err, bhav.ErrNotPublished) {
				continue
			}
			if err != nil {
				return fmt.Errorf("bse %s: %w", d, err)
			}
			if err := os.MkdirAll(filepath.Dir(bsePath(root, d)), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(bsePath(root, d), body, 0o644); err != nil {
				return err
			}
			got++
			break
		}
	}
	log("bse: %d new files", got)
	return nil
}

// fetchBSE gets one file. BSE answers a missing day with its HTML home page.
func fetchBSE(ctx context.Context, hc *http.Client, d market.Date) ([]byte, error) {
	delay := 5 * time.Second
	var last error
	for try := range 4 {
		if try > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
			delay *= 2
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, BSEURL(d), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "serene/0.1")
		resp, err := hc.Do(req)
		if err != nil {
			last = err
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			last = err
			continue
		}
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return nil, bhav.ErrNotPublished
		case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden:
			last = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		case resp.StatusCode != http.StatusOK:
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		case !bytes.HasPrefix(bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF}), []byte("TradDt,")):
			return nil, bhav.ErrNotPublished
		}
		return body, nil
	}
	return nil, fmt.Errorf("gave up after 4 tries: %w", last)
}

// rawFile is one stored bhavcopy.
type rawFile struct {
	Date market.Date
	Path string
	Zip  bool
}

func listRaw(dir, ext string, zipped bool) ([]rawFile, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "raw", "*", "*"+ext))
	if err != nil {
		return nil, err
	}
	var out []rawFile
	for _, m := range matches {
		d, err := market.ParseDate(strings.TrimSuffix(filepath.Base(m), ext))
		if err != nil {
			return nil, err
		}
		out = append(out, rawFile{Date: d, Path: m, Zip: zipped})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out, nil
}

func (f rawFile) rows() ([]Row, error) {
	b, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, err
	}
	if f.Zip {
		if b, err = bhav.Unzip(b); err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
	}
	rows, err := Parse(b, f.Date)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f.Path, err)
	}
	return rows, nil
}

// MktURL is NSE's F&O market activity report for a day. Its foDDMMYYYY.csv
// lists each future's traded quantity and contracts, whose ratio is the lot:
// the one exact lot source before UDiFF.
func MktURL(d market.Date) string {
	return fmt.Sprintf("https://nsearchives.nseindia.com/archives/fo/mkt/fo%02d%02d%04d.zip", d.Day, int(d.Month), d.Year)
}

func mktDir(root string) string { return filepath.Join(root, "bhav_fo", "nse_mkt") }

func mktPath(root string, d market.Date) string {
	return filepath.Join(mktDir(root), "raw", fmt.Sprint(d.Year), d.String()+".zip")
}

// DownloadMkt fetches the market activity report for the first trading day
// of each week before UDiFF, taking trading days from the stored bhavcopies.
// Every monthly future trades for about three months, so a weekly sample sees
// each expiry's lot about a dozen times.
func DownloadMkt(ctx context.Context, root string, log func(string, ...any)) error {
	files, err := listRaw(nseDir(root), ".csv.zip", true)
	if err != nil {
		return err
	}
	c := bhav.New()
	c.URL = MktURL
	seen := map[[2]int]bool{}
	got := 0
	for _, f := range files {
		if !f.Date.Before(UDiFFFrom) {
			break
		}
		y, w := f.Date.At(12, 0).ISOWeek()
		if seen[[2]int{y, w}] {
			continue
		}
		seen[[2]int{y, w}] = true
		if _, err := os.Stat(mktPath(root, f.Date)); err == nil {
			continue
		}
		body, err := c.Fetch(ctx, f.Date)
		if errors.Is(err, bhav.ErrNotPublished) {
			delete(seen, [2]int{y, w}) // try the week's next day
			continue
		}
		if err != nil {
			return fmt.Errorf("market activity %s: %w", f.Date, err)
		}
		if err := os.MkdirAll(filepath.Dir(mktPath(root, f.Date)), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(mktPath(root, f.Date), body, 0o644); err != nil {
			return err
		}
		if got++; got%50 == 0 {
			log("market activity: %d files, up to %s", got, f.Date)
		}
	}
	log("market activity: %d new files", got)
	return nil
}
