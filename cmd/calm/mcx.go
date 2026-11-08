package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"strings"
	"time"

	"calm/internal/auth"
	"calm/internal/data"
	"calm/internal/kite"
	"calm/internal/mcx"
)

func runMCXFetch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcx fetch", flag.ContinueOnError)
	names := fs.String("names", "", "commodities, such as CRUDEOIL,GOLD; all when empty")
	daily := fs.Bool("daily", true, "fetch each commodity's continuous daily series")
	minute := fs.Bool("minute", true, "fetch 1-minute bars of the futures still trading")
	lookback := fs.Int("lookback-days", 500, "how far back to look for a new contract's first trade")
	dir := fs.String("data", "data/mcx", "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tokens, err := openTokens(ctx)
	if err != nil {
		return err
	}
	defer tokens.Close()
	token, err := auth.Load(ctx, tokens, "zerodha")
	if err != nil {
		return err
	}
	apiKey := os.Getenv("ZERODHA_API_KEY")
	if apiKey == "" {
		return errors.New("ZERODHA_API_KEY is not set")
	}
	lots, err := mcx.UpstoxLots(ctx)
	if err != nil {
		return err
	}
	store, err := data.Open(*dir)
	if err != nil {
		return err
	}
	defer store.Close()
	cfg := mcx.Config{Dir: *dir, Daily: *daily, Minute: *minute,
		Lookback: time.Duration(*lookback) * 24 * time.Hour, Log: marketLog}
	if *names != "" {
		cfg.Names = strings.Split(strings.ToUpper(*names), ",")
	}
	return mcx.Run(ctx, cfg, store, kite.New(apiKey, token.AccessToken), lots)
}
