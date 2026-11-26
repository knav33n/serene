// Command optbt runs a grid of short straddle/strangle settings from a YAML
// config (see configs/optbt) over Fyers 1m bars and writes one CSV row per
// cell, ranked by MDD pass then t. --trades CELL writes that cell's per-day
// table instead.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"serene/internal/optbt"
)

func main() {
	cfgPath := flag.String("config", "configs/optbt/short_premium.yaml", "")
	from := flag.String("from", "", "override the config's from")
	to := flag.String("to", "", "override the config's to")
	index := flag.String("index", "", "only this index (NIFTY, SENSEX)")
	cellsF := flag.String("cells", "", "only these cell names, comma-separated")
	trades := flag.String("trades", "", "write the per-day table of this one cell to --out")
	workers := flag.Int("workers", runtime.NumCPU(), "")
	out := flag.String("out", "", "CSV path; default reports/optbt/<name>[_<index>].csv")
	minN := flag.Int("min-n", 1, "leave out cells with fewer trading days")
	top := flag.Int("top", 15, "print this many top cells")
	topN := flag.Int("top-min-n", 50, "printed cells need at least this many trading days")
	dry := flag.Bool("dry", false, "print the cell count and exit")
	flag.Parse()

	cfg, err := optbt.LoadConfig(*cfgPath)
	must(err)
	if cfg.Name == "" {
		cfg.Name = strings.TrimSuffix(filepath.Base(*cfgPath), filepath.Ext(*cfgPath))
	}
	if *from != "" {
		cfg.From = *from
	}
	if *to != "" {
		cfg.To = *to
	}
	if *index != "" {
		cfg.Index = []string{*index}
	}
	cells, err := cfg.Expand()
	must(err)
	want := map[string]bool{}
	for _, n := range strings.Split(*cellsF, ",") {
		if n != "" {
			want[n] = true
		}
	}
	if *trades != "" {
		want = map[string]bool{*trades: true}
	}
	if len(want) > 0 {
		var keep []optbt.Cell
		for _, c := range cells {
			if want[c.Name()] {
				c.ID = len(keep)
				keep = append(keep, c)
			}
		}
		if len(keep) != len(want) {
			must(fmt.Errorf("found %d of %d requested cells in the grid", len(keep), len(want)))
		}
		cells = keep
	}
	perIndex := map[string]int{}
	for _, c := range cells {
		perIndex[c.Index]++
	}
	fmt.Fprintf(os.Stderr, "%d cells %v, %s..%s\n", len(cells), perIndex, cfg.From, cfg.To)
	if *dry {
		return
	}
	path := *out
	if path == "" {
		suffix := ""
		if *index != "" {
			suffix = "_" + *index
		}
		if *trades != "" {
			suffix = "_" + *trades
		}
		path = filepath.Join("reports", "optbt", cfg.Name+suffix+".csv")
	}
	must(os.MkdirAll(filepath.Dir(path), 0o755))

	opt := optbt.Options{Workers: *workers, Log: os.Stderr}
	var days []optbt.DayRow
	if *trades != "" {
		opt.Detail = true
		opt.VIX, err = optbt.LoadVIX(cfg.VIXDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "no VIX: %v\n", err)
		}
		opt.OnDay = func(_ *optbt.Cell, row optbt.DayRow) { days = append(days, row) }
	}
	start := time.Now()
	accs, err := optbt.Run(context.Background(), cfg, cells, opt)
	must(err)
	fmt.Fprintf(os.Stderr, "ran in %s\n", time.Since(start).Round(time.Second))

	fh, err := os.Create(path)
	must(err)
	defer fh.Close()
	if *trades != "" {
		must(optbt.WriteDays(fh, days))
	}
	rows := optbt.Rank(cells, accs, cfg.MDDMax, *minN)
	if *trades == "" {
		must(optbt.WriteGrid(fh, rows))
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", path)
	fmt.Printf("%-70s %5s %6s %6s %7s %5s %5s %9s\n", "cell", "pass", "mdd%", "rec", "exp₹", "win%", "t", "net₹")
	printed := 0
	for _, r := range rows {
		if printed >= *top {
			break
		}
		if r.S.N < *topN {
			continue
		}
		printed++
		s := r.S
		fmt.Printf("%-70s %5t %6.2f %6.2f %7.0f %5.1f %5.2f %9.0f\n", r.Cell.Name(), r.Pass, s.MDDPct, s.RecRatio(),
			s.Avg, s.Win, s.T, s.NetSlip)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
