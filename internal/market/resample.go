package market

import (
	"fmt"
	"time"
)

// Resampler builds higher-timeframe bars from 1-minute bars of a single
// instrument. The same code runs in replay and live, so a strategy sees the
// same bars in every mode.
type Resampler struct {
	session Session
	tf      Timeframe
	cur     *Bar
	lastIn  time.Time
}

// NewResampler returns a resampler for tf.
func NewResampler(s Session, tf Timeframe) (*Resampler, error) {
	if tf <= 0 {
		return nil, fmt.Errorf("invalid timeframe %d", tf)
	}
	return &Resampler{session: s, tf: tf}, nil
}

// Add consumes a 1-minute bar and returns any bars this completes: the prior
// bucket when b starts a new one, then b's bucket if b was its final minute.
func (r *Resampler) Add(b Bar) ([]Bar, error) {
	if b.Timeframe != Minute1 {
		return nil, fmt.Errorf("resampler needs 1m bars, got %s for %s", b.Timeframe, b.Key)
	}
	if !r.session.Contains(b.Time) {
		return nil, fmt.Errorf("bar outside session: %s at %s", b.Key, b.Time.In(IST))
	}
	if !r.lastIn.IsZero() && !b.Time.After(r.lastIn) {
		return nil, fmt.Errorf("bars out of order: %s at %s after %s", b.Key, b.Time.In(IST), r.lastIn.In(IST))
	}
	r.lastIn = b.Time

	var done []Bar
	start, end := r.session.Bucket(b.Time, r.tf)
	if r.cur != nil && !r.cur.Time.Equal(start) {
		done = append(done, *r.cur)
		r.cur = nil
	}
	if r.cur == nil {
		r.cur = &Bar{Key: b.Key, Timeframe: r.tf, Time: start, End: end,
			Open: b.Open, High: b.High, Low: b.Low}
	}
	r.cur.High = max(r.cur.High, b.High)
	r.cur.Low = min(r.cur.Low, b.Low)
	r.cur.Close = b.Close
	r.cur.Volume += b.Volume
	r.cur.OI = b.OI
	if !b.End.Before(end) {
		done = append(done, *r.cur)
		r.cur = nil
	}
	return done, nil
}

// FlushUntil returns the open bar if its bucket has ended by now. Illiquid
// contracts can have no candle in a bucket's final minute, and without this
// their bar would wait for the next trade.
func (r *Resampler) FlushUntil(now time.Time) (Bar, bool) {
	if r.cur == nil || now.Before(r.cur.End) {
		return Bar{}, false
	}
	b := *r.cur
	r.cur = nil
	return b, true
}

// Resample converts sorted 1-minute bars of one instrument to tf.
func Resample(bars []Bar, s Session, tf Timeframe) ([]Bar, error) {
	r, err := NewResampler(s, tf)
	if err != nil {
		return nil, err
	}
	var out []Bar
	for _, b := range bars {
		done, err := r.Add(b)
		if err != nil {
			return nil, err
		}
		out = append(out, done...)
	}
	if len(bars) > 0 {
		if b, ok := r.FlushUntil(bars[len(bars)-1].Time.Add(366 * 24 * time.Hour)); ok {
			out = append(out, b)
		}
	}
	return out, nil
}
