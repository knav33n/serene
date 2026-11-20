# serene

Research and trade Indian index options in Go. Strategies are written once and
run unchanged in backtests, and later on paper and live.

**Status:** research works on synthetic data and real NIFTY options, broker
login works, and `serene manual` trades Nifty options by hand. Paper trading and live trading are not built yet, and those commands
only return an error. See `docs/roadmap.md`.

## Quick start

Requires Go 1.27 and a C compiler, because the DuckDB driver uses CGO.

```sh
go build -o serene ./cmd/serene
./serene research synth --from 2026-08-03 --to 2026-11-30
./serene research backtest --config configs/run.yaml      # writes reports/*.html
./serene research sweep --config configs/sweep.yaml
./serene research walkforward --config configs/sweep.yaml
```

Synthetic data is a fairly priced random walk, so no strategy has an edge on
it. Use it to test the system, never to judge an idea.

## Broker login

Upstox, Zerodha and ICICI Breeze tokens expire every morning (03:30 and 06:00
IST), so log in once a day. Tokens can place real orders, so they're encrypted
with `SERENE_TOKEN_KEY` before being stored in Postgres, and never shown in the
terminal or logs. `serene auth` serves the login page on 127.0.0.1:8080 and opens it.

serene is the only place logins are made. project_calm no longer logs in: each
login made here is also written into calm's `trading.broker_sessions`
(`CALM_DATABASE_URL`, in the clear as calm keeps it), and Zerodha's and Upstox's
are pushed to hearth, which stores them and writes them into hearth's calm.

Setup:

1. Copy `.env.example` to `.env` and fill it in.
2. Register a redirect URL with each broker's app. With these, a login finishes
   by itself while the page is open:
   - `http://127.0.0.1:8080/callback/upstox`
   - `http://127.0.0.1:8080/callback/zerodha`
   - `http://127.0.0.1:8080/callback/breeze` (Breeze sends `apisession`)

   You can use another URL if you can copy the token from it. This project's
   Upstox app uses `https://httpbin.org/get`. For Upstox, set
   `UPSTOX_REDIRECT_URI` to exactly the registered value, because it's sent with
   every login. A mismatch gives `UDAPI100068`, and `serene auth` prints the value
   it's using so you can compare.

   Login codes pass through httpbin, a third party, so switch to the local URL
   before live trading.

Daily use:

```sh
./serene auth                  # the login page (--no-open: don't open the browser); Ctrl+C stops it
./serene auth status           # shows each login, never the token
./serene auth logout upstox    # revokes the token at the broker and deletes it
./serene auth push zerodha     # sends a stored login to hearth again (--hearth=false: calm here only)
./serene auth --hearth=false   # logs in without pushing to hearth
```

The page says in one line which brokers still need today's login, and lists
each one: who's logged in, how long the token has left (the bar under it), and
where the login was sent. **Log in** opens the broker's page in a new tab. With a
local redirect URL the login finishes by itself and that tab comes back to the
page; otherwise paste the token from the redirected URL (`code` for Upstox,
`request_token` for Zerodha, `apisession` for Breeze), or the whole URL, into
the broker's row and press **Finish login**. Paste it quickly: these tokens are
short lived, and Upstox's works only once. A Zerodha login between 06:00 and
07:30 IST is refused before the token is spent: Zerodha's daily logout can still
wipe it.

After a Zerodha or Upstox login, serene pushes it to hearth through Systems
Manager (AWS profile `iac`, `ap-south-1`): it seals the token with hearth's own
`SERENE_TOKEN_KEY` (from `deploy/hearth/server.env`), so the command, which
Systems Manager keeps in its history, never carries the token in the clear.
hearth runs `serene auth put` with it, which stores it and writes it into
hearth's calm. A push that fails leaves the local login in place and says to
retry with **Send again** on the page, or `serene auth push`. Breeze and Fyers stay on this machine.

Code gets a token with `auth.Load(ctx, store, "upstox")`. Trading should also
check `Token.ValidThrough` so a token can't expire during the session.

To run the Postgres tests, set `SERENE_TEST_DATABASE_URL`; they use a temporary
schema.

## Manual trades

`serene manual` serves the manual trades page that used to be project_calm's
(`internal/manual`, the page in `web/`). It places **real orders** on Zerodha,
Upstox, Fyers and ICICI (Breeze), using the day's logins. Fyers and ICICI start
unticked. ICICI takes no market orders: "at market" there is a LIMIT 5% through
the LTP. Fyers has no charges API, so its rows show P&L without charges.

