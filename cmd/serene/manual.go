package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"serene/internal/auth"
	"serene/internal/breeze"
	"serene/internal/fyers"
	"serene/internal/ingest"
	"serene/internal/kite"
	"serene/internal/manual"
	"serene/internal/market"
	"serene/internal/upstox"
	"serene/web"

	"serene/internal/cost"
)

// clients keeps each broker's client for a few minutes, so the page's calls
// don't each read the token store.
type clients struct {
	mu   sync.Mutex
	kite *kite.Client
	ups  *upstox.Client
	fy   *fyers.Trader
	bz   *breeze.Client
	at   map[string]time.Time
}

const clientTTL = 5 * time.Minute

func (c *clients) zerodha(ctx context.Context) (*kite.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.kite != nil && time.Since(c.at["zerodha"]) < clientTTL {
		return c.kite, nil
	}
	k, err := kiteClient(ctx)
	if err != nil {
		return nil, err
	}
	c.kite, c.at["zerodha"] = k, time.Now()
	return k, nil
}

func (c *clients) upstox(ctx context.Context) (*upstox.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ups != nil && time.Since(c.at["upstox"]) < clientTTL {
		return c.ups, nil
	}
	u, err := upstoxClient(ctx)
	if err != nil {
		return nil, err
	}
	c.ups, c.at["upstox"] = u, time.Now()
	return u, nil
}

func (c *clients) fyers(ctx context.Context) (*fyers.Trader, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fy != nil && time.Since(c.at["fyers"]) < clientTTL {
		return c.fy, nil
	}
	appID := os.Getenv("FYERS_APP_ID")
	if appID == "" {
		return nil, errors.New("FYERS_APP_ID is not set")
	}
	token, err := storedToken(ctx, "fyers")
	if err != nil {
		return nil, err
	}
	c.fy, c.at["fyers"] = fyers.NewTrader(appID, token), time.Now()
	return c.fy, nil
}

func (c *clients) icici(ctx context.Context) (*breeze.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bz != nil && time.Since(c.at["icici"]) < clientTTL {
		return c.bz, nil
	}
	key, secret := os.Getenv("BREEZE_API_KEY"), os.Getenv("BREEZE_API_SECRET")
	if key == "" || secret == "" {
		return nil, errors.New("BREEZE_API_KEY and BREEZE_API_SECRET must be set")
	}
	token, err := storedToken(ctx, "breeze")
	if err != nil {
		return nil, err
	}
	b, err := breeze.Login(ctx, key, secret, token.Reveal())
	if err != nil {
		return nil, err
	}
	c.bz, c.at["icici"] = b, time.Now()
	return b, nil
}

// storedToken is a broker's login from serene auth.
func storedToken(ctx context.Context, broker string) (auth.Secret, error) {
	tokens, err := openTokens(ctx)
	if err != nil {
		return "", err
	}
	defer tokens.Close()
	t, err := auth.Load(ctx, tokens, broker)
	if err != nil {
		return "", fmt.Errorf("%w (log in with serene auth)", err)
	}
	return t.AccessToken, nil
}

// contracts keeps the day's live Nifty options.
type contracts struct {
	mu   sync.Mutex
	day  market.Date
	list []manual.Contract
}

func (cs *contracts) get(ctx context.Context, cl *clients) ([]manual.Contract, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	today := market.DateOf(time.Now().In(market.IST))
	if cs.day == today && len(cs.list) > 0 {
		return cs.list, nil
	}
	uc, err := cl.upstox(ctx)
	if err != nil {
		return nil, fmt.Errorf("Nifty's contracts come from Upstox: %w", err)
	}
	ups, err := uc.LiveOptionContracts(ctx, ingest.IndexKeys["NIFTY"], market.Date{})
	if err != nil {
		return nil, fmt.Errorf("upstox contracts: %w", err)
	}
	var kites []kite.Instrument
	if kc, err := cl.zerodha(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "manual: no Kite symbols:", err)
	} else if kites, err = kc.Instruments(ctx, "NFO"); err != nil {
		fmt.Fprintln(os.Stderr, "manual: no Kite symbols:", err)
		kites = nil
	}
	fys, err := fyers.New("", "", nil).SymbolMaster(ctx, "NSE_FO")
	if err != nil {
		fmt.Fprintln(os.Stderr, "manual: no Fyers symbols:", err)
		fys = nil
	}
	list, err := manual.NiftyContracts(ups, kites, fys)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, errors.New("upstox lists no live Nifty options")
	}
	if kites != nil && fys != nil { // without Kite's or Fyers' symbols, try again on the next call
		cs.day = today
	}
	cs.list = list
	return list, nil
}

