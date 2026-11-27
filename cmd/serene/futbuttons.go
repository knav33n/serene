package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"serene/internal/auth"
	"serene/internal/kite"
	"serene/internal/manual"
	"serene/internal/market"
	"serene/internal/slack"
)

// futButtonStrategies are the forward signals posted with a Zerodha button:
// the signal's side only, on the nearest future at least futMinDays from expiry.
var futButtonStrategies = []string{"goldm_hma_1h", "silverm_hma_1h"}

// futButtonsMaxAge: an older signal (a backfill) is posted without buttons.
const futButtonsMaxAge = 15 * time.Minute

// futMinDays: a button's future has at least this many days to expiry.
const futMinDays = 15

// buttonFuture is the nearest of futs (nearest first) expiring at least
// futMinDays after today.
func buttonFuture(futs []kite.Instrument, today market.Date) (kite.Instrument, bool) {
	cut := today.AddDays(futMinDays)
	for _, f := range futs {
		if !f.Expiry.Before(cut) {
			return f, true
		}
	}
	return kite.Instrument{}, false
}

// mcxFutures is name's MCX futures on Kite not yet expired, nearest first.
func mcxFutures(ins []kite.Instrument, name string, today market.Date) []kite.Instrument {
	var out []kite.Instrument
	for _, i := range ins {
		if i.Exchange == "MCX" && i.Type == "FUT" && i.Name == name && !i.Expiry.Before(today) {
			out = append(out, i)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Expiry.Before(out[b].Expiry) })
	return out
}

// futButtons is Hearth's handler for the signals' buttons, when
// SLACK_SIGNING_SECRET and SLACK_ORDER_USERS are set; nil without them.
func futButtons(cl *clients, logf func(string, ...any), dryRun bool) (*manual.SlackActions, error) {
	secret, users := os.Getenv("SLACK_SIGNING_SECRET"), os.Getenv("SLACK_ORDER_USERS")
	if secret == "" && users == "" {
		return nil, nil
	}
	if secret == "" || users == "" {
		return nil, errors.New("Slack's futures buttons need both SLACK_SIGNING_SECRET and SLACK_ORDER_USERS (member IDs, comma-separated)")
	}
	token := os.Getenv("SLACK_TOKEN")
	if token == "" {
		return nil, errors.New("Slack's futures buttons need SLACK_TOKEN to reply")
	}
	allowed := map[string]bool{}
	for _, u := range strings.Split(users, ",") {
		if u = strings.TrimSpace(u); u != "" {
			allowed[u] = true
		}
	}
	sc := slack.New(token)
	return &manual.SlackActions{Secret: secret, Users: allowed, Log: logf,
		Reply: func(ctx context.Context, channel, ts, text string) {
			if err := sc.Reply(ctx, channel, ts, text); err != nil {
				logf("slack reply: %v", err)
			}
		},
		Place: func(ctx context.Context, o manual.FutOrder) (string, error) {
			kc, err := cl.zerodha(ctx)
			if err != nil {
				return "", err
			}
			return placeFut(ctx, kc, o, time.Now(), dryRun)
		}}, nil
}

// placeFut places one lot of a GOLDM or SILVERM future at market, NRML, and
// waits up to half a minute to report its fill. A dry run stops before the
// order.
func placeFut(ctx context.Context, kc *kite.Client, o manual.FutOrder, now time.Time, dryRun bool) (string, error) {
	if !slices.Contains(futButtonStrategies, o.Strategy) {
		return "", fmt.Errorf("%s has no buttons", o.Strategy)
	}
	name := mcxForwardUnderlying(o.Strategy)
	ins, err := kc.Instruments(ctx, "MCX")
	if err != nil {
		return "", fmt.Errorf("kite MCX instruments: %w", err)
	}
	i := slices.IndexFunc(mcxFutures(ins, name, market.DateOf(now.In(market.IST))), func(f kite.Instrument) bool { return f.Symbol == o.Symbol })
	if i < 0 {
		return "", fmt.Errorf("%s isn't a live %s future on Kite", o.Symbol, name)
	}
	side := "BUY"
	if o.Side == market.Sell {
		side = "SELL"
	}
	if dryRun {
		return fmt.Sprintf(":test_tube: dry run: would %s 1 lot %s on Zerodha (MARKET, NRML) for %s; nothing placed", side, o.Symbol, o.Strategy), nil
	}
	// Kite's MCX quantity is in lots.
	id, err := kc.PlaceOrder(ctx, kite.OrderRequest{Exchange: "MCX", Symbol: o.Symbol, Side: side, Type: "MARKET", Product: "NRML",
		Qty: 1, Tag: "slack" + now.In(market.IST).Format("0102150405")})
	if err != nil {
		return "", err
	}
	return awaitFill(ctx, kc, id, fmt.Sprintf("%s 1 lot %s on Zerodha (order %s)", side, o.Symbol, id))
}

