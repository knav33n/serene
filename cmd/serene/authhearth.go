package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"serene/internal/auth"
)

// hearthEnv is hearth's .env, which holds the key hearth's token store is
// sealed with (deploy/hearth/env.sh uploads it).
const hearthEnv = "deploy/hearth/server.env"

// hearthBrokers are the logins hearth trades with: serene's signals, live
// and manual orders, and project_calm's worker.
var hearthBrokers = map[string]bool{"zerodha": true, "upstox": true, "fyers": true, "breeze": true}

// envValue reads one variable from a .env file without setting it.
func envValue(path, name string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if ok && strings.TrimSpace(k) == name {
			return strings.Trim(strings.TrimSpace(v), `"'`), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s has no %s", path, name)
}

// putArgs is a login as `serene auth put` takes it: the token sealed with
// hearth's key, so the command (which Systems Manager keeps in its history)
// never carries it in the clear.
func putArgs(t auth.Token, hearthKey string) ([]string, error) {
	sealer, err := auth.NewSealer(hearthKey)
	if err != nil {
		return nil, fmt.Errorf("hearth's SERENE_TOKEN_KEY: %w", err)
	}
	sealed, err := sealer.Seal(t.Broker, t.AccessToken)
	if err != nil {
		return nil, err
	}
	return []string{t.Broker, t.UserID, t.IssuedAt.UTC().Format(time.RFC3339), t.ExpiresAt.UTC().Format(time.RFC3339),
		base64.StdEncoding.EncodeToString(sealed)}, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func awsCLI(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "aws", args...)
	cmd.Env = os.Environ()
	if os.Getenv("AWS_PROFILE") == "" {
		cmd.Env = append(cmd.Env, "AWS_PROFILE=iac")
	}
	if os.Getenv("AWS_REGION") == "" {
		cmd.Env = append(cmd.Env, "AWS_REGION=ap-south-1")
	}
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("aws %s: %s", args[0], strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// pushHearth stores a login on hearth, through Systems Manager like
// deploy/hearth/deploy.sh: hearth's serene opens it with its own key, keeps
// it and writes it into hearth's project_calm.
func pushHearth(ctx context.Context, t auth.Token) error {
	key, err := envValue(hearthEnv, "SERENE_TOKEN_KEY")
	if err != nil {
		return err
	}
	args, err := putArgs(t, key)
	if err != nil {
		return err
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	script := "cd /home/serene/app && sudo -u serene ./serene auth put " + strings.Join(quoted, " ")
	instance, err := awsCLI(ctx, "ec2", "describe-instances", "--filters", "Name=tag:Name,Values=hearth",
		"Name=instance-state-name,Values=running", "--query", "Reservations[].Instances[].InstanceId", "--output", "text")
	if err != nil {
		return err
	}
	if !strings.HasPrefix(instance, "i-") || strings.ContainsAny(instance, " \t\n") {
		return fmt.Errorf("hearth isn't running (found %q)", instance)
	}
	params, _ := json.Marshal(map[string][]string{"commands": {script}})
	id, err := awsCLI(ctx, "ssm", "send-command", "--instance-ids", instance, "--document-name", "AWS-RunShellScript",
		"--comment", "serene auth put "+t.Broker, "--parameters", string(params), "--query", "Command.CommandId", "--output", "text")
	if err != nil {
		return err
	}
	for range 40 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
		status, _ := awsCLI(ctx, "ssm", "get-command-invocation", "--command-id", id, "--instance-id", instance,
			"--query", "Status", "--output", "text")
		switch status {
		case "", "Pending", "InProgress", "Delayed":
			continue
		case "Success":
			return nil
		}
		msg, _ := awsCLI(ctx, "ssm", "get-command-invocation", "--command-id", id, "--instance-id", instance,
			"--query", "StandardErrorContent", "--output", "text")
		return fmt.Errorf("on hearth: %s: %s", status, msg)
	}
	return errors.New("hearth didn't answer in 80 s")
}

// authPut is `serene auth put BROKER USER ISSUED EXPIRES SEALED` on hearth:
// it opens the sealed token with this machine's key, stores it, and writes it
// into project_calm (CALM_DATABASE_URL).
func authPut(ctx context.Context, store auth.Store, args []string) error {
	if len(args) != 5 {
		return errors.New("usage: serene auth put BROKER USER ISSUED EXPIRES SEALED")
	}
	sealer, err := auth.NewSealer(os.Getenv("SERENE_TOKEN_KEY"))
	if err != nil {
		return err
	}
	sealed, err := base64.StdEncoding.DecodeString(args[4])
	if err != nil {
		return errors.New("the sealed token isn't base64")
	}
	tok, err := sealer.Open(args[0], sealed)
	if err != nil {
		return err
	}
	t := auth.Token{Broker: args[0], UserID: args[1], AccessToken: tok}
	if t.IssuedAt, err = time.Parse(time.RFC3339, args[2]); err != nil {
		return err
	}
	if t.ExpiresAt, err = time.Parse(time.RFC3339, args[3]); err != nil {
		return err
	}
	if !t.Valid(time.Now()) {
		return errors.New("that login has expired")
	}
	if err := store.Put(ctx, t); err != nil {
		return err
	}
	fmt.Printf("%s: stored %s's login until %s\n", t.Broker, t.UserID, t.ExpiresAt.Format(time.RFC3339))
	return exportCalm(ctx, t)
}

// exportCalm writes a login into project_calm when CALM_DATABASE_URL is set.
func exportCalm(ctx context.Context, t auth.Token) error {
	url := os.Getenv("CALM_DATABASE_URL")
	if url == "" {
		return nil
	}
	ok, err := auth.ExportCalm(ctx, url, t)
	if ok {
		fmt.Printf("%s: written to project_calm\n", t.Broker)
	}
	return err
}

// afterLogin hands a new login on: to project_calm here, then to hearth for
// the brokers it trades with (unless toHearth is false), reporting each place
// as it starts and as it ends.
func afterLogin(ctx context.Context, store auth.Store, broker string, toHearth bool, report func(auth.Delivery)) error {
	t, err := store.Get(ctx, broker)
	if err != nil {
		return err
	}
	var errs []error
	send := func(to string, fn func() error) {
		report(auth.Delivery{To: to, State: "sending"})
		if err := fn(); err != nil {
			report(auth.Delivery{To: to, State: "failed", Error: err.Error()})
			errs = append(errs, fmt.Errorf("%s: %w", to, err))
			return
		}
		report(auth.Delivery{To: to, State: "ok"})
	}
	if os.Getenv("CALM_DATABASE_URL") != "" {
		send("calm", func() error { _, err := auth.ExportCalm(ctx, os.Getenv("CALM_DATABASE_URL"), t); return err })
	}
	if toHearth && hearthBrokers[broker] {
		send("hearth", func() error { return pushHearth(ctx, t) })
	}
	return errors.Join(errs...)
}

// printDelivery is a report for the terminal.
func printDelivery(broker string) func(auth.Delivery) {
	return func(d auth.Delivery) {
		switch d.State {
		case "ok":
			fmt.Printf("%s: sent to %s\n", broker, d.To)
		case "failed":
			fmt.Fprintf(os.Stderr, "%s: not sent to %s: %s\n", broker, d.To, d.Error)
		}
	}
}
