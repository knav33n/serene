package market

import (
	"testing"
	"time"
)

func minuteBar(key string, t time.Time, o, h, l, c Paise, v, oi int64) Bar {
	return Bar{Key: key, Timeframe: Minute1, Time: t, End: t.Add(time.Minute),
		Open: o, High: h, Low: l, Close: c, Volume: v, OI: oi}
}

func TestKeyRoundTrip(t *testing.T) {
	exp, _ := ParseDate("2026-11-09")
	cases := []Instrument{
		Index(NSE, "NIFTY"),
		Option(NFO, "NIFTY", exp, Rupees(24000), Call, 0),
		Option(NFO, "NIFTY", exp, Rupees(24000.5), Put, 0),
		{Exchange: NFO, Kind: KindFuture, Underlying: "NIFTY", Expiry: exp},
	}
	for _, c := range cases {
		got, err := ParseKey(c.Key())
		if err != nil {
			t.Fatalf("%s: %v", c.Key(), err)
		}
		if c.Kind == KindIndex {
			c.LotSize = 1
		}
		if got != c {
			t.Errorf("round trip %s: got %+v want %+v", c.Key(), got, c)
		}
	}
	for _, bad := range []string{"", "NFO", "NFO:NIFTY:2026-11-09:24000:XX", "NFO:NIFTY:bad:24000:CE"} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) should fail", bad)
		}
	}
}

func TestPaiseString(t *testing.T) {
	for p, want := range map[Paise]string{2400000: "24000", 2400050: "24000.50", -105: "-1.05", 5: "0.05"} {
		if got := p.String(); got != want {
			t.Errorf("%d: got %q want %q", int64(p), got, want)
		}
	}
}

func TestCloseMovesOnItsDate(t *testing.T) {
	before, _ := ParseDate("2026-07-31")
	after, _ := ParseDate("2026-08-03")
	if !NSESession.Close(before).Equal(before.At(15, 30)) || !NSESession.Close(after).Equal(after.At(15, 40)) {
		t.Fatal("wrong close")
	}
	if NSESession.Contains(before.At(15, 35)) || !NSESession.Contains(after.At(15, 35)) {
		t.Error("15:35 should be in session only after the change")
	}
	if _, end := NSESession.Bucket(after.At(15, 20), Hour1); !end.Equal(after.At(15, 40)) {
		t.Errorf("last hourly bar should run to the new close, got %s", end)
	}
}

func TestBucketAlignsToSessionOpen(t *testing.T) {
	d, _ := ParseDate("2026-07-15")
	cases := []struct {
		at         time.Time
		tf         Timeframe
		start, end time.Time
	}{
		{d.At(9, 15), Minute5, d.At(9, 15), d.At(9, 20)},
		{d.At(9, 19), Minute5, d.At(9, 15), d.At(9, 20)},
		{d.At(9, 20), Minute5, d.At(9, 20), d.At(9, 25)},
		// Hourly bars run 09:15-10:15, not 09:00-10:00.
		{d.At(10, 14), Hour1, d.At(9, 15), d.At(10, 15)},
		// The last hourly bar is cut at the close.
		{d.At(15, 20), Hour1, d.At(15, 15), d.At(15, 30)},
		{d.At(12, 0), Daily, d.At(9, 15), d.At(15, 30)},
	}
	for _, c := range cases {
		s, e := NSESession.Bucket(c.at, c.tf)
		if !s.Equal(c.start) || !e.Equal(c.end) {
			t.Errorf("%s %s: got %s-%s", c.at, c.tf, s, e)
		}
	}
}

func TestResampleMatchesManualAggregation(t *testing.T) {
	d, _ := ParseDate("2026-11-06")
	var bars []Bar
	for i, c := range []struct {
		o, h, l, cl Paise
		v           int64
	}{
		{100, 110, 95, 105, 10},
		{105, 120, 100, 115, 20},
		{115, 118, 90, 92, 30},
		{92, 95, 91, 94, 40},
		{94, 99, 93, 98, 50},
		{98, 101, 97, 100, 60}, // second bucket
	} {
		bars = append(bars, minuteBar("K", d.At(9, 15+i), c.o, c.h, c.l, c.cl, c.v, int64(i)))
	}
	out, err := Resample(bars, NSESession, Minute5)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d bars", len(out))
	}
	first := out[0]
	if first.Open != 100 || first.High != 120 || first.Low != 90 || first.Close != 98 || first.Volume != 150 || first.OI != 4 {
		t.Errorf("bad first bar: %+v", first)
	}
	if !first.Time.Equal(d.At(9, 15)) || !first.End.Equal(d.At(9, 20)) {
		t.Errorf("bad first bar times: %+v", first)
	}
	if out[1].Open != 98 || out[1].Close != 100 || out[1].Volume != 60 {
		t.Errorf("bad second bar: %+v", out[1])
	}
}

func TestBarCompletesOnFinalMinuteNotAfter(t *testing.T) {
	d, _ := ParseDate("2026-11-06")
	r, _ := NewResampler(NSESession, Minute5)
	for i := range 4 {
		done, err := r.Add(minuteBar("K", d.At(9, 15+i), 1, 1, 1, 1, 1, 0))
		if err != nil {
			t.Fatal(err)
		}
		if len(done) != 0 {
			t.Fatalf("minute %d completed a bar early", i)
		}
	}
	done, err := r.Add(minuteBar("K", d.At(9, 19), 1, 1, 1, 1, 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 {
		t.Fatalf("final minute should complete the bar, got %d", len(done))
	}
}

func TestIlliquidBucketFlushesByClock(t *testing.T) {
	d, _ := ParseDate("2026-11-06")
	r, _ := NewResampler(NSESession, Minute5)
	// Only one trade, early in the bucket.
	if _, err := r.Add(minuteBar("K", d.At(9, 16), 1, 1, 1, 1, 1, 0)); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.FlushUntil(d.At(9, 19)); ok {
		t.Fatal("flushed before the bucket ended")
	}
	b, ok := r.FlushUntil(d.At(9, 20))
	if !ok || !b.End.Equal(d.At(9, 20)) {
		t.Fatalf("expected flush at bucket end, got %v %+v", ok, b)
	}
}

func TestResamplerRejectsBadInput(t *testing.T) {
	d, _ := ParseDate("2026-11-06")
	r, _ := NewResampler(NSESession, Minute5)
	if _, err := r.Add(minuteBar("K", d.At(9, 0), 1, 1, 1, 1, 1, 0)); err == nil {
		t.Error("pre-open bar accepted")
	}
	if _, err := r.Add(minuteBar("K", d.At(9, 20), 1, 1, 1, 1, 1, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Add(minuteBar("K", d.At(9, 20), 1, 1, 1, 1, 1, 0)); err == nil {
		t.Error("duplicate bar accepted")
	}
	if _, err := NewResampler(NSESession, 0); err == nil {
		t.Error("zero timeframe accepted")
	}
}
