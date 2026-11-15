package etf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"serene/internal/kite"
	"serene/internal/market"
	"serene/internal/upstox"
)

// UpstoxFrom is the first month Upstox serves 1-minute candles for.
var UpstoxFrom = time.Date(2022, time.January, 1, 0, 0, 0, 0, market.IST)

// Kite's history caps per request, a little under its limits.
const (
	kiteMinuteDays = 59
	kiteDayDays    = 1900
)

// noData is Upstox's answer for an ISIN it has nothing for.
var noData = regexp.MustCompile(`^HTTP 4(00|04) `)

// LoadTokens reads Kite's NSE instrument tokens; ETFs missing from it are
// fetched from Upstox by ISIN.
func (f *Fetcher) LoadTokens(ctx context.Context) error {
	ins, err := f.Kite.Instruments(ctx, "NSE")
	if err != nil {
		return err
	}
	f.tokens = map[string]int64{}
	for _, in := range ins {
		if in.Segment == "NSE" {
			f.tokens[in.Symbol] = in.Token
		}
	}
	return nil
}

// Result is what one Fetch changed.
type Result struct {
	Daily, Minute           int // bars in the files
	NewDaily, NewMinute     int
	FromBhav                int
	DroppedZero             int
	KiteListed, UpstoxFound bool
}

// Fetch brings an ETF's files up to date: the whole history the first time,
// after that the bars since the last one. It also fills, from Upstox, any
// month since 2022 before the first 1-minute bar, which Kite lacks for a fund
// renamed since, and refills daily gaps from the bhavcopy. A fetch stopped
// part way keeps the windows it downloaded in raw/ and reuses them.
func (f *Fetcher) Fetch(ctx context.Context, e ETF) (Result, error) {
	var res Result
	dPath, mPath := f.Path(e, "daily"), f.Path(e, "1m")
	daily, err := f.read(dPath)
	if err != nil {
		return res, err
	}
	minute, err := f.read(mPath)
	if err != nil {
		return res, err
	}
	// Bhav days are refilled below, adjusted afresh.
	daily = deleteBhav(daily)
	nDaily, nMinute := len(daily), len(minute)
	now := time.Now().In(market.IST)
	cache := filepath.Join(f.Dir, e.Country, "raw", e.Symbol)
	tok, listed := f.tokens[e.Symbol]
	listed = listed && f.Kite != nil
	res.KiteListed = listed
	up := f.Upstox
	if e.ISIN() == "" {
		up = nil
	}
	if !listed && up == nil {
		return res, fmt.Errorf("%s: not in Kite's instruments and Upstox is off", e.Symbol)
	}

	// Daily: from the last bar, or the whole history.
	dSince := time.Time{}
	if n := len(daily); n > 0 {
		dSince = daily[n-1].T
	}
	var got []Bar
	if listed {
		got, err = f.kiteBars(ctx, e, tok, "day", kiteDayDays, "", dSince, now)
	} else {
		got, err = upstoxDaily(ctx, up, e, dSince, now)
	}
	if err != nil {
		return res, err
	}
	daily = append(daily, got...)

	// 1-minute.
	var mSince time.Time
	if n := len(minute); n > 0 {
		mSince = minute[n-1].T.Add(time.Minute)
	}
	switch {
	case listed && len(minute) == 0 && up != nil:
		got, err = f.sharedMinutes(ctx, e, tok, cache, now)
	case listed:
		got, err = f.kiteBars(ctx, e, tok, "minute", kiteMinuteDays, cache, mSince, now)
	default:
		got, _, err = upstoxMinutes(ctx, up, e, cache, newMonths(e, mSince, now))
	}
	if err != nil {
		return res, err
	}
	minute = append(minute, got...)

	// Months since 2022 before the first bar: lost to a rename at Kite.
	if f.Upstox == nil {
		up = nil // its login was rejected above
	}
	if up != nil {
		got, err = f.gapMinutes(ctx, up, e, cache, minute, now)
		if errors.Is(err, upstox.ErrAuth) {
			return res, err
		}
		if err != nil {
			f.logf("%s: %v", e.Symbol, err)
		}
		minute = append(minute, got...)
	}

	bh, err := f.bhavDays(e)
	if err != nil {
		return res, err
	}
	daily, res.FromBhav = fill(deleteBhav(daily), bh)
	var dz, mz int
	daily, dz = merge(daily)
	minute, mz = merge(minute)
	res.DroppedZero = dz + mz
	res.Daily, res.Minute = len(daily), len(minute)
	res.NewDaily, res.NewMinute = max(0, len(daily)-nDaily-res.FromBhav), max(0, len(minute)-nMinute)
	if err := f.write(e, dPath, daily); err != nil {
		return res, err
	}
	if len(minute) != nMinute || !exists(mPath) {
		if err := f.write(e, mPath, minute); err != nil {
			return res, err
		}
	}
	return res, nil
}

