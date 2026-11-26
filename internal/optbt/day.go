package optbt

import (
	"context"
	"sort"
	"sync"

	"serene/internal/data"
	"serene/internal/market"
)

// slots covers 09:15 to 15:55, one per minute, which holds both the 15:30
// and the later 15:40 close.
const slots = 400

type series struct {
	o, h, l, c [slots]market.Paise
	has        [slots]bool
}

func (s *series) bar(slot int) (o, h, l, c market.Paise, ok bool) {
	if s == nil || !s.has[slot] {
		return 0, 0, 0, 0, false
	}
	return s.o[slot], s.h[slot], s.l[slot], s.c[slot], true
}

type optKey struct {
	exp    market.Date
	strike market.Paise
	right  market.Right
}

type opt struct {
	inst market.Instrument
	key  optKey
	s    *series
}

// Day is one trading day of an index and its options.
type Day struct {
	Date     market.Date
	Ord      int // position in the stored trading days
	DTE      int // trading days to the nearest expiry; -2 when that expiry is not a stored day
	Expiries []market.Date
	Index    *series
	Close    int   // first slot after the last index bar
	Events   uint8 // event kinds on this day (see events.go)
	opts     map[optKey]*opt
	info     indexInfo

	mu     sync.RWMutex
	picks  map[pickKey]*opt
	hedges map[hedgeKey]*opt
}

// nearest returns the nearest expiry on or after the day (after it, for
// carrying overnight).
func (d *Day) nearest(after bool) market.Date {
	for _, e := range d.Expiries {
		if after && !d.Date.Before(e) {
			continue
		}
		return e
	}
	return market.Date{}
}

func newDay(date market.Date, info indexInfo) *Day {
	return &Day{Date: date, info: info, Index: &series{}, opts: map[optKey]*opt{},
		picks: map[pickKey]*opt{}, hedges: map[hedgeKey]*opt{}}
}

// put adds one 1m bar; inst is the zero value for the index.
func (d *Day) put(inst market.Instrument, slot int, b market.Bar) {
	if slot < 0 || slot >= slots {
		return
	}
	s := d.Index
	if inst.Kind == market.KindOption {
		k := optKey{inst.Expiry, inst.Strike, inst.Right}
		o := d.opts[k]
		if o == nil {
			o = &opt{inst: inst, key: k, s: &series{}}
			d.opts[k] = o
		}
		s = o.s
	} else if slot+1 > d.Close {
		d.Close = slot + 1
	}
	s.o[slot], s.h[slot], s.l[slot], s.c[slot], s.has[slot] = b.Open, b.High, b.Low, b.Close, true
}

// finish sets the expiry list and DTE once all bars are in.
func (d *Day) finish(ords map[market.Date]int) {
	seen := map[market.Date]bool{}
	for k := range d.opts {
		if !k.exp.Before(d.Date) && !seen[k.exp] {
			seen[k.exp] = true
			d.Expiries = append(d.Expiries, k.exp)
		}
	}
	sort.Slice(d.Expiries, func(i, j int) bool { return d.Expiries[i].Before(d.Expiries[j]) })
	d.DTE = -2
	if len(d.Expiries) > 0 {
		if o, ok := ords[d.Expiries[0]]; ok {
			d.DTE = o - d.Ord
		}
	}
}

func loadDay(ctx context.Context, store *data.Store, name string, info indexInfo, date market.Date,
	lots map[string]market.Instrument, ords map[market.Date]int) (*Day, error) {
	bars, err := store.DayBars(ctx, name, date)
	if err != nil {
		return nil, err
	}
	d := newDay(date, info)
	d.Ord = ords[date]
	open := date.At(9, 15)
	for _, b := range bars {
		if market.DateOf(b.Time) != date {
			continue
		}
		slot := int(b.Time.Sub(open).Minutes())
		if b.Key == info.IndexKey {
			d.put(market.Instrument{}, slot, b)
			continue
		}
		in, ok := lots[b.Key]
		if !ok || in.Kind != market.KindOption {
			continue
		}
		d.put(in, slot, b)
	}
	d.finish(ords)
	return d, nil
}
