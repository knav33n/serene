package analytics

import (
	_ "embed"
	"fmt"
	"html/template"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"calm/internal/market"
	"calm/internal/strategy"
)

//go:embed report.html.tmpl
var reportTemplate string

type point struct {
	Date     string  `json:"d"`
	Equity   float64 `json:"e"`
	Drawdown float64 `json:"dd"`
}

type tradeRow struct {
	Tag, Key, Side, Entry, Exit, How string
	Qty                              int64
	EntryPrice, ExitPrice            float64
	Net                              string
	Loss                             bool
}

type tile struct{ Label, Value string }

type slipRow struct {
	Multiple, Net, Sharpe, MaxDD string
	Loss                         bool
}

// WriteReport renders a backtest as a standalone HTML file.
func WriteReport(path string, o Outcome, slip []SlippageRow) error {
	m := o.Metrics
	view := struct {
		Title    string
		Subtitle string
		Tiles    []tile
		Quality  []tile
		Slip     []slipRow
		Points   []point
		Trades   []tradeRow
		Hidden   int
	}{
		Title: fmt.Sprintf("%s on %s", o.Spec.Strategy, o.Spec.Underlying),
		Subtitle: fmt.Sprintf("%s to %s, %d trading days, capital %s, params %s",
			o.Spec.From, o.Spec.To, m.Days, rupees(o.Spec.Capital), paramText(o.Spec.Params)),
		Tiles: []tile{
			{"Net P&L", rupees(m.Net)}, {"Return", pct(m.ReturnPct)}, {"Sharpe", num(m.Sharpe)},
			{"Max drawdown", pct(m.MaxDrawdownPct)}, {"Win rate", pct(m.WinRatePct)},
			{"Profit factor", num(m.ProfitFactor)},
		},
		Quality: []tile{
			{"Trades", fmt.Sprint(m.Trades)}, {"Expectancy", rupees(m.Expectancy)},
			{"Average win", rupees(m.AvgWin)}, {"Average loss", rupees(m.AvgLoss)},
			{"Gross P&L", rupees(m.Gross)}, {"Charges", rupees(m.Charges)},
			{"Sortino", num(m.Sortino)}, {"CAGR", pct(m.CAGRPct)},
			{"Legs settled at expiry", fmt.Sprint(m.SettledLegs)},
			{"Orders never filled", fmt.Sprint(m.Unfilled)},
			{"Orders rejected", fmt.Sprint(m.Rejected)},
		},
	}
	for _, r := range slip {
		view.Slip = append(view.Slip, slipRow{fmt.Sprintf("%g×", r.Multiple), rupees(r.Metrics.Net),
			num(r.Metrics.Sharpe), pct(r.Metrics.MaxDrawdownPct), r.Metrics.Net < 0})
	}
	peak := o.Result.Capital
	for _, p := range o.Result.Equity {
		peak = max(peak, p.Equity)
		view.Points = append(view.Points, point{p.Date.String(), p.Equity.Rupees(), (p.Equity - peak).Rupees()})
	}
	const maxRows = 1000
	trades := o.Result.Trades
	if len(trades) > maxRows {
		view.Hidden = len(trades) - maxRows
		trades = trades[len(trades)-maxRows:]
	}
	for _, t := range trades {
		how := "traded"
		if t.Settled {
			how = "settled"
		}
		view.Trades = append(view.Trades, tradeRow{t.Tag, t.Key, t.Side.String(), stamp(t.EntryTime), stamp(t.ExitTime), how,
			t.Qty, t.EntryPrice, t.ExitPrice, rupees(t.Net()), t.Net() < 0})
	}
	tmpl, err := template.New("report").Parse(reportTemplate)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return tmpl.Execute(f, view)
}

func paramText(p strategy.Params) string {
	parts := make([]string, 0, len(p))
	for _, k := range sortedKeys(p) {
		parts = append(parts, fmt.Sprintf("%s=%g", k, p[k]))
	}
	if len(parts) == 0 {
		return "default"
	}
	return strings.Join(parts, ", ")
}

func stamp(t time.Time) string { return t.In(market.IST).Format("2006-01-02 15:04") }

func rupees(p market.Paise) string {
	sign := ""
	if p < 0 {
		sign, p = "-", -p
	}
	return fmt.Sprintf("%s₹%s", sign, group(int64(math.Round(p.Rupees()))))
}

// group applies Indian digit grouping: 12,34,567.
func group(n int64) string {
	s := fmt.Sprint(n)
	if len(s) <= 3 {
		return s
	}
	out := s[len(s)-3:]
	s = s[:len(s)-3]
	for len(s) > 2 {
		out = s[len(s)-2:] + "," + out
		s = s[:len(s)-2]
	}
	return s + "," + out
}

func pct(v float64) string { return fmt.Sprintf("%.2f%%", v) }

func num(v float64) string {
	if math.IsInf(v, 1) {
		return "∞"
	}
	return fmt.Sprintf("%.2f", v)
}
