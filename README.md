# calm

Research and trade Indian index options in Go. Strategies are written once and
run unchanged in backtests, and later on paper and live.

**Status:** research works on synthetic data, and broker login works. Real data
download, paper trading and live trading are not built yet, and those commands
only return an error. See `docs/roadmap.md`.

## Quick start

Requires Go 1.27 and a C compiler, because the DuckDB driver uses CGO.

```sh
go build -o calm ./cmd/calm
./calm research synth --from 2026-08-03 --to 2026-11-30
./calm research backtest --config configs/run.yaml      # writes reports/*.html
./calm research sweep --config configs/sweep.yaml
./calm research walkforward --config configs/sweep.yaml
```

Synthetic data is a fairly priced random walk, so no strategy has an edge on
it. Use it to test the system, never to judge an idea.

## Broker login

Upstox and Zerodha tokens expire every morning (03:30 and 06:00 IST), so log in
once a day. Tokens can place real orders, so they're encrypted with
`CALM_TOKEN_KEY` before being stored in Postgres, and never shown in the page,
terminal or logs.

Setup:

1. Copy `.env.example` to `.env` and fill it in.
2. Register a redirect URL with each broker's app. With these, a login finishes
   by itself while `calm auth` is running:
   - `http://127.0.0.1:8080/callback/upstox`
   - `http://127.0.0.1:8080/callback/zerodha`

   You can use another URL if you can copy the token from it. This project's
   Upstox app uses `https://httpbin.org/get`. For Upstox, set
   `UPSTOX_REDIRECT_URI` to exactly the registered value, because it's sent with
   every login. A mismatch gives `UDAPI100068`, and `calm auth` prints the value
   it's using so you can compare.

   Login codes pass through httpbin, a third party, so switch to the local URL
   before live trading.

Daily use:

```sh
./calm auth                  # opens the login page
./calm auth status           # shows each login, never the token
./calm auth logout upstox    # revokes the token at the broker and deletes it
```

On the page, **Log in** shows the broker's URL and a box. Open the URL, log in,
then paste the token from the redirected URL (`code` for Upstox, `request_token`
for Zerodha), or paste the whole URL. Paste it quickly: these tokens are short
lived, and Upstox's works only once.

Code gets a token with `auth.Load(ctx, store, "upstox")`. Trading should also
check `Token.ValidThrough` so a token can't expire during the session.

The page is in `web/`. `web/dist` is committed, so `go build` doesn't need Node.
After changing it, run `npm run build` in `web/`. To run the Postgres tests, set
`CALM_TEST_DATABASE_URL`; they use a temporary schema.

## Writing a strategy

Implement `strategy.Strategy` in `internal/strategies`, and add it to the
registry in `strategies.go`. `straddle.go` shows multi-leg selling, and
`breakout.go` shows using several timeframes.

- `OnBar` only ever receives closed bars.
- Orders fill at the next minute's open, plus slippage.
- `ctx.Chain` returns only contracts that have traded today.
- Close positions yourself. Anything open at expiry cash-settles at intrinsic
  value.

## Reading results

- **Slippage table:** bar data has no bid/ask, so fills are assumed. Distrust
  any result that fails at the 2× or 3× rows.
- **Sweeps** are in-sample and show the best of many tries. Only walk-forward
  results were not used to choose parameters.
- **Margin** is an estimate, not SPAN.
- **Multi-leg fills** are simultaneous, so legging risk is not modelled.
- **Order size** is not limited by traded volume.

## Costs

`configs/costs.yaml` holds dated charges, and backtests stop with an error on
dates before its first period (2024-10-01). Add older rates to test earlier
history.
