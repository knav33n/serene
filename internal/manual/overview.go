package manual

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"

	"serene/internal/market"
)

// Brokers are the accounts the page shows, in its order.
var Brokers = []string{"zerodha", "upstox", "fyers", "icici"}

// Free is a broker's free margin, or why it couldn't be read.
type Free struct {
	Free  *float64 `json:"free"`
	Error string   `json:"error,omitempty"`
}

// Held is a broker's open F&O positions, or why they couldn't be read.
type Held struct {
	Rows  []Position `json:"rows"`
	Error string     `json:"error,omitempty"`
}

// DayRow is one F&O position traded today or held, with today's orders in it.
type DayRow struct {
	Broker string `json:"broker"`
	Position
	Orders  []Fill   `json:"orders"`
	Charges *float64 `json:"charges"`
	Net     *float64 `json:"net"`
	By      string   `json:"by"`               // the serene strategy its orders are for, else their tag
	Manual  bool     `json:"manual"`           // the desk's
	Target  *float64 `json:"target,omitempty"` // a manual trade's open target
}

// Total is a broker's day.
type Total struct {
	PnL     float64  `json:"pnl"`
	Charges *float64 `json:"charges"`
	Net     *float64 `json:"net"`
	Orders  int      `json:"orders"`
	Priced  int      `json:"priced"`
	Note    string   `json:"note,omitempty"`
}

// Day is today on both brokers.
type Day struct {
	Rows   []DayRow          `json:"rows"`
	Totals map[string]Total  `json:"totals"`
	Errors map[string]string `json:"errors"`
}

// View is the page.
type View struct {
	Expiries  []string        `json:"expiries"`
	Free      map[string]Free `json:"free"`
	Positions map[string]Held `json:"positions"`
	Trades    Day             `json:"trades"`
}

// strategy names an order by its tag.
func strategy(tag string) string {
	switch {
	case tag == Tag:
		return "manual"
	case strings.HasPrefix(tag, "hmr"):
		return "nifty_hma_retest"
	}
	return tag
}

type book struct {
	free   Free
	held   Held
	rows   []DayRow
	total  *Total
	errMsg string
}

func (d *Desk) read(ctx context.Context, name string) book {
	var bk book
	b, err := d.Broker(ctx, name)
	if err != nil {
		msg := err.Error()
		return book{free: Free{Error: msg}, held: Held{Rows: []Position{}, Error: msg}, errMsg: msg}
	}
	if f, err := b.Funds(ctx); err != nil {
		bk.free.Error = err.Error()
	} else {
		bk.free.Free = &f
	}
	ps, err := b.Positions(ctx)
	if err != nil {
		msg := err.Error()
		bk.held, bk.errMsg = Held{Rows: []Position{}, Error: msg}, msg
		return bk
	}
	bk.held.Rows = []Position{}
	for _, p := range ps {
		if p.Qty != 0 {
			bk.held.Rows = append(bk.held.Rows, p)
		}
	}
	var notes []string
	fills, err := b.Fills(ctx)
	if err != nil {
		notes = append(notes, "orders unread: "+err.Error())
		fills = nil
	}
	orders, err := b.Orders(ctx)
	if err != nil {
		notes = append(notes, "order tags unread: "+err.Error())
	}
	tags := map[string]string{}
	targets := map[string]float64{} // the lowest, when a contract has several
	for _, o := range orders {
		tags[o.ID] = o.Tag
		if t, ok := targets[o.Symbol]; o.Tag == Tag && o.Open && o.Side == "SELL" && (!ok || o.Price < t) {
			targets[o.Symbol] = o.Price // reached and placed, not filled yet
		}
	}
	for _, x := range d.targets().List(market.DateOf(d.now()).String()) {
		if t, ok := targets[x.Symbol]; x.Broker == name && (!ok || x.Price < t) {
			targets[x.Symbol] = x.Price
		}
	}
	charges := map[string]float64{}
	if len(fills) > 0 {
		if charges, err = b.Charges(ctx, fills); err != nil {
			notes = append(notes, "charges unread: "+err.Error())
			charges = map[string]float64{}
		}
	}
	type key struct{ sym, product string }
	rows := map[key]*DayRow{}
	var keys []key
	for _, p := range ps {
		k := key{p.Symbol, p.Product}
		if rows[k] == nil {
			keys = append(keys, k)
			rows[k] = &DayRow{Broker: name, Position: p, Orders: []Fill{}}
		}
	}
	for _, f := range fills {
		if c, ok := charges[f.OrderID]; ok {
			c = round2(c)
			f.Charges = &c
		}
		k := key{f.Symbol, f.Product}
		r := rows[k]
		if r == nil { // an order the positions don't list: a row of its own
			keys = append(keys, k)
			r = &DayRow{Broker: name, Position: Position{Symbol: f.Symbol, Name: f.Symbol, Exchange: f.Exchange, Product: f.Product}, Orders: []Fill{}}
			rows[k] = r
		}
		r.Orders = append(r.Orders, f)
		if t := tags[f.OrderID]; t != "" && r.By == "" {
			r.By = strategy(t)
		}
	}
	total := Total{Note: strings.Join(notes, "; ")}
	var totalCharges float64
	for _, k := range keys {
		r := rows[k]
		r.Manual = r.By == "manual"
		if t, ok := targets[r.Symbol]; ok && r.Manual {
			r.Target = &t
		}
		priced := fills != nil
		var sum float64
		for _, f := range r.Orders {
			total.Orders++
			if f.Charges == nil {
				priced = false
				continue
			}
			total.Priced++
			sum += *f.Charges
			totalCharges += *f.Charges
		}
		if priced {
			c := round2(sum)
			n := round2(r.PnL - c)
			r.Charges, r.Net = &c, &n
		}
		total.PnL += r.PnL
		bk.rows = append(bk.rows, *r)
	}
	total.PnL = round2(total.PnL)
	if fills != nil {
		c := round2(totalCharges)
		total.Charges = &c
		if total.Priced == total.Orders {
			n := round2(total.PnL - c)
			total.Net = &n
		}
	}
	bk.total = &total
	return bk
}

