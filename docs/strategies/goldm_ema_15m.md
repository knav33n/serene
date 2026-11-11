# goldm_ema_15m: forward test

Frozen 2026-11-11. The rules below are constants in `cmd/serene/mcxemafwd.go`.
They must not change during the forward test. Any change makes it a new
strategy that needs fresh data of its own.

## Rules

- **Instrument:** MCX GOLDM, one lot, on the active contract's minute bars.
- **Signal:** 15-minute bars built from each contract's own minutes. EMA 13 and
  EMA 34 of closes.
- **Position:** always in the market. Long while EMA 13 is above EMA 34 at a
  15m close, short while it is below. A change fills at the next minute's open.
- **Holding:** positions stay open overnight. At a roll, the old contract is
  closed at its first minute that day and the new one is entered in the
  current direction.
- **Costs:** charges from `configs/costs_mcx.yaml` and 1 tick of slippage on
  every fill (3 ticks for the stress check).

## Where it came from

It was the best of the 64 minute-bar variants in `serene research mcxema` on
2026-06-15 to 2026-11-10: +₹4,61,283, PF 1.82, 138 trades, max drawdown
₹95,143, +₹4,55,764 at 3 ticks. Both halves were positive (split at 2026-09-01).
But about 70% of the profit came from shorts while gold fell, the window was
five months of one regime, and it was picked as the best of a grid. Treat it
as a lead to check, not a result. `serene research mcxemafwd --replay`
reproduces those figures with the frozen code.

## Pass criteria (set before the data exists)

Judge it only on sessions from 2026-11-11, and only once it has at least 100
trades. At the research pace of about 28 a month, that's around late
February or March 2027. It passes if all of these hold:

1. Net positive at 1 tick.
2. Profit factor of at least 1.3.
3. Net positive at 3 ticks.
4. Max drawdown no more than ₹1,50,000.

The long/short split is reported but isn't a criterion.

## How to run it

1. Fetch: `./serene mcx fetch --names GOLDM`. Kite keeps minute bars only for
   live contracts, so fetch at least monthly or the bars are lost.
2. `./serene research mcxemafwd` prints the trade count and nothing else.
3. At 100+ trades, run `./serene research mcxemafwd --judge` once. It prints the
   result against the criteria and records the run in `data/mcx/trials.jsonl`.

`data/mcx` is sealed from 2026-11-11 in `configs/holdout.yaml`, so no other
research on that directory may read the forward data.