// futContracts gives a fresh entry signal its button: its side, on the
// nearest future with futMinDays to go. Kite's MCX list is read once per run, on the first need.
type futContracts struct {
	on     bool
	loaded bool
	ins    []kite.Instrument
}

func newFutContracts(on bool) *futContracts { return &futContracts{on: on} }

// row is the signal's button, or nil: buttons are off, the strategy has
// none, the signal is an exit or a backfill, or Kite's contracts can't be read (the
// signal then posts as text, which matters more than its buttons).
func (f *futContracts) row(ctx context.Context, s fwdSignal, now time.Time) slack.Block {
	// A reversal posts its exit and its entry on the same side: only the
	// entry gets the button, so one click is one lot.
	if !f.on || !slices.Contains(futButtonStrategies, s.Strategy) || s.Note != "enter" || now.Sub(s.Time) > futButtonsMaxAge {
		return nil
	}
	if !f.loaded {
		f.loaded = true
		kc, err := kiteClient(ctx)
		if err == nil {
			f.ins, err = kc.Instruments(ctx, "MCX")
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "serene: no futures buttons:", err)
		}
	}
	today := market.DateOf(now.In(market.IST))
	c, ok := buttonFuture(mcxFutures(f.ins, mcxForwardUnderlying(s.Strategy), today), today)
	if !ok {
		return nil
	}
	return manual.FutButtons(s.Strategy, s.Side, c.Symbol, fmt.Sprintf("%02d/%02d/%04d", c.Expiry.Day, int(c.Expiry.Month), c.Expiry.Year))
}

// runFutButtonsTest posts a test signal with the futures buttons, by default
// to a direct message with the first of SLACK_ORDER_USERS, to try the clicks
// against a Hearth run with --slack-dry-run.
func runFutButtonsTest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("forward buttons-test", flag.ContinueOnError)
	strat := fs.String("strategy", "goldm_hma_1h", "goldm_hma_1h or silverm_hma_1h")
	sideFlag := fs.String("side", "buy", "buy or sell")
	channel := fs.String("channel", "", "channel ID or member ID; default a DM with the first of SLACK_ORDER_USERS")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !slices.Contains(futButtonStrategies, *strat) {
		return fmt.Errorf("%s has no buttons", *strat)
	}
	side := market.Buy
	switch strings.ToLower(*sideFlag) {
	case "buy":
	case "sell":
		side = market.Sell
	default:
		return fmt.Errorf("--side %q: buy or sell", *sideFlag)
	}
	if err := auth.LoadDotEnv(".env"); err != nil {
		return err
	}
	target := *channel
	if target == "" {
		target = strings.TrimSpace(strings.Split(os.Getenv("SLACK_ORDER_USERS"), ",")[0])
	}
	token := os.Getenv("SLACK_TOKEN")
	if token == "" || target == "" {
		return errors.New("needs SLACK_TOKEN, and SLACK_ORDER_USERS or --channel")
	}
	sc := slack.New(token)
	ch, err := sc.Resolve(ctx, target)
	if err != nil {
		return err
	}
	now := time.Now()
	s := fwdSignal{Strategy: *strat, Time: now, Side: side, Instrument: mcxForwardUnderlying(*strat) + " FUT", Note: "enter"}
	row := newFutContracts(true).row(ctx, s, now)
	if row == nil {
		return errors.New("no futures from Kite for the buttons")
	}
	text := fmt.Sprintf(":test_tube: *TEST, not a signal* %s %s button · %s · pressing it places a REAL order", *strat, strings.ToUpper(*sideFlag), now.In(market.IST).Format("2006-01-02 15:04"))
	if _, err := sc.PostBlocks(ctx, ch, text, []slack.Block{slack.SectionBlock(text), row}); err != nil {
		return err
	}
	fmt.Println("posted the test buttons to", ch)
	return nil
}

// awaitFill waits up to half a minute for order id to fill and says how it went.
func awaitFill(ctx context.Context, kc interface {
	OrderStatus(ctx context.Context, id string) (kite.Order, error)
}, id, what string) (string, error) {
	var st kite.Order
	var err error
	for range 15 {
		if err := sleepCtx(ctx, 2*time.Second); err != nil {
			break
		}
		if st, err = kc.OrderStatus(ctx, id); err != nil {
			continue
		}
		switch strings.ToUpper(st.Status) {
		case "COMPLETE":
			return fmt.Sprintf(":white_check_mark: %s filled @ ₹%.2f", what, st.Average), nil
		case "REJECTED", "CANCELLED":
			return "", fmt.Errorf("%s %s: %s", what, strings.ToLower(st.Status), st.StatusMessage)
		}
	}
	return fmt.Sprintf(":hourglass: %s placed; status %s after 30s, check Kite", what, st.Status), nil
}