- **Trade a Nifty strike.** Pick the expiry, strike, CE / PE, a limit price
  (empty: at market), the target as a % net of charges, the capital, and the
  brokers. Each broker buys as many lots as the capital covers, or its free
  margin when that's less, after the broker's charges. The buy is split at the
  freeze quantity, into at most 10 orders. What hasn't filled at the limit in
  `--limit-wait` (60 s) is cancelled. The target is a resting LIMIT sell at the
  price that nets the target after charges both ways. There's no stop, and the
  product is intraday (MIS / I). At 15:20 the page squares off whatever manual
  position is still open while it runs, and the broker does it if the page
  isn't running.
- **Open F&O positions** on both accounts, whoever opened them. **Exit**
  cancels the contract's pending orders, then squares it off at market.
- **Today's trades**, read live from each broker's tradebook. Each position has
  its orders, the broker's P&L, its own charges (Kite's contract note, Upstox's
  brokerage API) and net. **By** names the strategy from the order tag, with
  `serenemanual` shown as manual and `hmr…` as nifty_hma_retest. A manual
  trade shows its target and a **Sell now** button.

Nothing is stored: a manual trade is a position whose orders carry the
`serenemanual` tag. Nifty's contracts and the at-market LTP come from Upstox,
and Kite's trading symbols from its instrument list.

Locally there's no password. The server listens only on a loopback address and
refuses anything else. It checks the Host header, and only takes JSON POSTs from
its own origin.

On hearth it's the home page, **https://hearth.naveenkumar.dev/**, behind a
login: a password and a TOTP code from an authenticator app. `serene-manual`
runs `serene manual --public=hearth.naveenkumar.dev --slack`, and host.sh points
Caddy's unclaimed paths at it. The session cookie lasts until 06:00 IST. Five
wrong tries lock the login for a minute, doubling up to 16. To set it up:

1. `./serene manual setup` asks for the password and prints
   `SERENE_MANUAL_PASSWORD` (a PBKDF2 hash) and `SERENE_MANUAL_TOTP`. Add the
   TOTP key to the authenticator app.
2. Put both, with `FYERS_APP_ID`, `BREEZE_API_KEY` and `BREEZE_API_SECRET`, in
   `deploy/hearth/server.env`, then run `deploy/hearth/env.sh` and
   `deploy/hearth/deploy.sh`.
3. Each morning `serene auth` pushes the Zerodha, Upstox, Fyers and Breeze
   logins to hearth (`serene auth push` again if one was missed).

Changing the password, or SERENE_TOKEN_KEY, ends every session.

```sh
./serene manual                # http://127.0.0.1:8767
./serene manual --slack        # also posts buys and square-offs to SLACK_CHANNEL
./serene manual setup          # the public page's password and TOTP
```

The page is in `web/`. `web/dist` is committed, so `go build` doesn't need Node.
After changing it, run `npm run build` in `web/` (`npm test` runs its tests).

## Real data

Downloads expired NIFTY options from Upstox into `data/upstox`. It needs the
Upstox Plus plan and a login from today.

```sh
./serene research fetch       # safe to rerun: finished expiries are skipped
./serene research coverage    # shows what's missing
```

A full download takes hours. If the token expires or the job stops, run
`serene auth`, then the same command, and it carries on. To backtest on this data,
set `data_dir: data/upstox` and `settlement: data/market` in your config, so
expiries settle at NSE's official close.

Data from 2026-05-01 is held back (`configs/holdout.yaml`): research runs that
reach it are refused. Keep it for one final check of a strategy whose design is
frozen, with `--unseal`, which is recorded. Every run is also logged to
`data/upstox/trials.jsonl`, and reports show how many parameter sets have been
tried for that strategy.

## Fyers data

Fyers holds expired options and futures as 1-minute bars with OI: NIFTY and
stocks from 2018-10-25, FINNIFTY from 2021-01, MIDCPNIFTY from 2022-01,
NIFTYNXT50 from 2024-05, BANKEX from 2023-07-28 and SENSEX from 2023-08.
Everything goes to `~/INSIGHTS/fyers`
(`configs/fyers.yaml`), in the same Parquet layout as `data/upstox`. It needs
the Prime plan; `plan: standard` keeps to Standard's caps instead. Set
`FYERS_APP_ID`, `FYERS_SECRET` and `FYERS_REDIRECT_URI` in `.env`, then log in
with `serene auth`.

```sh
./serene fyers lots       # NSE/BSE F&O bhavcopies -> lots, strikes, prices, universe.csv
./serene fyers all        # everything, for days: indices then every F&O stock since 2018
./serene fyers probe      # what history Fyers really serves (probe.parquet)
./serene fyers index      # 1m and daily spot bars from 2017-10
./serene fyers backfill   # expired options and futures, newest expiry first; safe to rerun
./serene fyers update     # daily after the close, after fyers lots (stocks need its fresh prices)
```

`fyers lots` reads what Fyers doesn't say from the exchanges' daily F&O files,
kept under `bhav_fo/`:

