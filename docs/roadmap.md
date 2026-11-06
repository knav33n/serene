# Roadmap

Goal: one Go system with two modes. Research pulls data, backtests and
validates. Prod runs the same strategy on paper or live. The first target is
NIFTY and BANKNIFTY options; Indian equities and MCX commodities follow.

| Step | Scope | Status |
|---|---|---|
| 1 | Domain types, Parquet store, resampler | Done |
| 2 | Engine, simulator, portfolio, settlement | Done |
| 3 | Costs, margin, Greeks, metrics, report | Done |
| - | `calm auth`: broker login, tokens in Postgres | Done, used with real Upstox and Zerodha |
| 4 | Upstox ingest and coverage report | Next |
| 5 | Sweeps, walk-forward, example strategies | Done |
| 6 | Paper mode: Kite ticker, live fills, journal, tick recording | Not started |
| 7 | Live mode: order adapters, risk layer, reconciliation | Not started |

## Step 4: Upstox ingest

- `calm research fetch`: resumable, repeatable, within rate limits.
- `calm research coverage`: reports what's missing by expiry and contract.
- Lot sizes come from each contract, never hardcoded.
- Default scope: NIFTY and BANKNIFTY, strikes within ±5% of spot, 1-minute bars.

Findings:

- Expired-instrument APIs need the Upstox Plus plan (`UDAPI1149` otherwise).
- Expiries cover "up to six months", so run fetch regularly to build up history.
- Developers report gaps in expired data, so measure coverage.
- Index minute candles go back to January 2022 on all plans.
- Kite has no candles for expired options, so Upstox is the historical source.
- Unconfirmed: maximum date range per expired-candle request, and rate limits for
  these endpoints (probably 50 per second, 500 per minute, 2,000 per 30 minutes).

## Step 6: Paper

Kite ticker feed, simulated fills on live bid/ask, an append-only JSONL order
journal, and recording live ticks to Parquet to calibrate slippage. Includes a
parity test: the same strategy on recorded data must produce identical orders
through the backtest and paper paths.

## Step 7: Live

Kite order adapter, then Upstox. Live needs both an explicit flag and config.

- Pre-trade limits on lots, daily loss and order rate, plus a kill switch.
- On startup, reconcile positions with the broker and refuse to trade on a mismatch.
- Refuse to start unless `Token.ValidThrough` covers 15:30 IST.
- Run a strategy on paper first, then start at minimum size.
- Test against a fake broker: rejections, disconnects, position mismatches.

Constraints:

- SEBI retail algo rules from April 2026: whitelisted static IP, exchange order
  tagging, registration above 10 orders per second per exchange. Check the primary
  circulars before going live.
- Upstox rejects orders from an unregistered IP (`UDAPI1154`). The IP can change
  once a week, and changing it invalidates tokens.
- `192.168.31.98` is a private address, so a trading machine outside that network
  needs a VPN or its own database.
- `dbadmin` is an admin login. Use a role limited to the `calm` schema before
  going live.
- On a headless server, log in with `ssh -L 8080:127.0.0.1:8080 server`.
