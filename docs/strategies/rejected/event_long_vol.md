# event_long_vol: long straddles, strangles and guts on events

Researched 2026-11-09 with `serene research eventvol`. Not a registered strategy:
no cell is worth freezing, and the holdout (from 2026-05-01) was not touched.

## What was tried

NIFTY, 2024-12-27 to 2026-04-30, one lot of each leg, after charges, 30 bps a
side slippage (2 ticks minimum). Strikes are spot at entry rounded to 50, in the
nearest expiry alive at the exit. Fills are on the first traded minute.

- **Events** (the India session that first trades the news): RBI MPC decisions
  (8), Union Budgets (2, both weekend sessions), FOMC decisions (11, the session
  after the US announcement), all of them together (21), weekly expiries (70),
  and every other day as a control (247).
- **Timings:** T-1 15:10 to T 09:45 (gap only); T-1 15:10 to T 15:15; T 09:20
  to 11:30; T 09:20 to 15:15; T-1 15:10 to T+1 15:15; T-3 09:30 to T-1 15:10
  (the run-up, sold before the news).
- **Shapes:** guts 100 and 50, straddle, strangle 50, 100 and 200 points out.

216 combinations. Results: `reports/eventvol.txt`, `reports/eventvol.csv`,
and every trade in `reports/eventvol_trades.csv`.

## Results

- **Control days lose in every cell** (PF 0.56 to 0.83). Being long premium on
  an ordinary day is a steady loss, as expected.
- **RBI:** loses in every cell. The 10:00 announcement moves the index about 86
  points by 11:30 against a 300-point straddle.
- **Budget:** 2 events. Nothing to judge.
- **Expiry (0DTE and 1DTE):** loses in every cell. The 0DTE intraday straddle
  lost ₹108 a trade while expiry was on Thursday (to Aug 2025) and ₹2,003 a
  trade once it moved to Tuesday. Buying the run-up into expiry is the worst
  cell of all, at -₹188k.
- **Macro together:** best is the gap-only strangle 200, +₹4,283 on 21 trades
  at PF 1.17. It loses in the first half.
- **FOMC, T-1 15:10 to T+1 15:15:** the only strong cell. The straddle made
  +₹41,049 on 10 trades, PF 4.1, positive in both halves and at 3× slippage.

## Why the FOMC result isn't trusted

- It's the best of 216 tries. 12 of 216 made money in both halves.
- Two trades make 81% of it: 2026-03-20 (+₹20,701, a 690-point fall) and
  2025-03-20 (+₹12,707). The 2025-05-09 trade came during the India-Pakistan
  strikes, and 2025-08-01 the day after US tariffs on India. Those moves were
  not about the Fed.
- Ten random control trades of the same timing beat it 5% of the time, before
  allowing for the 216 tries.
- The same timing on ordinary days whose T is a Thursday or Friday already has
  PF 1.2, so part of it is the weekday (holding Wednesday to Friday, or over a
  weekend), not the Fed.
- Placebo weeks (the same timing 1 and 2 weeks either side) are mixed: -₹5,990,
  +₹7,213, -₹2,148, +₹85.

## If it's ever revisited

Freeze "FOMC, straddle, T-1 15:10 to T+1 15:15" before looking at any more
data, and judge it only on FOMC dates after 2026-05-01. There are 8 a year, so
30 trades takes about four years. The holdout has 3 or 4; that is too few to
pass anything, only enough to reject.

The event dates are hardcoded in `cmd/serene/eventvol.go`. RBI's 2026-04-08 date
was checked on rbi.org.in; the rest are from published calendars, not checked
against a source in this repo.