func runManual(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "setup" {
		return manualSetup(os.Stdin, os.Stdout)
	}
	fs := flag.NewFlagSet("manual", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8767", "loopback address to serve on (no password unless --public)")
	public := fs.String("public", "", "the host a proxy serves the page as over HTTPS (e.g. hearth.naveenkumar.dev): "+
		"needs SERENE_MANUAL_PASSWORD and SERENE_MANUAL_TOTP from serene manual setup, and SERENE_TOKEN_KEY")
	costsPath := fs.String("costs", "configs/costs.yaml", "charges schedule, for sizing and the target")
	useSlack := fs.Bool("slack", false, "post buys and square-offs to SLACK_CHANNEL")
	dryRun := fs.Bool("slack-dry-run", false, "Slack's futures buttons check the order on Kite and reply, but place nothing")
	wait := fs.Duration("limit-wait", 60*time.Second, "how long a LIMIT buy waits before what's left is cancelled")
	if err := fs.Parse(args); err != nil {
		return err
	}
	costs, err := cost.Load(*costsPath)
	if err != nil {
		return err
	}
	var notify func(context.Context, string)
	if *useSlack {
		c, channel, err := openSlackChannel(ctx, "SLACK_CHANNEL")
		if err != nil {
			return err
		}
		notify = func(ctx context.Context, text string) {
			if err := c.Post(ctx, channel, text); err != nil {
				fmt.Fprintln(os.Stderr, "slack:", err)
			}
		}
	}
	cl := &clients{at: map[string]time.Time{}}
	cs := &contracts{}
	desk := &manual.Desk{
		Broker: func(ctx context.Context, name string) (manual.Broker, error) {
			switch name {
			case "zerodha":
				k, err := cl.zerodha(ctx)
				if err != nil {
					return nil, err
				}
				return &manual.Zerodha{C: k}, nil
			case "upstox":
				u, err := cl.upstox(ctx)
				if err != nil {
					return nil, err
				}
				return &manual.Upstox{C: u}, nil
			case "fyers":
				f, err := cl.fyers(ctx)
				if err != nil {
					return nil, err
				}
				return &manual.Fyers{C: f}, nil
			case "icici":
				b, err := cl.icici(ctx)
				if err != nil {
					return nil, err
				}
				return &manual.ICICI{C: b, LTP: func(ctx context.Context, bc breeze.Contract) (float64, error) {
					list, err := cs.get(ctx, cl)
					if err != nil {
						return 0, err
					}
					for _, c := range list {
						if bc.Stock == "NIFTY" && c.Expiry == bc.Expiry && c.Strike == bc.Strike && c.Right == bc.Right {
							return upstoxLTP(ctx, cl, c)
						}
					}
					return 0, fmt.Errorf("no live Nifty %s %g %s", bc.Expiry, bc.Strike, bc.Right)
				}}, nil
			}
			return nil, fmt.Errorf("no broker %q", name)
		},
		Contracts: func(ctx context.Context) ([]manual.Contract, error) { return cs.get(ctx, cl) },
		LTP:       func(ctx context.Context, c manual.Contract) (float64, error) { return upstoxLTP(ctx, cl, c) },
		Costs:     costs,
		Notify:    notify,
		LimitWait: *wait,
	}
	logf := func(format string, a ...any) {
		fmt.Printf("%s "+format+"\n", append([]any{time.Now().In(market.IST).Format("15:04:05")}, a...)...)
	}
	var slackActions *manual.SlackActions
	if *useSlack {
		if slackActions, err = futButtons(cl, logf, *dryRun); err != nil {
			return err
		}
	}
	var gate *manual.Gate
	if *public != "" {
		if gate, err = manualGate(); err != nil {
			return err
		}
	}
	srv := &manual.Server{Addr: *addr, Public: *public, Gate: gate, Desk: desk, UI: web.Dist(), Log: logf}
	if slackActions != nil {
		srv.Slack = slackActions
		if *dryRun {
			fmt.Println("Slack futures buttons on POST /slack/actions (dry run: nothing is placed)")
		} else {
			fmt.Println("Slack futures buttons on POST /slack/actions (REAL Zerodha orders)")
		}
	}
	if *public != "" {
		fmt.Printf("Hearth on http://%s, served as https://%s behind its login (REAL orders)\n", *addr, *public)
	} else {
		fmt.Printf("Hearth on http://%s (REAL orders on Zerodha, Upstox, Fyers and ICICI; no password, so loopback only)\n", *addr)
	}
	return srv.Run(ctx)
}

// upstoxLTP is a contract's last price from Upstox.
func upstoxLTP(ctx context.Context, cl *clients, c manual.Contract) (float64, error) {
	u, err := cl.upstox(ctx)
	if err != nil {
		return 0, err
	}
	got, err := u.LTP(ctx, c.UpstoxKey)
	if err != nil {
		return 0, err
	}
	return got[c.UpstoxKey], nil
}

// manualGate is the public page's login from the environment. Its sessions
// are signed with a key derived from SERENE_TOKEN_KEY.
func manualGate() (*manual.Gate, error) {
	hash, totp, tokenKey := os.Getenv("SERENE_MANUAL_PASSWORD"), os.Getenv("SERENE_MANUAL_TOTP"), os.Getenv("SERENE_TOKEN_KEY")
	if hash == "" || totp == "" {
		return nil, errors.New("--public needs SERENE_MANUAL_PASSWORD and SERENE_MANUAL_TOTP: run serene manual setup")
	}
	if tokenKey == "" {
		return nil, errors.New("--public needs SERENE_TOKEN_KEY to sign its sessions")
	}
	if !strings.HasPrefix(hash, "pbkdf2-sha256:") {
		return nil, errors.New("SERENE_MANUAL_PASSWORD isn't a hash from serene manual setup")
	}
	secret, err := manual.ParseTOTPSecret(totp)
	if err != nil {
		return nil, fmt.Errorf("SERENE_MANUAL_TOTP: %w", err)
	}
	key := sha256.Sum256([]byte("serene manual session|" + tokenKey))
	return &manual.Gate{PasswordHash: hash, TOTP: secret, Key: key[:], Secure: true}, nil
}

// manualSetup asks for the public page's password, twice, and prints the
// .env lines for it and a fresh TOTP secret, with the link that adds the
// secret to an authenticator app.
func manualSetup(in *os.File, out io.Writer) error {
	echo := quiet(in)
	defer echo()
	r := bufio.NewReader(in)
	ask := func(prompt string) (string, error) {
		fmt.Fprint(out, prompt)
		line, err := r.ReadString('\n')
		fmt.Fprintln(out)
		if err != nil && line == "" {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	pw, err := ask("Password for the manual page (12+ characters): ")
	if err != nil {
		return err
	}
	again, err := ask("Again: ")
	if err != nil {
		return err
	}
	if pw != again {
		return errors.New("the passwords differ")
	}
	hash, err := manual.HashPassword(pw)
	if err != nil {
		return err
	}
	secret := manual.NewTOTPSecret()
	fmt.Fprintf(out, "\nAdd to deploy/hearth/server.env, then run deploy/hearth/env.sh and deploy:\n\n")
	fmt.Fprintf(out, "SERENE_MANUAL_PASSWORD=%s\nSERENE_MANUAL_TOTP=%s\n\n", hash, secret)
	fmt.Fprintf(out, "Add the TOTP to an authenticator app: type the key %s (time-based, 6 digits),\n", secret)
	fmt.Fprintf(out, "or make a QR of this link (e.g. qrencode -t ansiutf8 '<link>'):\n\n%s\n", manual.TOTPURI(secret, "manual"))
	return nil
}

// quiet turns off a terminal's echo while a password is typed; it returns the undo.
func quiet(f *os.File) func() {
	if st, err := f.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return func() {}
	}
	off := exec.Command("stty", "-echo")
	off.Stdin = f
	if off.Run() != nil {
		return func() {}
	}
	return func() {
		on := exec.Command("stty", "echo")
		on.Stdin = f
		on.Run()
	}
}
