package optbt

import (
	"math"
)

const firstYear, nYears = 2015, 20

// Acc accumulates one cell's daily results in date order, without keeping
// them, so a grid of a million cells fits in memory. All money is net after
// costs and slippage.
type Acc struct {
	N                 int
	wins, losses      int
	sum, sq, gw, gl   float64
	Net, Gross        float64
	worst, best       float64
	rom, marg         float64
	ceSL, peSL, both  int
	reent             int
	eq, peak          float64
	peakOrd, firstOrd int
	epDD              float64 // deepest drawdown below the current peak
	mdd, mddPeakV     float64
	mddPeakOrd        int
	mddTroughOrd      int
	recOrd            int // trading days from trough back to the peak; -1 = not yet
	front             []ddPt
	roi               [nYears]float64
	curW, curL        int
	maxW, maxL        int
}

type ddPt struct{ peak, dd float64 }

// Add takes the next day in date order.
func (a *Acc) Add(r *DayResult) {
	x := r.NetSlip
	if a.N == 0 {
		a.firstOrd, a.peakOrd, a.recOrd = r.Ord, r.Ord-1, -1
	}
	a.N++
	a.sum += x
	a.sq += x * x
	a.Net += r.Net
	a.Gross += r.Gross
	a.marg += r.Margin
	a.rom += x / r.Margin * 100
	a.worst, a.best = min(a.worst, x), max(a.best, x)
	if x > 0 {
		a.wins++
		a.gw += x
		a.curW, a.curL = a.curW+1, 0
	} else {
		a.losses++
		a.gl -= x
		a.curW, a.curL = 0, a.curL+1
	}
	a.maxW, a.maxL = max(a.maxW, a.curW), max(a.maxL, a.curL)
	if r.CESL {
		a.ceSL++
	}
	if r.PESL {
		a.peSL++
	}
	if r.CESL && r.PESL {
		a.both++
	}
	a.reent += r.Reentries
	if y := r.Date.Year - firstYear; y >= 0 && y < nYears {
		a.roi[y] += x
	}
	a.eq += x
	if a.eq > a.peak {
		a.pushEpisode()
		a.peak, a.peakOrd, a.epDD = a.eq, r.Ord, 0
	} else if dd := a.peak - a.eq; dd > a.epDD {
		a.epDD = dd
		if dd > a.mdd {
			a.mdd, a.mddPeakV, a.mddPeakOrd, a.mddTroughOrd, a.recOrd = dd, a.peak, a.peakOrd, r.Ord, -1
		}
	}
	if a.mdd > 0 && a.recOrd < 0 && r.Ord > a.mddTroughOrd && a.eq >= a.mddPeakV {
		a.recOrd = r.Ord - a.mddTroughOrd
	}
}

// pushEpisode keeps the drawdowns that can be the largest in % terms: peaks
// only rise, so a later episode matters only if it is deeper.
func (a *Acc) pushEpisode() {
	if a.epDD <= 0 {
		return
	}
	if k := len(a.front); k > 0 && a.front[k-1].dd >= a.epDD {
		return
	}
	a.front = append(a.front, ddPt{a.peak, a.epDD})
}

// Summary is the reported view of an Acc.
type Summary struct {
	N                     int
	Win                   float64 // % of days
	Avg, ExpR, T, PF      float64
	AvgWin, AvgLoss       float64
	NetSlip, Net, Gross   float64
	Worst, Best           float64
	Capital               float64 // average margin
	ROM                   float64 // average return on margin per day, %
	MDD, MDDPct           float64
	MDDDays, RecDays      int // trading days; RecDays −1 = not recovered
	ROI                   map[int]float64
	ROIMin, ROICV         float64
	CESL, PESL, BothSL    float64 // % of days
	ReentPerDay           float64
	MaxWinRun, MaxLossRun int
}

func (a *Acc) Summary() Summary {
	s := Summary{N: a.N, ROI: map[int]float64{}, RecDays: -1}
	if a.N == 0 {
		return s
	}
	n := float64(a.N)
	s.Win = float64(a.wins) / n * 100
	s.Avg = a.sum / n
	s.NetSlip, s.Net, s.Gross = a.sum, a.Net, a.Gross
	s.Worst, s.Best = a.worst, a.best
	if a.N > 1 {
		if sd := math.Sqrt(max(0, (a.sq-a.sum*a.sum/n)/(n-1))); sd > 0 {
			s.T = s.Avg / sd * math.Sqrt(n)
		}
	}
	if a.gl > 0 {
		s.PF = a.gw / a.gl
	}
	if a.wins > 0 {
		s.AvgWin = a.gw / float64(a.wins)
	}
	if a.losses > 0 {
		s.AvgLoss = a.gl / float64(a.losses)
		s.ExpR = s.Avg / s.AvgLoss
	}
	s.Capital = a.marg / n
	s.ROM = a.rom / n
	s.MDD = a.mdd
	if a.mdd > 0 {
		s.MDDDays = a.mddTroughOrd - a.mddPeakOrd
		s.RecDays = a.recOrd
	}
	front := a.front
	if a.epDD > 0 {
		front = append(front[:len(front):len(front)], ddPt{a.peak, a.epDD})
	}
	for _, p := range front {
		s.MDDPct = max(s.MDDPct, p.dd/(s.Capital+p.peak)*100)
	}
	var ys, ysq float64
	first := true
	for i, v := range a.roi {
		if v == 0 {
			continue
		}
		r := v / s.Capital * 100
		s.ROI[firstYear+i] = r
		ys += r
		ysq += r * r
		if first || r < s.ROIMin {
			s.ROIMin, first = r, false
		}
	}
	if k := float64(len(s.ROI)); k > 1 && ys != 0 {
		mu := ys / k
		s.ROICV = math.Sqrt(max(0, (ysq-k*mu*mu)/(k-1))) / math.Abs(mu)
	}
	s.CESL, s.PESL, s.BothSL = float64(a.ceSL)/n*100, float64(a.peSL)/n*100, float64(a.both)/n*100
	s.ReentPerDay = float64(a.reent) / n
	s.MaxWinRun, s.MaxLossRun = a.maxW, a.maxL
	return s
}

// RecRatio is recovery days ÷ drawdown days, or NaN when not recovered.
func (s Summary) RecRatio() float64 {
	if s.RecDays < 0 || s.MDDDays == 0 {
		return math.NaN()
	}
	return float64(s.RecDays) / float64(s.MDDDays)
}
