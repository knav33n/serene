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
| 4 | Upstox ingest and coverage report | Built for NIFTY |
| 5 | Sweeps, walk-forward, example strategies | Done |
| 6 | Paper mode: Kite ticker, live fills, journal, tick recording | Not started |
| 7 | Live mode: order adapters, risk layer, reconciliation | Not started |

## Step 4: Upstox ingest

- `calm research fetch`: resumable, repeatable, paced to 1.1 requests per second.
- `calm research coverage`: reports what's missing by expiry and contract.
- Lot sizes come from each contract, never hardcoded.
- Scope so far: NIFTY only, 1-minute bars, strikes within 5% of the index's range
  in the 30 days before expiry. BANKNIFTY needs its Upstox key adding.

Findings from real responses, some of which contradict the docs:

- Expired-instrument APIs need the Upstox Plus plan (`UDAPI1149` otherwise).
- NIFTY expiries were available back to 2024-10-03 when checked on 2026-11-06,
  not the six months the docs state. The window may roll, so keep fetching.
- One expired-candle request can cover a contract's whole life, but returns
  HTTP 500 at about 100,000 candles (seen on the Dec 2025 expiry, with 91,672
  still fine), so `ExpiredCandles` splits a window that fails. Index candles are
  limited to one month per request.
- The 2024-12-26 monthly expiry lists 281 contracts, but Upstox returned no
  candles for the four checked over any window, and none in the full download.
  Its manifest is kept, so a fetch won't ask again. `calm research coverage`
  lists it under empty expiries.
- Expired-candle lists don't include the unexpired series, so the current week's
  contracts arrive after they expire.
- Since 2026-08-03 derivatives close at 15:40 and the cash market has a closing
  auction: the index freezes around 15:15 and jumps to the close near 15:28.
  `market.NSESession` handles the date.
- Developers report gaps in expired data, so measure coverage.
- Kite has no candles for expired options, so Upstox is the historical source.
- Rate limits for these endpoints are inferred: 2,000 per 30 minutes is the
  tightest, and the fetcher paces to it.
- Each day file holds every strike of every live expiry, so backtests over real
  data will load far more bars than synthetic ones. Filtering at load time is the
  likely next speed-up.

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
