package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"serene/internal/kite"
	"serene/internal/market"
)

// The tick recorder (user, 2026-11-26: "start recording tick data for the
// contract traded … I need this to figure out SL"): a long-running service
// that, on NSE trading days from 09:14 to 15:31, streams Kite's full-mode
// ticks of every contract hma_crossover watches (the day files in
// data/paper, ATM and ±100 CE/PE of each index) into
// data/ticks/<day>/<EXCHANGE>_<SYMBOL>.csv. Every watched contract is
// recorded from when hma_crossover writes the day file (its 09:16:20 run,
// after the 09:15 bar closes; no guessing strikes before the open, user
// 2026-11-27), so a trade's file already holds the ticks before its buy.
// Nothing goes to Slack (user, 2026-11-27): the files stay on hearth.
const (
	tickStart = 9*60 + 14
	tickEnd   = 15*60 + 31
)

// tickHeader is the recorder's CSV columns. Times are IST.
var tickHeader = []string{"recv_time", "exch_time", "ltp", "last_qty", "last_trade_time", "volume", "avg_price", "oi",
	"bid", "bid_qty", "ask", "ask_qty"}

const tickTimeFmt = "2006-01-02 15:04:05.000"

func runTicks(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("paper ticks", flag.ContinueOnError)
	dir := fs.String("dir", "data/ticks", "where the day's tick files go")
	paperDir := fs.String("paper", "data/paper", "hma_crossover's day files: the contracts to record")
	probe := fs.Duration("probe", 0, "connect now, subscribe to the NIFTY 50 index (token 256265) and the latest day files' contracts, print what comes for this long, record nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *probe > 0 {
		return probeTicks(ctx, *probe, *paperDir)
	}
	for {
		now := time.Now().In(market.IST)
		d := market.DateOf(now)
		if m := minuteOfDay(now); !market.NSETradingDay(d) || m < tickStart || m >= tickEnd {
			if err := sleepCtx(ctx, time.Minute); err != nil {
				return err
			}
			continue
		}
		err := recordTicks(ctx, d, *dir, *paperDir)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "ticks:", err)
			if err := sleepCtx(ctx, 10*time.Second); err != nil {
				return err
			}
		}
	}
}

// tickContract is a recorded contract: Kite's EXCHANGE:SYMBOL.
type tickContract struct{ quote string }

// tickContracts is the day's watched contracts by Kite token.
func tickContracts(d market.Date, paperDir string) (map[int64]tickContract, error) {
	out := map[int64]tickContract{}
	for _, x := range hmaIndices {
		name := "day-" + d.String() + ".json"
		if x.Name != "NIFTY" {
			name = "day-" + x.Name + "-" + d.String() + ".json"
		}
		var st paperDayState
		ok, err := readJSONFile(filepath.Join(paperDir, name), &st)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		for _, l := range st.Legs {
			out[l.Token] = tickContract{quote: string(x.Exchange) + ":" + l.Symbol}
		}
	}
	return out, nil
}

// tickPath is the day's tick file of quote (EXCHANGE:SYMBOL).
func tickPath(dir string, d market.Date, quote string) string {
	return filepath.Join(dir, d.String(), strings.ReplaceAll(quote, ":", "_")+".csv")
}

type tickFile struct {
	f *os.File
	w *bufio.Writer
	c *csv.Writer
}

