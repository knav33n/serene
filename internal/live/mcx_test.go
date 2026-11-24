package live

import (
	"context"
	"strings"
	"testing"
	"time"

	"serene/internal/market"
)

const (
	dec = "MCX:GOLDM:2026-12-04:FUT"
	jan = "MCX:GOLDM:2027-01-05:FUT"
)

func mcxCfg() MCXConfig { return MCXConfig{Lots: 1, Buffer: 0.003, MaxMods: 3, TagCode: "gh"} }

func tgt(side market.Side, key string, last float64) MCXTarget {
	return MCXTarget{Side: side, Key: key, Tick: market.Rupees(1),
		Last: map[string]market.Paise{dec: market.Rupees(last), jan: market.Rupees(last + 500)}}
}

var t0 = time.Date(2026, 11, 20, 10, 1, 20, 0, market.IST)

func mstep(t *testing.T, f *fake, st *MCXState, tg MCXTarget, c MCXConfig, now time.Time) []MCXEvent {
	t.Helper()
	evs, err := StepMCX(context.Background(), f, st, tg, c, now)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestMCXFlatTargetPlacesNothing(t *testing.T) {
	f := newFake(0)
	st := &MCXState{Strategy: "goldm_hmasma_10m"}
	mstep(t, f, st, tgt(0, dec, 130000), mcxCfg(), t0)
	if len(f.log) != 0 || st.Order != nil {
		t.Fatalf("placed %v", f.log)
	}
}

func TestMCXEnterThenReverseIsCloseThenOpen(t *testing.T) {
	f := newFake(0)
	st := &MCXState{Strategy: "goldm_hmasma_10m"}
	c := mcxCfg()
	mstep(t, f, st, tgt(market.Sell, dec, 130000), c, t0)
	// Sell 0.3% under 130000 = 129610.
	if f.last() != "place 1 SELL LIMIT 1 @129610/0" {
		t.Fatal(f.last())
	}
	f.fill("1", 1, 129990)
	evs := mstep(t, f, st, tgt(market.Sell, dec, 130000), c, t0.Add(time.Minute))
	if len(evs) != 1 || evs[0].Fill == nil || st.Qty != -1 || st.Key != dec || st.Entry != market.Rupees(129990) {
		t.Fatalf("after fill %+v %+v", st, evs)
	}
	// Nothing more while the side holds.
	mstep(t, f, st, tgt(market.Sell, dec, 129000), c, t0.Add(2*time.Minute))
	if len(f.log) != 1 {
		t.Fatal(f.log)
	}
	// Turn up: one 1-lot buy to close, not a 2-lot reversal.
	mstep(t, f, st, tgt(market.Buy, dec, 128000), c, t0.Add(3*time.Minute))
	if f.last() != "place 2 BUY LIMIT 1 @128384/0" {
		t.Fatal(f.last())
	}
	f.fill("2", 1, 128010)
	evs = mstep(t, f, st, tgt(market.Buy, dec, 128000), c, t0.Add(4*time.Minute))
	if len(evs) != 1 || !evs[0].Fill.Close || evs[0].Fill.Entry != market.Rupees(129990) || st.Qty != 0 {
		t.Fatalf("close %+v %+v", st, evs)
	}
	mstep(t, f, st, tgt(market.Buy, dec, 128000), c, t0.Add(5*time.Minute))
	if f.last() != "place 3 BUY LIMIT 1 @128384/0" {
		t.Fatal(f.last())
	}
}

func TestMCXRollClosesAndReopensSameSide(t *testing.T) {
	f := newFake(0)
	f.held[dec] = -1
	st := &MCXState{Strategy: "goldm_hmasma_10m", Key: dec, Qty: -1, Entry: market.Rupees(131000)}
	c := mcxCfg()
	mstep(t, f, st, tgt(market.Sell, jan, 125000), c, t0)
	if f.last() != "place 1 BUY LIMIT 1 @125375/0" || !strings.HasPrefix(st.Order.Why, "roll") {
		t.Fatal(f.log, st.Order)
	}
	f.fill("1", 1, 125010)
	mstep(t, f, st, tgt(market.Sell, jan, 125000), c, t0.Add(time.Minute))
	mstep(t, f, st, tgt(market.Sell, jan, 125000), c, t0.Add(2*time.Minute))
	if f.last() != "place 2 SELL LIMIT 1 @125123/0" || f.reqs["2"].Instrument.Key() != jan {
		t.Fatal(f.log)
	}
}

func TestMCXBrokerHoldingLessHalts(t *testing.T) {
	f := newFake(0) // holds nothing
	st := &MCXState{Strategy: "goldm_hmasma_10m", Key: dec, Qty: -1}
	evs := mstep(t, f, st, tgt(market.Buy, dec, 125000), mcxCfg(), t0)
	if st.Halted == "" || len(evs) != 1 || len(f.log) != 0 {
		t.Fatal(st, f.log)
	}
	mstep(t, f, st, tgt(market.Buy, dec, 125000), mcxCfg(), t0.Add(time.Minute))
	if len(f.log) != 0 {
		t.Fatal("placed while halted", f.log)
	}
}

func TestMCXRepricesThenHalts(t *testing.T) {
	f := newFake(0)
	c := mcxCfg()
	st := &MCXState{Strategy: "goldm_hmasma_10m"}
	mstep(t, f, st, tgt(market.Buy, dec, 125000), c, t0)
	for i := 1; i <= 3; i++ {
		mstep(t, f, st, tgt(market.Buy, dec, 125000+float64(i)*100), c, t0.Add(time.Duration(i)*time.Minute))
	}
	if st.Order.Mods != 3 || !strings.HasPrefix(f.last(), "modify 1") {
		t.Fatal(f.log)
	}
	mstep(t, f, st, tgt(market.Buy, dec, 125500), c, t0.Add(5*time.Minute))
	if st.Halted == "" {
		t.Fatal("not halted")
	}
}

func TestMCXRejectedHalts(t *testing.T) {
	f := newFake(0)
	st := &MCXState{Strategy: "goldm_hmasma_10m"}
	mstep(t, f, st, tgt(market.Buy, dec, 125000), mcxCfg(), t0)
	f.orders["1"].Status = Rejected
	f.orders["1"].Message = "insufficient funds"
	mstep(t, f, st, tgt(market.Buy, dec, 125000), mcxCfg(), t0.Add(time.Minute))
	if !strings.Contains(st.Halted, "insufficient funds") || st.Qty != 0 {
		t.Fatal(st)
	}
}

func TestMCXFailedPlaceIsFoundByTag(t *testing.T) {
	f := newFake(0)
	f.failPlace = true
	st := &MCXState{Strategy: "goldm_hmasma_10m"}
	if _, err := StepMCX(context.Background(), f, st, tgt(market.Buy, dec, 125000), mcxCfg(), t0); err == nil {
		t.Fatal("want error")
	}
	f.fill("1", 1, 125100)
	mstep(t, f, st, tgt(market.Buy, dec, 125000), mcxCfg(), t0.Add(time.Minute))
	if st.Qty != 1 || len(f.log) != 1 {
		t.Fatal(st, f.log)
	}
}

func TestMCXLostPlaceIsPlacedAgain(t *testing.T) {
	f := newFake(0)
	f.lostPlace = true
	st := &MCXState{Strategy: "goldm_hmasma_10m"}
	StepMCX(context.Background(), f, st, tgt(market.Buy, dec, 125000), mcxCfg(), t0)
	mstep(t, f, st, tgt(market.Buy, dec, 125000), mcxCfg(), t0.Add(3*time.Minute)) // forgets it
	mstep(t, f, st, tgt(market.Buy, dec, 125000), mcxCfg(), t0.Add(4*time.Minute)) // places again
	if len(f.log) != 1 || !strings.HasPrefix(f.last(), "place 1 BUY") {
		t.Fatal(f.log)
	}
}
