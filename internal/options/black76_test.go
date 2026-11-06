package options

import (
	"errors"
	"math"
	"testing"
)

func TestPutCallParity(t *testing.T) {
	c := Input{Forward: 24100, Strike: 24000, Years: 7.0 / 365, Rate: 0.06, Call: true}
	p := c
	p.Call = false
	got := Price(c, 0.15) - Price(p, 0.15)
	want := math.Exp(-c.Rate*c.Years) * (c.Forward - c.Strike)
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("parity: got %f want %f", got, want)
	}
}

func TestImpliedVolRoundTrip(t *testing.T) {
	for _, vol := range []float64{0.08, 0.15, 0.4, 1.2} {
		for _, k := range []float64{23000, 24000, 25000} {
			for _, call := range []bool{true, false} {
				in := Input{Forward: 24000, Strike: k, Years: 5.0 / 365, Rate: 0.06, Call: call}
				got, err := ImpliedVol(in, Price(in, vol))
				if Price(in, vol) < 1e-3 {
					continue // no information left in a near-zero price
				}
				if err != nil || math.Abs(got-vol) > 1e-4 {
					t.Errorf("vol %f k %f call %v: got %f err %v", vol, k, call, got, err)
				}
			}
		}
	}
}

func TestImpliedVolRejectsPriceBelowIntrinsic(t *testing.T) {
	in := Input{Forward: 24500, Strike: 24000, Years: 5.0 / 365, Call: true}
	if _, err := ImpliedVol(in, 100); !errors.Is(err, ErrNoVol) {
		t.Errorf("got %v", err)
	}
}

func TestGreeksMatchFiniteDifferences(t *testing.T) {
	in := Input{Forward: 24000, Strike: 24100, Years: 10.0 / 365, Rate: 0.06, Call: true}
	const vol = 0.15
	g := Sensitivities(in, vol)
	up, dn := in, in
	up.Forward += 1
	dn.Forward -= 1
	if fd := (Price(up, vol) - Price(dn, vol)) / 2; math.Abs(fd-g.Delta) > 1e-4 {
		t.Errorf("delta %f vs fd %f", g.Delta, fd)
	}
	if fd := Price(up, vol) - 2*Price(in, vol) + Price(dn, vol); math.Abs(fd-g.Gamma) > 1e-4 {
		t.Errorf("gamma %f vs fd %f", g.Gamma, fd)
	}
	if fd := (Price(in, vol+1e-4) - Price(in, vol-1e-4)) / 2e-4; math.Abs(fd-g.Vega) > 1e-2 {
		t.Errorf("vega %f vs fd %f", g.Vega, fd)
	}
}

func TestExpiredOptionIsIntrinsic(t *testing.T) {
	if got := Price(Input{Forward: 24100, Strike: 24000, Call: true}, 0.2); got != 100 {
		t.Errorf("got %f", got)
	}
}