// Overview is the page's view: Nifty's expiries, each broker's free margin,
// open F&O positions and today's trades.
func (d *Desk) Overview(ctx context.Context) (View, error) {
	cs, err := d.Contracts(ctx)
	if err != nil {
		return View{}, err
	}
	v := View{Expiries: []string{}, Free: map[string]Free{}, Positions: map[string]Held{},
		Trades: Day{Rows: []DayRow{}, Totals: map[string]Total{}, Errors: map[string]string{}}}
	for _, e := range Expiries(cs, d.now()) {
		v.Expiries = append(v.Expiries, e.String())
	}
	books := make([]book, len(Brokers))
	var wg sync.WaitGroup
	for i, name := range Brokers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			books[i] = d.read(ctx, name)
		}()
	}
	wg.Wait()
	for i, name := range Brokers {
		bk := books[i]
		v.Free[name], v.Positions[name] = bk.free, bk.held
		v.Trades.Rows = append(v.Trades.Rows, bk.rows...)
		if bk.total != nil {
			v.Trades.Totals[name] = *bk.total
		}
		if bk.errMsg != "" {
			v.Trades.Errors[name] = bk.errMsg
		}
	}
	latest := func(r DayRow) string {
		t := ""
		for _, o := range r.Orders {
			t = max(t, o.Time)
		}
		return t
	}
	sort.SliceStable(v.Trades.Rows, func(i, j int) bool { return latest(v.Trades.Rows[i]) > latest(v.Trades.Rows[j]) })
	return v, nil
}

// ByOrder sums a tradebook's fills into one row per order: the quantities
// summed, the price their average, the first fill's time.
func ByOrder(fills []Fill) []Fill {
	idx := map[string]int{}
	var out []Fill
	value := map[string]float64{}
	for _, f := range fills {
		i, ok := idx[f.OrderID]
		value[f.OrderID] += float64(f.Qty) * f.Price
		if !ok {
			idx[f.OrderID] = len(out)
			out = append(out, f)
			continue
		}
		out[i].Qty += f.Qty
		if f.Time != "" && (out[i].Time == "" || f.Time < out[i].Time) {
			out[i].Time = f.Time
		}
	}
	for i := range out {
		if out[i].Qty > 0 {
			out[i].Price = math.Round(value[out[i].OrderID]/float64(out[i].Qty)*100) / 100
		}
	}
	return out
}
