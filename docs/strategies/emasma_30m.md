# goldm_emasma_30m, silverm_emasma_30m: silent forward test

Frozen 2026-11-24 at the user's request ("dont send me any updates on slack.
Only inform me after 100 trades"). Signals only: no orders. The rule is in
`cmd/serene/emasma.go` and must not change: a change is a new strategy.

## Rules

- **Instruments:** MCX GOLDM and SILVERM futures, 1 lot each, the backtest's
  active contract (most volume the day before), out at a roll.
- **Signal:** 30-minute bars. EMA 13 of closes against SMA 34 of closes,
  checked at each 30m close. Long while the EMA is above, short while below:
  always in. Fill at the next minute's open, 1 tick slippage, Zerodha charges.
- **Start:** trades entered from 2026-11-25. The side held then (a cross
  before it) does not count.
- **Runs:** `serene forward emasma --fetch --slack` on hearth
  (serene-emasma.timer, every 30 minutes of the session). It posts nothing
  until both strategies have 100 closed trades, then one verdict to
  SLACK_ALERT_CHANNEL; data/forward_emasma_judged.json stops repeats.
  Failures aren't posted either: check `journalctl -u serene-emasma`.
- **Expected:** about 245 trades a year each, so 100 by about late April 2027.

## Verdict, fixed before the start

Each passes if net > 0 at 1 and at 3 ticks, PF >= 1.1, and its MTM
drawdown is <= 1.5x the backtest's (GOLDM ₹3,74,882, SILVERM ₹5,61,816).
GOLDM and SILVERM correlate 0.76, so two passes are not independent.

## Where it came from

`serene research mcxma10` on Fyers expired futures 2019-2026
(reports/mcxma10_fyers/timeframes/). 198 variants (SMA/EMA/HMA for each line ×
11 pairs × L+S/long) at 10m/15m/30m/1h. Ranked by the worse of
2019-23 and 2024-26 net / MTM drawdown at 3 ticks, it was the only rule in the top 10 of
both metals. Its neighbourhood (30m-1h, pairs 12/26-21/55, EMA or SMA slow
line) passes broadly. 3-tick backtest: GOLDM 2019-23 +₹2.47L (MTM DD ₹75k),
2024-26 +₹12.3L (DD ₹2.50L); SILVERM +₹4.26L (DD ₹64k), +₹22.2L (DD ₹3.75L).
The longest stretch without a new high was 552 days (GOLDM, 2020-22) and 503
(SILVERM, 2019-23).

It was chosen from about 1,600 metal × frame × variant runs, using both
periods. Only this forward test is unseen data.