func deleteBhav(bars []Bar) []Bar {
	out := bars[:0]
	for _, b := range bars {
		if !strings.HasPrefix(b.Source, "bhav") {
			out = append(out, b)
		}
	}
	return out
}

// sharedMinutes is a first full download with both brokers: Kite takes the
// years before 2022, then months from 2022 on from the oldest end of a queue
// while Upstox takes them from the newest end. Months Upstox has nothing for
// go to Kite.
func (f *Fetcher) sharedMinutes(ctx context.Context, e ETF, tok int64, cache string, now time.Time) ([]Bar, error) {
	q := newMonths(e, time.Time{}, now)
	var upBars []Bar
	var upEmpty []time.Time
	var upErr error
	var wg sync.WaitGroup
	wg.Go(func() { upBars, upEmpty, upErr = upstoxMinutes(ctx, f.Upstox, e, cache, q) })
	month := func(m time.Time) ([]Bar, error) {
		end := m.AddDate(0, 1, 0).Add(-time.Second)
		if end.After(now) {
			end = now
		}
		return f.kiteBars(ctx, e, tok, "minute", kiteMinuteDays, cache, m, end)
	}
	out, err := f.kiteBars(ctx, e, tok, "minute", kiteMinuteDays, cache, time.Time{}, UpstoxFrom.Add(-time.Second))
	for err == nil {
		m, ok := q.oldest()
		if !ok {
			break
		}
		var bs []Bar
		bs, err = month(m)
		out = append(out, bs...)
	}
	wg.Wait()
	if err != nil {
		return nil, err
	}
	if upErr != nil {
		f.logf("%s: %v: Kite takes the rest", e.Symbol, upErr)
		if errors.Is(upErr, upstox.ErrAuth) {
			f.Upstox = nil
		}
	}
	out = append(out, upBars...)
	for {
		m, ok := q.oldest() // left behind if Upstox stopped early
		if !ok {
			break
		}
		upEmpty = append(upEmpty, m)
	}
	for _, m := range upEmpty {
		bs, err := month(m)
		if err != nil {
			return nil, err
		}
		out = append(out, bs...)
	}
	return out, nil
}

// gapMinutes fetches from Upstox the months since 2022 (or the listing) up to
// the first 1-minute bar that have no bars, and the first bar's month when it
// starts after the 3rd. Upstox keeps a fund's history across renames.
func (f *Fetcher) gapMinutes(ctx context.Context, up *upstox.Client, e ETF, cache string, minute []Bar, now time.Time) ([]Bar, error) {
	first := now
	have := map[string]bool{}
	for _, b := range minute {
		if b.T.Before(first) {
			first = b.T
		}
		have[b.T.Format("2006-01")] = true
	}
	firstMonth := time.Date(first.Year(), first.Month(), 1, 0, 0, 0, 0, market.IST)
	q := &months{}
	for m := startMonth(e, time.Time{}); !m.After(firstMonth); m = m.AddDate(0, 1, 0) {
		partial := m.Equal(firstMonth) && first.Day() > 3
		if !have[m.Format("2006-01")] || partial {
			q.ms = append(q.ms, m)
		}
	}
	q.hi = len(q.ms)
	bars, _, err := upstoxMinutes(ctx, up, e, cache, q)
	return bars, err
}

// kiteBars walks back from until in windows of span days to the later of the
// listing and since, stopping after three empty windows. Windows are cached
// under cache, except one reaching now.
func (f *Fetcher) kiteBars(ctx context.Context, e ETF, tok int64, interval string, span int, cache string,
	since, until time.Time) ([]Bar, error) {
	first := time.Date(e.First.Year(), e.First.Month(), e.First.Day(), 0, 0, 0, 0, market.IST)
	if since.After(first) {
		first = since
	}
	var out []Bar
	empty := 0
	for to := until; !to.Before(first) && empty < 3; {
		from := to.AddDate(0, 0, -span)
		from = time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, market.IST)
		if from.Before(first) {
			from = first
		}
		cs, err := f.kiteCached(ctx, tok, interval, from, to, cache, !to.Before(time.Now().Add(-time.Hour)))
		if err != nil {
			return nil, fmt.Errorf("%s: kite %s %s..%s: %w", e.Symbol, interval, from.Format(time.DateOnly), to.Format(time.DateOnly), err)
		}
		if len(cs) == 0 {
			empty++
		} else {
			empty = 0
		}
		for _, c := range cs {
			out = append(out, Bar{c.Time, c.Open, c.High, c.Low, c.Close, c.Volume, "kite"})
		}
		to = from.Add(-time.Second)
	}
	return out, nil
}