- **Lots.** UDiFF files (NSE from 2024-07-08, BSE from 2024-01) state each
  contract's lot. Before that, NSE's market activity report gives each future's
  traded quantity and contracts, whose ratio is the lot (`nse_mkt`), and weekly
  options take their monthly's. A turnover estimate (`nse_derived`) is the last
  resort, and SENSEX 10 / BANKEX 15 are assumed before BSE's archive (`manual`).
  Upstox's exact lots win where `data/upstox` has them. Every lot row records
  its source.
- **Universe.** `universe.csv` lists every stock with options on any day since
  2018-10, including those since dropped or delisted, which `stocks_file` adds
  to the download. Fyers no longer lists a delisted stock (HDFC, MINDTREE,
  CADILAHC), but it still serves the contracts by ticker, so their expiries
  and strikes come from the bhavcopy (`"source": "bhavcopy"` in the manifest).
- **Prices.** Fyers' stock spot history is split-adjusted (IRCTC's 2021 spot
  reads 740 against a future at 3,700), so a stock's strikes are chosen around
  its unadjusted near-month future close from the bhavcopy. The stored stock
  spot bars are still Fyers' adjusted ones; use the futures for an unadjusted
  underlying.

`fyers all` sleeps to the next day when the daily budget is spent. When the
token expires (06:30 IST daily) it posts to Slack (`SLACK_TOKEN`,
`SLACK_CHANNEL`) and resumes once `serene auth` has stored a new token. The
client stays under the plan's rate limits and stops on a rate-limit error,
because Fyers blocks the account for the day after repeated breaches; daily
usage is kept in `fetch/usage.json`. An expiry with no known lot is skipped and
left unfinished; `serene fyers lot --underlying NAME --from DATE --size N` adds
one. Stocks are stored under a safe name (`M&M` as `M_M`). To backtest, set
both `data_dir` and `settlement` to the Fyers directory. Expiries then settle at
Fyers' daily index close, which is also the only settlement source for SENSEX.

## ETF data

Daily and 1-minute history for every ETF traded on NSE, into `~/INSIGHTS/etf`,
grouped by the country of the fund's holdings (`us`, `hongkong`, `india`). It
needs a Kite or Upstox login from today, ideally both, and the bhavcopy store
from `serene equities fetch`.

```sh
./serene etf fetch                 # all of them; the first run takes hours, later ones bring files up to date
./serene etf fetch --symbols MON100,MAFANG
```

Files are `<country>/daily/<SYM>.parquet` and `<country>/1m/<SYM>.parquet`, with
prices in rupees, split-adjusted, and a `source` column. `universe.csv` lists each
ETF with every symbol and ISIN it has traded under, and `coverage.csv` what each
file holds. Kite only has a fund's history since its last rename, so the rest comes
from the bhavcopy (daily, adjusted for splits, `source = bhav_adj`) and from Upstox,
which keys by ISIN (1-minute, from 2022). Before 2022 a renamed fund's 1-minute
bars start at the rename. Windows already downloaded are kept in `<country>/raw/`,
so a stopped run carries on where it was.

## Backup

All data lives on the local disk: `data/` (equities, market, MCX from Kite,
Upstox, NSE, live and paper state) and `~/INSIGHTS/{fyers,etf}`. At the end
of the day, plug in the Seagate drive and mirror it there:

```sh
./serene backup --dry-run   # what would be copied or deleted
./serene backup             # data/ -> MARKET_DATA, ~/INSIGHTS/{fyers,etf} -> INSIGHTS/{fyers,etf}
```

It is an exact mirror (`rsync --delete`): files deleted locally go from the
drive too. It refuses to run if the drive is not mounted, or if a source is
missing, empty, or itself on the drive. Folders are in `configs/backup.yaml`.

## Daily market report

Stores every Nifty index from NSE's daily file and reports which sectors are
moving and what regime the market is in. It needs no broker login.

```sh
./serene market fetch               # backfills from 2012, safe to rerun
./serene market report              # writes reports/market/DATE.html
./serene market check               # data integrity and regime history
./serene market daily --slack       # fetch, report and post once per close
```

NSE publishes the file about 23:20 IST. To run it at 09:00 IST on Tuesday to
Saturday, and at next boot if the machine was off:

```sh
cp deploy/serene-market.* ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now serene-market.timer
```

Slack needs `SLACK_TOKEN` and `SLACK_CHANNEL` in `.env`, and the app needs
`chat:write` and `files:write`. Each close posts one message: a chart card and the
full HTML report. The card is rendered with headless Chrome or Chromium, and if that
fails the summary goes out as text with an alert. Other failures post to Slack too:
to `SLACK_ALERT_CHANNEL` if it is set, as they do for every job, else to `SLACK_CHANNEL`.

