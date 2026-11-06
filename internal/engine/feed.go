package engine

import (
	"context"
	"io"
	"sort"
	"time"

	"calm/internal/data"
	"calm/internal/market"
)

// Batch is every 1-minute candle starting at Time.
type Batch struct {
	Time time.Time
	Bars []market.Bar
}

// Feed supplies minutes in time order and returns io.EOF when finished.
type Feed interface {
	Next(ctx context.Context) (Batch, error)
}

// batches groups bars sorted by time into minutes.
func batches(bars []market.Bar) []Batch {
	var out []Batch
	for i := 0; i < len(bars); {
		j := i
		for j < len(bars) && bars[j].Time.Equal(bars[i].Time) {
			j++
		}
		out = append(out, Batch{Time: bars[i].Time, Bars: bars[i:j]})
		i = j
	}
	return out
}

// SliceFeed replays bars held in memory.
type SliceFeed struct{ queue []Batch }

// NewSliceFeed sorts bars and returns a feed over them.
func NewSliceFeed(bars []market.Bar) *SliceFeed {
	sorted := append([]market.Bar(nil), bars...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].Time.Equal(sorted[j].Time) {
			return sorted[i].Time.Before(sorted[j].Time)
		}
		return sorted[i].Key < sorted[j].Key
	})
	return &SliceFeed{queue: batches(sorted)}
}

// Next returns the next minute.
func (f *SliceFeed) Next(context.Context) (Batch, error) {
	if len(f.queue) == 0 {
		return Batch{}, io.EOF
	}
	b := f.queue[0]
	f.queue = f.queue[1:]
	return b, nil
}

// StoreFeed replays stored data one day at a time.
type StoreFeed struct {
	store       *data.Store
	underlyings []string
	days        map[market.Date][]string
	order       []market.Date
	queue       []Batch
}

// NewStoreFeed returns a feed over [from, to] for the underlyings.
func NewStoreFeed(s *data.Store, underlyings []string, from, to market.Date) (*StoreFeed, error) {
	f := &StoreFeed{store: s, underlyings: underlyings, days: map[market.Date][]string{}}
	for _, u := range underlyings {
		days, err := s.Days(u, from, to)
		if err != nil {
			return nil, err
		}
		for _, d := range days {
			if len(f.days[d]) == 0 {
				f.order = append(f.order, d)
			}
			f.days[d] = append(f.days[d], u)
		}
	}
	sort.Slice(f.order, func(i, j int) bool { return f.order[i].Before(f.order[j]) })
	return f, nil
}

// TradingDays returns the days the feed will replay.
func (f *StoreFeed) TradingDays() []market.Date { return f.order }

// Next returns the next minute.
func (f *StoreFeed) Next(ctx context.Context) (Batch, error) {
	for len(f.queue) == 0 {
		if len(f.order) == 0 {
			return Batch{}, io.EOF
		}
		d := f.order[0]
		f.order = f.order[1:]
		var bars []market.Bar
		for _, u := range f.days[d] {
			b, err := f.store.DayBars(ctx, u, d)
			if err != nil {
				return Batch{}, err
			}
			bars = append(bars, b...)
		}
		sf := NewSliceFeed(bars)
		f.queue = sf.queue
	}
	b := f.queue[0]
	f.queue = f.queue[1:]
	return b, nil
}
