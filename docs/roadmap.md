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
| - | `calm market`: daily index store, sector report, regime, Slack | Done |
| - | Seams for paper and live: holdout, decision log, order lifecycle, risk gate, resume | Done |
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
- Untraded minutes still get a candle, carrying the last price with zero volume:
  about half of all bars. The simulator never fills on them. At-the-money
  front-week strikes almost always trade, so the default straddle is unchanged.
- Expiries settle at NSE's official close (`settlement: data/market`). Before the
  closing auction it differed from the last 1-minute index bar by a median 15
  and up to 120 points; holding the straddle to expiry nets 7% less with it.
- Kite has no candles for expired options, so Upstox is the historical source.
- Rate limits for these endpoints are inferred: 2,000 per 30 minutes is the
  tightest, and the fetcher paces to it.
- Each day file holds every strike of every live expiry, so backtests over real
  data will load far more bars than synthetic ones. Filtering at load time is the
  likely next speed-up.

## Daily market report

Source: NSE's `ind_close_all_DDMMYYYY.csv`, with 167 indices against 139 on Upstox
and 136 on Zerodha, plus P/E, P/B and volume, and no login. Findings from the files:

- Published about 23:20 IST, so the report runs the next morning. A file can be
  rewritten later, so each run rechecks the newest five.
- History starts 2012-02-21. Files are missing for 2014-12-15, 2015-03-12 and
  2015-07-08, which were trading days.
- niftyindices.com answers a missing file with 200 and an HTML page, so the NSE
  archive, which sends a real 404, is tried first.
- Names changed many times; `configs/indices.yaml` holds the aliases, and
  `calm market check` proves them. `Nifty Full Midcap 100` is a different series
  from `NIFTY Midcap 100`.
- The 2016-07-07 and 2023-04-12 files hold back-calculated rows for indices launched
  years later, so a gap over 10 days restarts a series.
- 2014 and 2015 files use slash dates, and three April 2023 files are month first.
- Nifty High Beta 50 was rebased ×2 on 2014-08-04, which is adjusted out using the
  published previous close.
- NSE's P/E basis changed on 2021-03-31 (Nifty 500 fell from 43.3 to 35.9
  overnight), so percentiles start after it.

Regime rule change: the planned rule had a single line at +4 and -4, which gave 63
state changes since 2012. The exit rule (+1 and -1) cut that to 39, at the cost of
leaving Bull later. It was chosen to stop flipping near the line, not to fit a
particular fall.

## Seams for paper and live

Built from a review of seven open-source trading systems (sklinkert/at,
simple-trader, goquant, india-trade-cli, algorithmic-trading-engine,
kite-algo-bot, auto-trader-bot) on 2026-11-07. Live orders will go to both
Zerodha and Upstox, so none of this is specific to one broker.

- Research holdout and trial ledger: `configs/holdout.yaml`, `--unseal`,
  `<data_dir>/trials.jsonl`.
- Every order decision is recorded with a reason; reports group refusals.
- Report manifests: commit, config and cost hashes.
- `internal/oms`: per-leg states, including `Unknown` for an order sent with no
  known outcome. Client IDs are letters and digits, at most 20 characters.
- `broker.Broker` has `Cancel`; `broker.Account` (positions, orders) and
  `broker.Funds` are separate. `internal/broker/brokertest` is the contract
  every adapter must pass; the simulator passes it with both fill models.
- `sim.FillModel`: `BarOpen` for backtests, `QuoteFill` (ask for buys, bid for
  sells, stale quotes refused) for paper.
- `internal/risk`: daily loss over the IST day net of charges, lot and rate
  limits, stale quotes, a limit-price collar, and a manual halt. Exits are never
  refused, by the gate or by margin.
- `engine.Config.Resume`: a restart replays the journal through the strategy and
  refuses to continue if the strategy disagrees or an order was left open.

Not built: a strategy warm-up interface, since neither strategy needs history
from before the day.

## Rejected strategies

A strategy that fails its test keeps its code and configs and gets a write-up in
`docs/strategies/rejected/`. Add it to `strategies.Rejected` so research runs
warn, and leave its trial ledger entries alone, since they are what later trial
counts are measured against. Delete the code only when it gets in the way, in a
commit that points to the write-up.

- `lower_high_break`: failed its holdout on 2026-11-08.

## Step 6: Paper

Kite ticker feed, simulated fills on live bid/ask, an append-only JSONL order
journal, and recording live ticks to Parquet to calibrate slippage. Includes a
parity test: the same strategy on recorded data must produce identical orders
through the backtest and paper paths.

Decided:

- The journal is JSONL, fsynced before each order is sent, with a `flock` so two
  runs can't trade at once. Trading must not depend on Postgres or Redis.
- Startup reads the journal back into positions, today's loss and any halt, then
  resumes through `engine.Config.Resume`. A mismatch with the broker's
  `Account` refuses to start.
- Recorded ticks live apart from `data/upstox` and `data/market` and are never
  merged into them; replays are marked as such.
- `calm check` before the open and after the close: token valid through the
  close, trading day, data fresh, journal lock free, `.env` mode 600, disk, and
  that the run started. It reports to Slack and never acts.

## Step 7: Live

Kite order adapter, then Upstox. Live needs both an explicit flag and config.

- Pre-trade limits on lots, daily loss and order rate, plus a kill switch.
- On startup, reconcile positions with the broker and refuse to trade on a mismatch.
- Refuse to start unless `Token.ValidThrough` covers 15:30 IST.
- Run a strategy on paper first, then start at minimum size.
- Test against a fake broker: rejections, disconnects, position mismatches.
  The fake and both adapters run `internal/broker/brokertest`.
- Never retry order placement automatically. A timeout leaves the order
  `Unknown`; look it up by client ID and halt entries until it is resolved.
  Reads may retry.
- If either live switch is missing, refuse to start. Never fall back to paper
  quietly.
- Two-level kill: halt stops entries, flatten closes positions. A position is
  closed only on a confirmed fill; if the broker API is down, halt and alert.
- A structure that fills some legs and not others (`oms.Order.Broken`) is
  flattened and alerted. So is a position whose protective stop can't be placed.
- Each adapter maps `market.Product` and broker statuses to calm's own; an
  unrecognised status becomes `Unknown`, never `Filled`. Check each broker's tag
  limit, stop types for options, and any idempotency support before building.

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