// recordTicks streams until the session ends or the connection fails,
// subscribing to new day-file contracts as hma_crossover writes them.
func recordTicks(ctx context.Context, d market.Date, dir, paperDir string) error {
	kc, err := kiteClient(ctx)
	if err != nil {
		return err
	}
	t, err := kite.DialTicker(ctx, kite.TickerURL, kc.APIKey, kc.Token)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer t.Close()
	files := map[int64]*tickFile{}
	defer func() {
		for _, f := range files {
			f.c.Flush()
			f.w.Flush()
			f.f.Close()
		}
	}()
	type msg struct {
		ticks []kite.Tick
		err   error
	}
	msgs := make(chan msg, 64)
	go func() {
		for {
			rctx, rcancel := context.WithTimeout(ctx, time.Minute)
			ts, err := t.Read(rctx)
			rcancel()
			select {
			case msgs <- msg{ts, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	contracts := map[int64]tickContract{}
	check := time.NewTicker(15 * time.Second)
	defer check.Stop()
	flush := time.NewTicker(2 * time.Second)
	defer flush.Stop()
	subscribe := func() error {
		cs, err := tickContracts(d, paperDir)
		if err != nil {
			return err
		}
		var add []int64
		for tok, c := range cs {
			if _, ok := contracts[tok]; !ok {
				contracts[tok] = c
				add = append(add, tok)
			}
		}
		if len(add) > 0 {
			fmt.Printf("ticks: recording %d more contract(s), %d in all\n", len(add), len(contracts))
		}
		return t.Subscribe(ctx, add)
	}
	if err := subscribe(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-check.C:
			if minuteOfDay(time.Now().In(market.IST)) >= tickEnd {
				return nil
			}
			if err := subscribe(); err != nil {
				return err
			}
		case <-flush.C:
			for _, f := range files {
				f.c.Flush()
				if err := f.w.Flush(); err != nil {
					return err
				}
			}
		case m := <-msgs:
			if m.err != nil {
				return m.err
			}
			recv := time.Now().In(market.IST)
			for _, tk := range m.ticks {
				c, ok := contracts[tk.Token]
				if !ok {
					continue
				}
				f := files[tk.Token]
				if f == nil {
					if f, err = openTickFile(tickPath(dir, d, c.quote)); err != nil {
						return err
					}
					files[tk.Token] = f
				}
				if err := f.c.Write(tickRow(recv, tk)); err != nil {
					return err
				}
			}
		}
	}
}

func openTickFile(path string) (*tickFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	w := bufio.NewWriter(f)
	tf := &tickFile{f: f, w: w, c: csv.NewWriter(w)}
	if fi, err := f.Stat(); err == nil && fi.Size() == 0 {
		if err := tf.c.Write(tickHeader); err != nil {
			f.Close()
			return nil, err
		}
	}
	return tf, nil
}

func tickRow(recv time.Time, t kite.Tick) []string {
	ts := func(x time.Time) string {
		if x.IsZero() {
			return ""
		}
		return x.In(market.IST).Format(tickTimeFmt)
	}
	px := func(p float64) string { return strconv.FormatFloat(p, 'f', 2, 64) }
	n := func(q int64) string { return strconv.FormatInt(q, 10) }
	return []string{ts(recv), ts(t.Exchange), px(t.LTP), n(t.LastQty), ts(t.LastTrade), n(t.Volume), px(t.Avg), n(t.OI),
		px(t.Bid), n(t.BidQty), px(t.Ask), n(t.AskQty)}
}

func probeTicks(ctx context.Context, wait time.Duration, paperDir string) error {
	kc, err := kiteClient(ctx)
	if err != nil {
		return err
	}
	t, err := kite.DialTicker(ctx, kite.TickerURL, kc.APIKey, kc.Token)
	if err != nil {
		return err
	}
	defer t.Close()
	tokens := []int64{niftyIndexToken}
	d := market.DateOf(time.Now().In(market.IST))
	for range 10 {
		cs, err := tickContracts(d, paperDir)
		if err != nil {
			return err
		}
		if len(cs) > 0 {
			for tok, c := range cs {
				tokens = append(tokens, tok)
				fmt.Println("subscribing", tok, c.quote, "from", d)
			}
			break
		}
		d = d.AddDays(-1)
	}
	if err := t.Subscribe(ctx, tokens); err != nil {
		return err
	}
	fmt.Println("connected; subscribed to", len(tokens), "tokens")
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	msgs, ticks := 0, 0
	for {
		ts, err := t.Read(ctx)
		if ctx.Err() != nil {
			fmt.Printf("%d messages, %d ticks of tradables in %s\n", msgs, ticks, wait)
			return nil
		}
		if err != nil {
			return err
		}
		msgs++
		for _, tk := range ts {
			ticks++
			if ticks <= 5 {
				fmt.Println(strings.Join(tickRow(time.Now(), tk), ","))
			}
		}
	}
}
