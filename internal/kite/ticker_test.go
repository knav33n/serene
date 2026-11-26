package kite

import (
	"encoding/binary"
	"testing"
	"time"
)

func TestParseTicks(t *testing.T) {
	full := make([]byte, tickFull)
	put := func(at int, v int32) { binary.BigEndian.PutUint32(full[at:], uint32(v)) }
	put(0, 12345)
	put(4, 26995) // ₹269.95
	put(8, 65)
	put(16, 1000)
	put(44, 1764131400)
	put(48, 777)
	put(60, 1764131401)
	put(64, 130) // best bid qty
	put(68, 26990)
	put(64+60, 195) // best ask qty
	put(68+60, 27000)
	msg := []byte{0, 3}
	for _, p := range [][]byte{full, make([]byte, 32), full[:tickLTP]} { // full, an index packet, ltp
		msg = binary.BigEndian.AppendUint16(msg, uint16(len(p)))
		msg = append(msg, p...)
	}
	ts, err := ParseTicks(msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 {
		t.Fatalf("want 2 ticks, got %+v", ts)
	}
	a := ts[0]
	if a.Token != 12345 || a.LTP != 269.95 || a.LastQty != 65 || a.Volume != 1000 || a.OI != 777 ||
		a.Bid != 269.90 || a.BidQty != 130 || a.Ask != 270 || a.AskQty != 195 ||
		!a.Exchange.Equal(time.Unix(1764131401, 0)) || !a.LastTrade.Equal(time.Unix(1764131400, 0)) {
		t.Errorf("full tick %+v", a)
	}
	if b := ts[1]; b.Token != 12345 || b.LTP != 269.95 || b.Volume != 0 {
		t.Errorf("ltp tick %+v", b)
	}
	if ts, err := ParseTicks([]byte{0}); err != nil || ts != nil {
		t.Errorf("heartbeat: %v %v", ts, err)
	}
	if _, err := ParseTicks([]byte{0, 1, 0, 184, 1}); err == nil {
		t.Error("a cut packet parsed")
	}
}
