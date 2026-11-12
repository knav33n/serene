# nifty_ema_retest (paper trade)

Paper traded from 2026-11-13 at the user's request. No orders go to a broker,
and it has **not been backtested**.

## Rules
- **Contracts:** NIFTY's 09:15 open rounded to 100, nearest weekly expiry,
  both the CE and the PE. The choice is kept in `data/paper/day-<date>.json`.
- **Chart:** each option's own 1-minute bars. EMA 13 and EMA 34 of closes,
  warmed on the previous trading day.
- **Arm:** a close with EMA13 above EMA34 after a close at or below it.
- **Entry:** a buy limit at the EMA34 of the last closed bar, re-placed each
  bar. It fills on the first bar whose low reaches it, at the limit or that
  bar's open if lower. A close with EMA13 back at or below EMA34 cancels it,
  though a retest usually fills before that can happen. No arms or fills from
  15:00. One trade a day: the first leg to fill.
- **Stop:** the most recent swing low of today confirmed before the fill and
  below it. A swing low's low is strictly below the 2 bars on each side. With
  no such low, that arm is dropped.
- **Trail:** each later confirmed swing low higher than the stop raises it,
  starting at the fill bar's close. A low at or below the stop exits at the
  stop, or at the open if it gaps through.
- **Time exit:** the open of the 15:15 bar.
- **Size:** ₹1,00,000 / (fill × lot size), rounded down.
- **Charges:** `configs/costs.yaml`. There is no slippage: a touch fills the
  limit and the stop fills at its level, which flatters 1-minute option bars.

## Running
`serene paper emaretest [--slack] [--date DATE]` replays the day from Kite and
posts the entry and exit once each to `SLACK_PAPER_CHANNEL` (C0CGWU1QK7X).
Closed trades are appended to `reports/paper_emaretest.csv`. hearth runs it
20 s past each minute, 09:00–15:59 IST on weekdays (`serene-paper.timer`).
It needs that morning's Kite login in project_calm.

Code: `internal/paper/emaretest.go` (rules, tested), `cmd/serene/papertrade.go`.
