package fobhav

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"serene/internal/market"
)

func d(y int, m time.Month, day int) market.Date { return market.Date{Year: y, Month: m, Day: day} }

const legacyHead = "INSTRUMENT,SYMBOL,EXPIRY_DT,STRIKE_PR,OPTION_TYP,OPEN,HIGH,LOW,CLOSE,SETTLE_PR,CONTRACTS,VAL_INLAKH,OPEN_INT,CHG_IN_OI,TIMESTAMP,\n"

const udiffHead = "TradDt,BizDt,Sgmt,Src,FinInstrmTp,FinInstrmId,ISIN,TckrSymb,SctySrs,XpryDt,FininstrmActlXpryDt,StrkPric,OptnTp,FinInstrmNm,OpnPric,HghPric,LwPric,ClsPric,LastPric,PrvsClsgPric,UndrlygPric,SttlmPric,OpnIntrst,ChngInOpnIntrst,TtlTradgVol,TtlTrfVal,TtlNbOfTxsExctd,SsnId,NewBrdLotQty,Rmks,Rsvd1,Rsvd2,Rsvd3,Rsvd4\n"

func TestParseBothFormats(t *testing.T) {
	legacy := legacyHead +
		"FUTSTK,SBIN,31-Jan-2019,0,XX,305.4,308.4,304.35,306.3,306.3,14636,134574.16,75288000,-1287000,10-JAN-2019,\n" +
		"OPTSTK,SBIN,31-Jan-2019,97.5,CE,0,0,0,18.7,12.65,0,0,0,0,10-JAN-2019,\n" +
		"FUTIVX,INDIAVIX,15-Jan-2019,0,XX,1,1,1,1,1,1,1,1,1,10-JAN-2019,\n"
	rows, err := Parse([]byte(legacy), d(2019, 1, 10))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || !rows[0].Future || rows[0].Index || rows[0].Volume != 14636 || rows[0].ValueRs != 134574.16e5 {
		t.Fatalf("legacy %+v", rows)
	}
	if rows[1].Future || rows[1].Strike != 97.5 || rows[1].Right != "CE" || rows[1].Expiry != d(2019, 1, 31) {
		t.Fatalf("legacy option %+v", rows[1])
	}
	udiff := udiffHead +
		"2026-01-13,2026-01-13,FO,NSE,STO,158255,,SBIN,,2026-03-30,2026-03-30,1040.00,PE,SBIN26MAR1040PE,0.00,0.00,0.00,78.10,0.00,78.10,1028.45,37.90,0,0,0,0.00,0,F1,750,,,,,\n" +
		"2026-01-13,2026-01-13,FO,NSE,IDF,1,,NIFTY,,2026-01-27,2026-01-27,,,NIFTY26JANFUT,0,0,0,25800,0,0,0,25800,0,0,0,0,0,F1,65,,,,,\n"
	rows, err = Parse([]byte(udiff), d(2026, 1, 13))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Lot != 750 || rows[0].Right != "PE" || !rows[1].Future || !rows[1].Index || rows[1].Lot != 65 {
		t.Fatalf("udiff %+v", rows)
	}
	if _, err := Parse([]byte(legacy), d(2019, 1, 11)); err == nil {
		t.Fatal("accepted a file dated another day")
	}
}

func TestLotFromTurnover(t *testing.T) {
	// SBIN's lot was 3000: turnover / contracts / price lands near it on each
	// day. The estimate is approximate; the market activity report is exact.
	var l lotInterval
	for _, r := range []Row{
		{Volume: 14636, ValueRs: 134574.16e5, High: 308.4, Low: 304.35, Settle: 306.3},
		{Volume: 10000, ValueRs: 3000 * 10000 * 290.0, High: 292, Low: 288, Settle: 290},
	} {
		l.add(r)
	}
	if got := l.size(); got < 2995 || got > 3005 {
		t.Fatalf("lot %d (interval %.1f..%.1f)", got, l.lo, l.hi)
	}
	// An odd size is found when the interval is tight.
	var odd lotInterval
	odd.add(Row{Volume: 100, ValueRs: 1061 * 100 * 500.0, High: 500.5, Low: 499.5, Settle: 500})
	if got := odd.size(); got != 1061 {
		t.Fatalf("odd lot %d (%.1f..%.1f)", got, odd.lo, odd.hi)
	}
}

func zipped(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	w, err := z.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte(body))
	z.Close()
	return buf.Bytes()
}

func TestBuild(t *testing.T) {
	root := t.TempDir()
	put := func(day market.Date, body string) {
		p := filepath.Join(nseDir(root), "raw", "2019", day.String()+".csv.zip")
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, zipped(t, "x.csv", body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// OLDCO: a monthly future and options; its lot is 1250 by the market
	// activity report, though turnover suggests 1200. One weekly-style option
	// expiry before it takes the monthly's lot.
	put(d(2019, 1, 10), legacyHead+
		"FUTSTK,OLDCO,31-Jan-2019,0,XX,1,101,99,100,100,50,60.0,1,1,10-JAN-2019,\n"+
		"OPTSTK,OLDCO,31-Jan-2019,100,CE,0,0,0,5,5,0,0,0,0,10-JAN-2019,\n"+
		"OPTSTK,OLDCO,31-Jan-2019,100,PE,0,0,0,5,5,0,0,0,0,10-JAN-2019,\n"+
		"OPTSTK,OLDCO,24-Jan-2019,95,PE,0,0,0,5,5,0,0,0,0,10-JAN-2019,\n"+
		"FUTSTK,FUTONLY,31-Jan-2019,0,XX,1,101,99,100,100,50,60.0,1,1,10-JAN-2019,\n")
	mkt := "INSTRUMENT,SYMBOL    ,EXP_DATE  ,OPEN_PRICE ,HI_PRICE   ,LO_PRICE   ,CLOSE_PRICE,OPEN_INT*      ,TRD_VAL           ,TRD_QTY          ,NO_OF_CONT       ,NO_OF_TRADE      \n" +
		"FUTSTK    ,OLDCO     ,31/01/2019,1,1,1,1,1,1,          62500,               50,1\n" +
		"FUTSTK    ,OLDCO     ,28/02/2019,1,1,1,1,1,1,              0,                0,0\n" +
		"* - OPEN_INT as available\n"
	p := mktPath(root, d(2019, 1, 10))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, zipped(t, "fo10012019.csv", mkt), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := Build(root, d(2018, 10, 1), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Symbols != 1 || sum.Stocks != 1 || sum.Estimated != 1 || sum.EstimateOff != 1 {
		t.Fatalf("summary %+v", sum)
	}
	s, ok, err := Load(root, "OLDCO")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if len(s.Expiries) != 2 || s.Closes["2019-01-10"] != 100 {
		t.Fatalf("symbol %+v", s)
	}
	for _, x := range s.Expiries {
		if x.Lot != 1250 || x.LotSource != SourceMkt {
			t.Errorf("%s lot %d %s", x.Expiry, x.Lot, x.LotSource)
		}
	}
	if x := s.Expiries[1]; !x.Future || len(x.Calls) != 1 || len(x.Puts) != 1 {
		t.Errorf("monthly %+v", x)
	}
	uni, err := ReadUniverse(filepath.Join(root, "universe.csv"))
	if err != nil || len(uni) != 1 || uni[0] != "OLDCO" {
		t.Fatalf("universe %v %v", uni, err)
	}
}
