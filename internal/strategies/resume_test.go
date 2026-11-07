package strategies

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"calm/internal/cost"
	"calm/internal/data"
	"calm/internal/engine"
	"calm/internal/margin"
	"calm/internal/market"
	"calm/internal/sim"
	"calm/internal/strategy"
	"calm/internal/synth"
)

func straddleRun(t *testing.T, store *data.Store, from, to market.Date, resume *engine.Resume) (engine.Result, error) {
	r, _, err := straddleRunWithBroker(t, store, from, to, resume)
	return r, err
}

func straddleRunWithBroker(t *testing.T, store *data.Store, from, to market.Date, resume *engine.Resume) (engine.Result, *sim.Broker, error) {
	t.Helper()
	ctx := context.Background()
	contracts, err := store.Contracts(ctx, "NIFTY")
	if err != nil {
		t.Fatal(err)
	}
	feed, err := engine.NewStoreFeed(store, []string{"NIFTY"}, from, to)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New("short_straddle", "NIFTY", strategy.Params{"stop_pct": 15})
	if err != nil {
		t.Fatal(err)
	}
	b, err := sim.New(sim.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	costs, err := cost.Load("../../configs/costs.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg := engine.Config{Contracts: contracts, Session: market.NSESession, Costs: costs,
		Margin: margin.Model{NakedPct: 12}, Capital: market.Rupees(1e6), Resume: resume}
	r, err := engine.New(cfg, feed, b, s).Run(ctx)
	return r, b, err
}

func journalUpTo(r engine.Result, at time.Time) *engine.Resume {
	j := &engine.Resume{At: at}
	for _, o := range r.Orders {
		if !o.Placed.After(at) {
			j.Orders = append(j.Orders, o)
		}
	}
	for _, f := range r.Fills {
		if f.Time.Before(at) {
			j.Fills = append(j.Fills, f)
		}
	}
	return j
}

// A restart between a straddle's entry and its premium stop must give the run
// an uninterrupted one would have. Without the replay, the strategy forgets the
// credit it took, so its stop can never fire, and it sells again once flat.
func TestStraddleResumesThroughItsStop(t *testing.T) {
	store, err := data.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	from, _ := market.ParseDate("2026-10-05")
	to := from.AddDays(20)
	cfg := synth.Default(from, to, 11)
	cfg.StrikesEachSide = 4
	cfg.Expiries = 1
	if _, err := synth.Generate(context.Background(), store, cfg); err != nil {
		t.Fatal(err)
	}
	full, err := straddleRun(t, store, from, to, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Find a day whose cover came from the stop, before the 15:15 exit.
	var entry, cover market.Order
	for i := 1; i < len(full.Orders); i++ {
		o := full.Orders[i]
		exitAt := market.NSESession.Open(market.DateOf(o.Placed)).Add(360 * time.Minute)
		if o.Legs[0].Side == market.Buy && o.Placed.Before(exitAt) && full.Orders[i-1].Tag == o.Tag {
			entry, cover = full.Orders[i-1], o
			break
		}
	}
	if cover.ID == 0 {
		t.Fatal("no stopped-out day in the data; change the seed or stop")
	}
	gap := cover.Placed.Sub(entry.Placed)
	if gap < 5*time.Minute {
		t.Fatalf("entry and stop only %s apart", gap)
	}
	at := entry.Placed.Add(gap / 2).Truncate(time.Minute)

	resumed, b, err := straddleRunWithBroker(t, store, from, to, journalUpTo(full, at))
	if err != nil {
		t.Fatal(err)
	}
	// Only orders placed after the restart may reach the broker.
	sent, _ := b.Orders(context.Background())
	var want []int64
	for _, o := range full.Orders {
		if o.Placed.After(at) {
			want = append(want, o.ID)
		}
	}
	var got []int64
	for _, o := range sent {
		got = append(got, o.ID)
	}
	if len(want) == 0 || len(want) == len(full.Orders) || !reflect.DeepEqual(got, want) {
		t.Fatalf("broker got orders %v, want %v: those placed after %s", got, want, at.In(market.IST))
	}
	if !reflect.DeepEqual(resumed.Orders, full.Orders) {
		t.Fatalf("orders differ after resuming at %s", at.In(market.IST))
	}
	if !reflect.DeepEqual(resumed.Fills, full.Fills) || !reflect.DeepEqual(resumed.Equity, full.Equity) {
		t.Fatalf("fills or equity differ after resuming at %s", at.In(market.IST))
	}

	t.Run("a journal the strategy disagrees with is refused", func(t *testing.T) {
		j := journalUpTo(full, at)
		j.Orders[len(j.Orders)-1].Tag = "someone-else"
		if _, err := straddleRun(t, store, from, to, j); err == nil || !strings.Contains(err.Error(), "journal has") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("an order open at the restart is refused", func(t *testing.T) {
		// The entry is placed at its decision bar's close and fills in the
		// next minute, so resuming at its placement leaves it open.
		_, err := straddleRun(t, store, from, to, journalUpTo(full, entry.Placed))
		if err == nil || !strings.Contains(err.Error(), "reconcile it with the broker") {
			t.Fatalf("err = %v", err)
		}
	})
}
