# goldm_hma_1h: forward test

Frozen 2026-11-11. The rules below are constants in `cmd/serene/mcxhmafwd.go`.
They must not change during the forward test. Any change makes it a new
strategy that needs fresh data of its own. It runs next to goldm_ema_15m and
is judged on its own.

## Rules

- **Instrument:** MCX GOLDM, one lot, on the active contract's minute bars.
- **Signal:** 1-hour bars built from each contract's own minutes, counted from
  the 09:00 open (09:00, 10:00, …; the last bar is the session's tail).
  HMA 21 of closes: WMA(2·WMA(10) − WMA(21), 5).
- **Position:** always in the market. Long while HMA 21 at a 1h close is above
  its value two 1h bars earlier, short while it is below. A change fills at
  the next minute's open.
- **Holding:** positions stay open overnight. At a roll, the old contract is
  closed at its first minute that day and the new one is entered in the
  current direction.
- **Costs:** charges from `configs/costs_mcx.yaml` and 1 tick of slippage on
  every fill (3 ticks for the stress check).

## Where it came from

It was the best of the 144 minute-bar variants in `serene research mcxhma` on
2026-06-15 to 2026-11-10 (5m to 4h; seven HMA pairs and five HMA turns; long+short
and long only): +₹4,83,469, PF 1.80, 136 trades, max drawdown ₹73,066,
+₹4,78,029 at 3 ticks. Both halves were positive (split at 2026-09-01). That
is only about 5% better than goldm_ema_15m over the same window. About 70% of
the profit came from shorts while gold fell, the window was five months of one
regime, and it was picked as the best of 144. What held up more broadly is the
frame: 10 of 12 long+short 1h variants and 11 of 12 2h ones were positive in
both halves, while 5m and 15m lost. Treat it as a lead to check, not a result.
`serene research mcxhmafwd --replay` reproduces those figures with the frozen
code.

## Pass criteria (set before the data exists)

Judge it only on sessions from 2026-11-11, and only once it has at least 100
trades. At the research pace of about 27 a month, that's around late
February or March 2027. It passes if all of these hold:

1. Net positive at 1 tick.
2. Profit factor of at least 1.3.
3. Net positive at 3 ticks.
4. Max drawdown no more than ₹1,50,000.

The long/short split is reported but isn't a criterion. Passing doesn't make it
better than goldm_ema_15m; the two are judged separately and their forward
results shouldn't be compared to pick a winner before both are judged.

## How to run it

1. Fetch: `./serene mcx fetch --names GOLDM`. Kite keeps minute bars only for
   live contracts, so fetch at least monthly or the bars are lost.
2. `./serene research mcxhmafwd` prints the trade count and nothing else.
3. At 100+ trades, run `./serene research mcxhmafwd --judge` once. It prints the
   result against the criteria and records the run in `data/mcx/trials.jsonl`.

`data/mcx` is sealed from 2026-11-11 in `configs/holdout.yaml`, so no other
research on that directory may read the forward data.
