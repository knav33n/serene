# silverm_ema_15m and silverm_hma_1h: forward tests

Frozen 2026-11-11. These are the two frozen GOLDM rules, goldm_ema_15m and
goldm_hma_1h, applied to MCX SILVERM without any change. The rules are
constants in `cmd/serene/silvermfwd.go` (`smConfig`). They must not change
during the forward test. Any change makes it a new strategy that needs fresh
data of its own. Each one is judged on its own, separately from the GOLDM
tests.

## Rules

- **Instrument:** MCX SILVERM (5 kg, ₹1 tick), one lot, on the active
  contract's minute bars.
- **silverm_ema_15m:** 15-minute bars from each contract's own minutes. Long
  while EMA 13 is above EMA 34 at a 15m close, short while it is below.
- **silverm_hma_1h:** 1-hour bars counted from the 09:00 open. Long while
  HMA 21 is above its own value two 1h bars back, short while it is below.
- **Both:** always in the market. A change fills at the next minute's open.
  Positions stay open overnight. At a roll, the old contract is closed at its
  first minute that day and the new one is entered in the current direction.
  Charges come from `configs/costs_mcx.yaml`, with 1 tick of slippage on every
  fill (3 ticks for the stress check).

## Where they came from

`serene research mcxmetals` (reports/mcxmetals_silver.txt) ran the EMA and
Hull grids on silver for 2026-07-01 to 2026-11-10, split at 2026-09-01. Before
July, SILVERM's only bars come from thinly traded far-month contracts. These
rules were not picked from the silver grid; they are the GOLDM rules applied
unchanged:

| | trades | net ₹ (1 tick) | PF | max DD ₹ | 3 ticks ₹ | shorts ₹ |
|---|---|---|---|---|---|---|
| silverm_ema_15m | 136 | 3,24,622 | 1.56 | 87,573 | 3,21,902 | 2,86,604 |
| silverm_hma_1h | 118 | 2,70,322 | 1.44 | 95,026 | 2,67,962 | 2,59,677 |

Both halves were positive for both rules. Caveats:
- Shorts made 85–95% of the profit while silver fell from ₹2.33L to ₹1.87L/kg.
- Silver's daily returns correlated 0.76 with GOLDM's over the window, so this
  is the same falling precious-metals regime as the GOLDM research. It is not
  an independent confirmation.
- On base metals the same rules lost on copper (−16% and −10% of notional).

Treat these as a second look at the GOLDM bet, not a result.
`serene research silvermfwd --strategy <name> --replay` reproduces the figures
above with the frozen code.

## Pass criteria (set before the data exists)

Judge each one only on sessions from 2026-11-12, and only once it has at least
100 trades. At the research pace of 27–32 trades a month, that's around
February or March 2027. A strategy passes if all of these hold:

1. Net positive at 1 tick.
2. Profit factor of at least 1.3.
3. Net positive at 3 ticks.
4. Max drawdown no more than ₹1,50,000. A SILVERM lot is about ₹11L, close to
   GOLDM's ₹15L.

Don't compare the four forward tests (two GOLDM, two SILVERM) to pick a winner
before each has been judged. Gold and silver move together, so the GOLDM and
SILVERM results will be correlated: a pass on both isn't two independent
passes.

## How to run it

1. hearth fetches SILVERM minute bars each minute with the signals. For a local
   copy, run `./serene mcx fetch --names SILVERM`, at least monthly, because
   Kite keeps minute bars only for live contracts.
2. `./serene research silvermfwd --strategy silverm_ema_15m` (or
   `silverm_hma_1h`) prints the trade count and nothing else.
3. At 100+ trades, run it once with `--judge`. It prints the result against
   the criteria and records the run in `data/mcx/trials.jsonl`.
