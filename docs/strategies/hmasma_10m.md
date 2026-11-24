# goldm_hmasma_10m, silverm_hmasma_10m: live

Frozen 2026-11-24 and traded LIVE on Zerodha from that day, at the user's
request. The rule is in `cmd/serene/hmasma.go` and must not change; a change
is a new strategy.

## Rules

- **Instruments:** MCX GOLDM and SILVERM futures, 1 lot each, NRML.
- **Signal:** 10-minute bars of the backtest's active contract. HMA 34 of
  closes against SMA 89 of closes, checked at each 10m close. Long while the
  HMA is above, short while below: always in the market.
- **Orders:** `serene live mcx` on hearth, each minute after the bars are
  fetched. It holds the nearest future with at least 20 days to expiry and
  rolls out of it when fewer remain. A reversal is a close and then an open,
  one lot each, as LIMITs 0.3% through the last close, repriced each minute.
  A rejection, a short position at Zerodha, or 10 reprices without a fill
  halts the strategy until data/live/mcx-<strategy>.json is fixed by hand.
- **Start:** only a cross after go-live is traded, not the side held then.
- **Slack:** signals and fills with P&L to C0CH2BZU202, halts and failures to
  SLACK_ALERT_CHANNEL as well.

## Where it came from

`serene research mcxma10` (reports/mcxma10.txt): 198 variants per metal
(SMA/EMA/HMA for each line × 11 pairs × L+S/long) on GOLDM 2026-06-15 and
SILVERM 2026-07-01 to 2026-11-10, 1 tick + charges. HMA/SMA 34/89 L+S had the
best worst-metal result: GOLDM +₹4,44,536 (142 trades, PF 1.77, DD ₹1.13L),
SILVERM +₹3,93,195 (134 trades, PF 1.76, DD ₹80k), both halves positive on
both, unchanged at 3 ticks. Shorts made 71% / 82% of it while both metals fell.
Gaps held overnight and over weekends were 14–20% of the net.

Held-back check, run once (2026-11-11..23, `--unseal`): GOLDM +₹1,57,682 over
8 trades, SILVERM +₹75,030 over 10, again from shorts.

It is selected from a grid in one falling regime; nothing here shows it works
when gold and silver rise.
