# pdl_swing_day (stopped)

**Stopped 2026-11-26:** the forward signals (`serene paper pdlswing`, the
`serene-pdl` timer on hearth) were removed. The research command and the rules
in `internal/paper/pdlswing.go` remain.

Frozen on 2026-11-12 from `serene research pdlswing`, variant `cap400/day`. The
source is the r/NSEbets post "Holy grail of options buying" (puts only, buy the
previous-day-low break after a quiet day below the 20 EMA). The post's 3-day
hold is **not** used: it did no better than random puts in 2025–26 and was flat
over 14 years of daily NIFTY. Forward signals start on 2026-11-13. They are
signals only: no P&L is posted and no orders go to a broker.

## Rules
- **Setup (yesterday, daily NIFTY 50 bars):** the close is below EMA20 of
  closes, and the green body (close − open)/open is under 1%. The day must also
  be green, narrow range (its range no larger than each of the 3 days before)
  or inside the day before (`paper.PDLSetup`).
- **Signal:** the first 1-minute NIFTY bar today whose low is below yesterday's
  low and that ends before 15:00.
- **Contract, fixed at the signal bar:** the nearest NIFTY expiry with at least
  4 trading days after today. From the ATM strike (spot rounded to 50), step
  50 deeper ITM and take the deepest put whose last price at the signal minute
  is ₹400 or less. Strikes with no trade yet are skipped. If the ATM put is
  already above ₹400, there is no trade.
- **Entry:** the put's open on the minute after the signal (within 3 minutes),
  1 lot.
- **Exit:** the open of the 15:15 bar, with no stop.
- At most one trade a day.

## Research (2024-12-27..2026-04-30, charges + 30 bps a side)
27 trades: +₹66.5k, PF 2.25, max DD ₹23k, 1 position at a time, about ₹30k
premium. First half +₹23k, second half +₹43k. Net +₹62k at 2× slippage. It
beat all 200 random-entry draws (median random −₹0.3k). The best 5 days
contribute +₹81k, more than the total. Daily NIFTY 2012–2026 with the same exit
made +4,588 points over 261 signals and was positive in 14 of 15 years
(index points, no costs).

## Judging (set before any forward trade)
After 30 forward trades (about 16 months at the research rate), price each from
`reports/paper_pdlswing.csv` with `configs/costs.yaml` + 30 bps a side. It
passes if:
- net > 0,
- PF ≥ 1.2, and
- net is not negative at 2× slippage.

Code: `internal/paper/pdlswing.go` (rules, tested);
research in `cmd/serene/pdlswing.go`.
