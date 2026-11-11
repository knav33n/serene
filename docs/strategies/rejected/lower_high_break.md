# lower_high_break: rejected 2026-11-08

Status: rejected. It failed its one-time holdout test. The code stays in the
registry so past results can be reproduced, and research runs print a
`REJECTED` warning. Don't retune it on data before 2026-11-03.

## The idea

An option-buying scalp on NIFTY, taken from a real CE trade on 2026-11-06
(entry 9:45, exit 9:56 when it broke below the 9:51 higher low).

- Strike: the ATM CE or PE in the nearest expiry, at the day's index open
  rounded to 100, fixed for the day. Calls and puts trade independently.
- Chart: the option's own 5-minute bars.
- Setup: after a pullback of at least 10%, a close above a lower high (the
  first break).
- Entry: the second break, a close above the next swing high (2 bars each
  side), with the close above the option's session VWAP and EMA21.
- Exit: a stop at the pullback low that trails up to 1-bar higher lows on
  closes, a 15-minute hold, or 15:15.
- Size: 25% of ₹1L in premium, at most 5 lots. Fills at the next minute with
  30 bps slippage.

Code: `internal/strategies/lowerhigh.go`. Configs:
`configs/lower_high_break.yaml` (research) and
`configs/lower_high_break_holdout.yaml` (the holdout run).

## Research: 2024-12-27 to 2026-04-30

- `serene research stops` and `serene research grid` tried 1,696 combinations of
  stops, targets, holds, time filters and bar sizes. On 1-minute bars none had
  an edge. 5-minute bars with the 15-minute hold was the one choice that held
  up across the split at 2025-09-01.
- Engine backtest: +₹30.9k, PF 1.23, 114 trades.
- Almost all of that came May–Sep 2025. From the 2025-09-22 peak there was a
  7-month drawdown of −₹36.6k at daily closes. In hindsight that warned that
  the edge was a single period, not a lasting pattern.

## Holdout: 2026-05-01 to 2026-11-03

Pass criteria, set before the run: net positive at 30 bps, PF at least 1.1,
and not negative at 2× slippage.

| | Result |
|---|---|
| Net at 30 bps | −₹36,234 |
| Net at 0 bps | −₹30,895 |
| Profit factor | 0.44 |
| Win rate | 27.5% |
| Trades | 40 |

It failed every criterion, and it lost money even with no slippage, so costs
aren't the reason.

## Trend filters, tried after the holdout

The `regime` (serene market Bull/Bear) and `adx_min` 25 filters looked better
before May 2026 (regime=1: PF 1.49 vs 1.23). On the holdout period all six
combinations lost (PF 0.14–0.50), and regime=1 did worst. These ran after the
holdout was spent, so they are evidence against the idea, not a fix for it.

## Trials

26 trials of lower_high_break in the `data/upstox/trials.jsonl` ledger,
13 of them on data from 2026-05-01. Each later idea tested on that period
counts against those 13.

## Why it probably failed

- The rules came from one trade and were then picked from a large grid. With
  1,696 combinations, a PF of 1.23 that came from one five-month stretch is
  what luck alone would produce.
- A 27.5% win rate with a tight stop needs large winners. Over the holdout
  there weren't enough of them to cover theta and costs.

## What would justify another look

A different idea, not a retune of this one. It would need data the idea hasn't
seen: weeks after 2026-11-03, or paper trading in step 6. The reusable pieces
are `internal/swing` (swing points), `internal/indicator` (EMA, VWAP, ADX) and
`serene research swings` for looking at a single day.