The regime is **Bull**, **Neutral** or **Bear**, from seven checks on Nifty 500
scored -7 to +7. It enters Bull at +4 and Bear at -4, leaves at +1 and -1, and
changes only after 5 straight closes.

Read it as a description. It lags turns: in four of the five big falls since 2015
it left Bull 9-11% below the peak, and it confirmed Bear late, at -37% in March 2020 and on
the low in February 2025. Bear days were followed by gains more often than not, so
it's no signal to sell. Thresholds were set before looking at results, apart from
the exit rule, and five falls can't validate them.

## Stock prices

Stores NSE's daily bhavcopy: open, high, low, close, volume and turnover for every
security traded, from the first session on 1994-11-03. It needs no broker login.

```sh
./serene equities fetch             # backfills from 1994, safe to rerun
./serene equities adjust            # finds splits and bonuses, needs an Upstox login
./serene equities check --adjusted  # compares with Upstox daily candles
./serene equities daily             # fetch, then confirm any new splits or bonuses
```

Raw zips are kept in `data/equities/raw/` and rebuilt into
`data/equities/stocks/year=YYYY.parquet`, with prices in paise as traded. Stocks
are in series EQ, BE, BZ, SM and ST; the file also holds bonds, gold bonds and units.

Prices aren't adjusted for corporate actions, and NSE's previous close isn't
either, so `adjust` compares each listed stock with Upstox's adjusted daily
candles: their ratio steps on each ex-date. A step that is a split or bonus
ratio, such as 1/2 for a 1:1 bonus or 1/5 for a split from ₹10 to ₹2, is
snapped to it exactly. Any other step of 3% or more is kept as `other`: a
demerger, a rights issue, or a dividend large enough for Upstox to adjust, as
with Coal India's in November 2023. Results go to `data/equities/adjust/events.parquet` and `factors.parquet`:

```sql
-- factor_all also counts the other events; a stock with none has no rows
SELECT s.date, s.close / 100 * coalesce(f.factor, 1) AS close
FROM 'data/equities/stocks/*.parquet' s
LEFT JOIN 'data/equities/adjust/factors.parquet' f
  ON s.symbol = f.symbol AND s.date BETWEEN f.from_date AND f.to_date
WHERE s.symbol = 'RELIANCE' AND s.series = 'EQ'
```

Limits: Upstox's daily history starts in 2003, so earlier events are missing, though
later ones still adjust earlier prices. Delisted stocks aren't adjusted. A
renamed stock is followed through its ISIN, which the files carry from
2011-06-22 (INFOSYSTCH became INFY then). Telling a demerger from a bonus by
price alone can fail when the two ratios lie within 0.3%.

The daily job marks stocks that open and close below 0.92 or above 1.9 times
their last close, or change ISIN, and checks them against Upstox. Upstox tokens
expire at 03:30 IST, so it confirms them only on days you have run `serene auth`;
until then they wait. To run it at 19:30 IST on weekdays:

```sh
cp deploy/serene-equities.* ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now serene-equities.timer
```

## hearth

`deploy/hearth/` deploys serene to hearth, the shared AWS host from the iac
repo, and installs whatever `serene-*` timers sit in that directory. None do
now: the MCX hourly Slack pulse was removed on 2026-11-11, and a deploy stops
and deletes any installed unit the release no longer ships.

```sh
(cd ../iac && iac serene up)          # once: bucket, policy, serene user and database
cp deploy/hearth/server.env.example deploy/hearth/server.env   # fill it in
deploy/hearth/env.sh                  # .env -> SSM /serene/env
deploy/hearth/deploy.sh               # HEAD -> hearth
```

## Writing a strategy

Implement `strategy.Strategy` in `internal/strategies`, and add it to the
registry in `strategies.go`. `straddle.go` shows multi-leg selling, and
`breakout.go` shows using several timeframes.

- `OnBar` only ever receives closed bars.
- Orders fill at the next minute's open, plus slippage.
- `ctx.Chain` returns only contracts that have traded today.
- Since 2026-08-03 the index freezes from about 15:15 while options trade until
  15:40, so don't use `ctx.Spot` as a live level after 15:15.
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
- **Orders refused** lists rejections by reason. Add a `risk:` block to a config
  (see `configs/run.yaml`) to see what pre-trade limits would have stopped.
  Exits are never refused.
- **Trial count** on the report is every parameter set tried for the strategy on
  that data. The more tries, the more of the best result is luck.
- **`reports/*.json`** records the commit, config and cost hashes behind each
  report.

## Costs

`configs/costs.yaml` holds dated charges, and backtests stop with an error on
dates before its first period (2024-10-01). Add older rates to test earlier
history.