func (f *Fetcher) kiteCached(ctx context.Context, tok int64, interval string, from, to time.Time, dir string, live bool) ([]kite.Candle, error) {
	path := ""
	if dir != "" && !live {
		path = filepath.Join(dir, "k-"+from.Format("20060102T1504")+"-"+to.Format("20060102T1504")+".json")
		if b, err := os.ReadFile(path); err == nil {
			var cs []kite.Candle
			if json.Unmarshal(b, &cs) == nil {
				return cs, nil
			}
		}
	}
	cs, err := f.Kite.Candles(ctx, tok, interval, from, to, false)
	if err != nil {
		return nil, err
	}
	if path != "" {
		if err := saveJSON(path, cs); err != nil {
			return nil, err
		}
	}
	return cs, nil
}

func saveJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// upstoxDaily fetches daily candles by ISIN in 10-year windows back to since
// or the listing.
func upstoxDaily(ctx context.Context, up *upstox.Client, e ETF, since, now time.Time) ([]Bar, error) {
	stop := e.First
	if since.After(stop) {
		stop = since
	}
	var out []Bar
	for to := market.DateOf(now); ; {
		from := market.DateOf(to.At(0, 0).AddDate(-10, 0, 1))
		if s := market.DateOf(stop); from.Before(s) {
			from = s
		}
		cs, err := up.DailyCandles(ctx, "NSE_EQ|"+e.ISIN(), from, to)
		if err != nil && !noData.MatchString(err.Error()) {
			return nil, fmt.Errorf("%s: upstox daily: %w", e.Symbol, err)
		}
		for _, c := range cs {
			out = append(out, Bar{c.Time, c.Open, c.High, c.Low, c.Close, c.Volume, "upstox"})
		}
		if len(cs) == 0 || !from.At(0, 0).After(stop) {
			return out, nil
		}
		to = market.DateOf(from.At(0, 0).AddDate(0, 0, -1))
	}
}

// months is a queue of months two brokers draw from, one from each end.
type months struct {
	mu     sync.Mutex
	ms     []time.Time
	lo, hi int
}

// startMonth is the first month Upstox could serve: 2022-01, or the listing
// month, or since's month, whichever is latest.
func startMonth(e ETF, since time.Time) time.Time {
	m := time.Date(e.First.Year(), e.First.Month(), 1, 0, 0, 0, 0, market.IST)
	if s := time.Date(since.Year(), since.Month(), 1, 0, 0, 0, 0, market.IST); s.After(m) {
		m = s
	}
	if m.Before(UpstoxFrom) {
		m = UpstoxFrom
	}
	return m
}

// newMonths queues the months from startMonth to now.
func newMonths(e ETF, since, now time.Time) *months {
	q := &months{}
	for m := startMonth(e, since); !m.After(now); m = m.AddDate(0, 1, 0) {
		q.ms = append(q.ms, m)
	}
	q.hi = len(q.ms)
	return q
}

func (q *months) oldest() (time.Time, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.lo >= q.hi {
		return time.Time{}, false
	}
	q.lo++
	return q.ms[q.lo-1], true
}

func (q *months) newest() (time.Time, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.lo >= q.hi {
		return time.Time{}, false
	}
	q.hi--
	return q.ms[q.hi], true
}

// upstoxMinutes fetches 1-minute candles a month at a time from the newest end
// of q, by the current ISIN. Finished months are cached as up-YYYY-MM.json.
// It returns the months that came back empty, and on an error the months it
// didn't do are left in q.
func upstoxMinutes(ctx context.Context, up *upstox.Client, e ETF, cache string, q *months) ([]Bar, []time.Time, error) {
	now := time.Now().In(market.IST)
	var out []Bar
	var empty []time.Time
	for {
		m, ok := q.newest()
		if !ok {
			return out, empty, nil
		}
		end := m.AddDate(0, 1, 0)
		done := !end.After(now.AddDate(0, 0, -1))
		path := filepath.Join(cache, "up-"+m.Format("2006-01")+".json")
		var cs []upstox.Candle
		hit := false
		if done {
			if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &cs) == nil {
				hit = true
			}
		}
		if !hit {
			var err error
			cs, err = up.Candles(ctx, "NSE_EQ|"+e.ISIN(), market.DateOf(m), market.DateOf(end.AddDate(0, 0, -1)))
			if err != nil && !noData.MatchString(err.Error()) {
				q.mu.Lock()
				q.hi++ // give the month back
				q.mu.Unlock()
				return out, empty, fmt.Errorf("upstox 1m %s: %w", m.Format("2006-01"), err)
			}
			if done {
				if err := saveJSON(path, cs); err != nil {
					return out, empty, err
				}
			}
		}
		if len(cs) == 0 {
			empty = append(empty, m)
		}
		for _, c := range cs {
			out = append(out, Bar{c.Time, c.Open, c.High, c.Low, c.Close, c.Volume, "upstox"})
		}
	}
}
