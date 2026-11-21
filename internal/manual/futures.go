package manual

// Futures orders from Slack buttons. A forward signal of an MCX strategy is
// posted with a button per contract (the current and next futures); pressing
// one, after Slack's confirm dialog, sends the click here, and Hearth places
// that order for one lot on Zerodha. Slack signs each click with the app's
// signing secret, and only the Slack members in Users may place orders.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"serene/internal/market"
	"serene/internal/slack"
)

// FutOrder is what a button asks for: one lot of Symbol, a Kite MCX future.
type FutOrder struct {
	Strategy string
	Side     market.Side
	Symbol   string
}

// Value is the button's value, which Slack returns with the click.
func (o FutOrder) Value() string {
	return strings.Join([]string{o.Strategy, sideWord(o.Side), o.Symbol}, "|")
}

func sideWord(s market.Side) string {
	if s == market.Sell {
		return "SELL"
	}
	return "BUY"
}

// ParseFutOrder reads a button's value.
func ParseFutOrder(v string) (FutOrder, error) {
	p := strings.Split(v, "|")
	if len(p) != 3 || p[0] == "" || p[2] == "" {
		return FutOrder{}, fmt.Errorf("unreadable button value %q", v)
	}
	o := FutOrder{Strategy: p[0], Symbol: p[2]}
	switch p[1] {
	case "BUY":
		o.Side = market.Buy
	case "SELL":
		o.Side = market.Sell
	default:
		return FutOrder{}, fmt.Errorf("unreadable side %q", p[1])
	}
	return o, nil
}

// FutButtons is the row of buttons under a signal: buy each contract, then
// sell each. labels name each contract on its button, e.g. its expiry.
func FutButtons(strategy string, syms, labels []string) slack.Block {
	var bs []slack.Button
	for _, side := range []market.Side{market.Buy, market.Sell} {
		style := "primary"
		if side == market.Sell {
			style = "danger"
		}
		for i, sym := range syms {
			o := FutOrder{Strategy: strategy, Side: side, Symbol: sym}
			bs = append(bs, slack.Button{ActionID: fmt.Sprintf("fut_%d", len(bs)), Text: fmt.Sprintf("%s %s (%s)", sideWord(side), sym, labels[i]),
				Value: o.Value(), Style: style,
				Confirm: fmt.Sprintf("*%s 1 lot %s* on Zerodha at market (NRML), for %s.", sideWord(side), sym, strategy)})
		}
	}
	return slack.ActionsBlock(bs...)
}

// SlackActions takes Slack's button clicks.
type SlackActions struct {
	Secret string          // the Slack app's signing secret
	Users  map[string]bool // Slack member IDs allowed to place orders
	// Place places the order and says what happened, e.g. the fill.
	Place func(ctx context.Context, o FutOrder) (string, error)
	// Reply posts in the thread under the signal.
	Reply func(ctx context.Context, channel, ts, text string)
	Log   func(format string, a ...any)
	Now   func() time.Time

	mu   sync.Mutex
	done map[string]bool // channel|message|button already pressed
}

func (a *SlackActions) logf(format string, args ...any) {
	if a.Log != nil {
		a.Log(format, args...)
	}
}

func (a *SlackActions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	if err := slack.Verify(a.Secret, r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature"), body, now); err != nil {
		a.logf("slack click refused from %s: %v", clientIP(r), err)
		http.Error(w, "forbidden", http.StatusUnauthorized)
		return
	}
	c, err := slack.ParseClick(body)
	if err != nil {
		a.logf("slack click: %v", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	// Slack wants an answer within 3 seconds; the order is placed after it.
	w.WriteHeader(http.StatusOK)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	reply := func(text string) {
		if a.Reply != nil {
			a.Reply(ctx, c.Channel, c.MessageTS, text)
		}
	}
	if !a.Users[c.User] {
		cancel()
		a.logf("slack click from %s (%s) refused: not an allowed user", c.User, c.UserName)
		reply(fmt.Sprintf(":no_entry: <@%s> isn't allowed to place orders from Slack.", c.User))
		return
	}
	o, err := ParseFutOrder(c.Value)
	if err != nil {
		cancel()
		reply(":x: " + err.Error())
		return
	}
	key := c.Channel + "|" + c.MessageTS + "|" + c.ActionID
	a.mu.Lock()
	if a.done == nil {
		a.done = map[string]bool{}
	}
	again := a.done[key]
	a.done[key] = true
	a.mu.Unlock()
	if again {
		cancel()
		reply(fmt.Sprintf(":warning: %s %s was already pressed on this signal; nothing placed. Use Kite for another lot.", sideWord(o.Side), o.Symbol))
		return
	}
	a.logf("slack: %s presses %s 1 lot %s (%s)", c.UserName, sideWord(o.Side), o.Symbol, o.Strategy)
	go func() {
		defer cancel()
		res, err := a.Place(ctx, o)
		if err != nil {
			a.logf("slack order %s %s: %v", sideWord(o.Side), o.Symbol, err)
			reply(fmt.Sprintf(":x: <@%s> %s 1 lot %s failed: %v", c.User, sideWord(o.Side), o.Symbol, err))
			return
		}
		reply(fmt.Sprintf("<@%s> %s", c.User, res))
	}()
}
