package upstox

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"testing"
)

func TestKeyRatiosAndIncome(t *testing.T) {
	var paths []string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		switch r.URL.Path {
		case "/v2/fundamentals/INE002A01018/key-ratios":
			fmt.Fprint(w, `{"status":"success","data":[{"name":"P/E","company_value":"19.37","sector_value":"17.56"},
				{"name":"ROE","company_value":"8.69%","sector_value":"16.96%"},{"name":"ROCE","company_value":"-","sector_value":"1"}]}`)
		case "/v2/fundamentals/INE002A01018/income-statement":
			fmt.Fprint(w, `{"status":"success","data":{"type":"consolidated","time_period":"quarterly","units_in":"crore",
				"income_statement":[{"category":"net_profit","history":[{"value":29250.0,"period":"Sep 2026","change":"+27.17%"},
				{"value":23001.0,"period":"Jun 2026"}]}],"full_statement":null}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx := context.Background()
	rs, err := c.KeyRatios(ctx, "INE002A01018")
	if err != nil {
		t.Fatal(err)
	}
	if RatioValue(rs, "P/E") != 19.37 || RatioValue(rs, "roe") != 8.69 || !math.IsNaN(RatioValue(rs, "ROCE")) ||
		!math.IsNaN(RatioValue(rs, "P/B")) {
		t.Errorf("%+v", rs)
	}
	in, err := c.IncomeStatement(ctx, "INE002A01018", true)
	if err != nil {
		t.Fatal(err)
	}
	h := in.History("net_profit")
	if len(h) != 2 || h[0].Value != 29250 || h[0].Period != "Sep 2026" || in.History("revenue") != nil {
		t.Errorf("%+v", in)
	}
	if paths[1] != "/v2/fundamentals/INE002A01018/income-statement?type=consolidated&time_period=quarterly" {
		t.Errorf("%v", paths)
	}
}
