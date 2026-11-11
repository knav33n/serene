# serene

Research and trade Indian index options in Go. Strategies are written once and
run unchanged in backtests, and later on paper and live.

**Status:** research works on synthetic data and real NIFTY options, and broker
login works. Paper trading and live trading are not built yet, and those commands
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

Upstox and Zerodha tokens expire every morning (03:30 and 06:00 IST), so log in
once a day. Tokens can place real orders, so they're encrypted with
`SERENE_TOKEN_KEY` before being stored in Postgres, and never shown in the page,
terminal or logs.

Setup:

1. Copy `.env.example` to `.env` and fill it in.
2. Register a redirect URL with each broker's app. With these, a login finishes
   by itself while `serene auth` is running:
   - `http://127.0.0.1:8080/callback/upstox`
   - `http://127.0.0.1:8080/callback/zerodha`

   You can use another URL if you can copy the token from it. This project's
   Upstox app uses `https://httpbin.org/get`. For Upstox, set
   `UPSTOX_REDIRECT_URI` to exactly the registered value, because it's sent with
   every login. A mismatch gives `UDAPI100068`, and `serene auth` prints the value
   it's using so you can compare.

   Login codes pass through httpbin, a third party, so switch to the local URL
   before live trading.

Daily use:

```sh
./serene auth                  # opens the login page
./serene auth status           # shows each login, never the token
./serene auth logout upstox    # revokes the token at the broker and deletes it
```

On the page, **Log in** shows the broker's URL and a box. Open the URL, log in,
then paste the token from the redirected URL (`code` for Upstox, `request_token`
for Zerodha), or paste the whole URL. Paste it quickly: these tokens are short
lived, and Upstox's works only once.

Code gets a token with `auth.Load(ctx, store, "upstox")`. Trading should also
check `Token.ValidThrough` so a token can't expire during the session.

The page is in `web/`. `web/dist` is committed, so `go build` doesn't need Node.
After changing it, run `npm run build` in `web/`. To run the Postgres tests, set
`SERENE_TEST_DATABASE_URL`; they use a temporary schema.

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
fails the summary goes out as text with an alert. Other failures post to Slack too.

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
