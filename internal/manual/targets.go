package manual

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"serene/internal/market"
)

// Tracked is one manual buy's target, watched by the desk: when the LTP
// reaches Price, Qty is sold at market. Nothing rests at the broker.
type Tracked struct {
	ID       string  `json:"id"`
	Day      string  `json:"day"` // the trading day it's for, YYYY-MM-DD
	Broker   string  `json:"broker"`
	Symbol   string  `json:"symbol"`
	Exchange string  `json:"exchange"`
	Product  string  `json:"product"`
	Expiry   string  `json:"expiry"`
	Strike   float64 `json:"strike"`
	Right    string  `json:"right"`
	Qty      int64   `json:"qty"`
	Entry    float64 `json:"entry"`
	Price    float64 `json:"price"` // the target
}

// Targets are the targets being tracked, kept in a JSON file (when Path is
// set) so a restart keeps watching them.
type Targets struct {
	Path string

	mu   sync.Mutex
	list []Tracked
	read bool
	n    int64
}

func (t *Targets) load() {
	if t.read {
		return
	}
	t.read = true
	if t.Path == "" {
		return
	}
	b, err := os.ReadFile(t.Path)
	if err != nil {
		return
	}
	json.Unmarshal(b, &t.list)
}

func (t *Targets) save() error {
	if t.Path == "" {
		return nil
	}
	b, err := json.MarshalIndent(t.list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(t.Path), 0o755); err != nil {
		return err
	}
	tmp := t.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, t.Path)
}

// Add tracks x, giving it an ID.
func (t *Targets) Add(x Tracked) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.load()
	t.n++
	x.ID = fmt.Sprintf("%d-%d", time.Now().UnixNano(), t.n)
	t.list = append(t.list, x)
	return t.save()
}

// List is what's tracked for day (all days with "").
func (t *Targets) List(day string) []Tracked {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.load()
	var out []Tracked
	for _, x := range t.list {
		if day == "" || x.Day == day {
			out = append(out, x)
		}
	}
	return out
}

// Drop stops tracking the targets drop picks.
func (t *Targets) Drop(drop func(Tracked) bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.load()
	var kept []Tracked
	for _, x := range t.list {
		if !drop(x) {
			kept = append(kept, x)
		}
	}
	if len(kept) == len(t.list) {
		return nil
	}
	t.list = kept
	return t.save()
}

func (d *Desk) targets() *Targets {
	if d.Targets == nil {
		d.Targets = &Targets{}
	}
	return d.Targets
}

// untrack stops watching the broker's targets on symbol: it's been exited.
func (d *Desk) untrack(broker, symbol string) {
	d.targets().Drop(func(x Tracked) bool { return x.Broker == broker && x.Symbol == symbol })
}

// Watch checks each target tracked today against its LTP and sells, at
// market, the ones reached. Targets from earlier days are dropped (the 15:20
// square-off, or the broker's, has exited them). It's called every few
// seconds while the page runs.
func (d *Desk) Watch(ctx context.Context) error {
	now := d.now()
	today := market.DateOf(now).String()
	d.targets().Drop(func(x Tracked) bool { return x.Day != today })
	if before(now, OpenAt) || !before(now, CloseAt) {
		return nil
	}
	var errs []error
	ltps := map[string]float64{}
	for _, x := range d.targets().List(today) {
		key := x.Expiry + fmt.Sprint(x.Strike) + x.Right
		ltp, ok := ltps[key]
		if !ok {
			c, err := d.contract(ctx, x.Expiry, x.Strike, x.Right)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if ltp, err = d.LTP(ctx, c); err != nil {
				errs = append(errs, fmt.Errorf("LTP of %s: %w", Label(c), err))
				continue
			}
			ltps[key] = ltp
		}
		if ltp <= 0 || ltp < x.Price {
			continue
		}
		if err := d.hit(ctx, x, ltp); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// hit places x's target: a LIMIT sell at its price for its quantity (at most
// what's held), split at the freeze quantity. The LTP has reached the price,
// so it should fill there or better; what doesn't rests until it does, Exit,
// or the 15:20 square-off (which cancels it first).
func (d *Desk) hit(ctx context.Context, x Tracked, ltp float64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	still := false
	for _, y := range d.targets().List(x.Day) { // an Exit may have taken it meanwhile
		still = still || y.ID == x.ID
	}
	if !still {
		return nil
	}
	fail := func(err error) error { return fmt.Errorf("target %s on %s: %w", x.Symbol, Names[x.Broker], err) }
	b, err := d.Broker(ctx, x.Broker)
	if err != nil {
		return fail(err)
	}
	ps, err := b.Positions(ctx)
	if err != nil {
		return fail(fmt.Errorf("positions: %w", err))
	}
	var held int64
	for _, p := range ps {
		if p.Symbol == x.Symbol && p.Exchange == x.Exchange && p.Product == x.Product {
			held += p.Qty
		}
	}
	if held <= 0 {
		d.targets().Drop(func(y Tracked) bool { return y.ID == x.ID })
		d.say(ctx, "manual: target %.2f on %s (%s) reached, but nothing's held: stopped watching it", x.Price, x.Symbol, Names[x.Broker])
		return nil
	}
	qty := min(x.Qty, held)
	lot, freeze := int64(1), int64(0)
	if c, err := d.contract(ctx, x.Expiry, x.Strike, x.Right); err == nil {
		lot, freeze = c.LotSize, c.FreezeQty
	}
	var placed int64
	var placeErr error
	for _, q := range OrderSizes(qty, lot, freeze) {
		if _, err := b.Place(ctx, OrderReq{Symbol: x.Symbol, Exchange: x.Exchange, Product: x.Product, Side: "SELL", Type: "LIMIT", Qty: q, Price: x.Price}); err != nil {
			placeErr = err
			break
		}
		placed += q
	}
	if placed > 0 {
		d.targets().Drop(func(y Tracked) bool { return y.ID == x.ID })
		if placeErr != nil && qty > placed { // keep watching what didn't go in
			x.Qty = qty - placed
			d.targets().Add(x)
		}
		d.say(ctx, "manual: :dart: target %.2f reached (LTP %.2f): LIMIT sell %d %s on %s @ %.2f placed",
			x.Price, ltp, placed, x.Symbol, Names[x.Broker], x.Price)
	}
	if placeErr != nil {
		return fail(placeErr)
	}
	return nil
}
