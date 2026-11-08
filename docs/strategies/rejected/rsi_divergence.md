# rsi_divergence: rejected 2026-11-08

Status: rejected. No version showed an edge on research data, so none went to
its holdout. The code stays in the registry so the results can be reproduced,
and research runs print a `REJECTED` warning. The holdouts are untouched for
this idea: NIFTY options from 2026-05-01, stocks from 2023-01-01.

## The idea

Buy an option or a stock after an RSI(14) divergence:

- Regular bullish: a swing low below the previous one with a higher RSI.
  Regular bearish: a higher swing high with a lower RSI (puts only).
- Hidden bullish: a higher low with a lower RSI. Hidden bearish: a lower high
  with a higher RSI (puts only).

Swing points are 2 bars each side and 3 to 24 bars apart, within one session
for options.

Options: the ATM CE or PE in the nearest expiry, at the day's NIFTY open
rounded to 100. Entry is a market order filled in the next minute. Exit is a
stop at the option's low, a 2R target, or 15:15. Size is 25% of ₹1L in
premium, at most 5 lots, with 30 bps slippage.

Code: `internal/strategies/rsidiv.go`, with the `chart`, `hidden`,
`trend_ema` and `confirm_ema` modes, and `indicator.RSI`. Stocks:
`calm research stockdiv` in `cmd/calm/stockdiv.go`.

## NIFTY options, 2024-12-27 to 2026-04-30

| # | Config | Version | Trades | PF | Net at 30 bps | Net at 0 bps |
|---|---|---|---|---|---|---|
| 1 | rsi_divergence.yaml | regular, option's own 5m chart, RSI < 40 at 1st low | 340 | 0.88 | −₹75,606 | +₹6,588 |
| 2 | rsi_divergence_index.yaml | regular, NIFTY 5m chart (bull → CE, bear → PE) | 237 | 0.84 | −₹53,860 | −₹14,845 |
| 3 | rsi_hidden_index.yaml | hidden + EMA50 trend, NIFTY 5m | 295 | 0.93 | −₹40,848 | +₹13,829 |
| 4 | rsi_hidden_option.yaml | hidden + EMA50 trend, option's 5m chart | 152 | 0.67 | −₹95,008 | +₹3,049 |
| 5 | rsi_confirmed_15m.yaml | regular, NIFTY 15m, 30/70, close across EMA20 within 4 bars | 12 | 1.09 | +₹1,732 | +₹2,975 |

Pass criteria for trials 3 and 5, set before each run: net positive at 30
bps, PF at least 1.1, and not negative at 2× slippage. Trial 3 failed all
three. Trial 5 failed on PF. It also had only 12 trades, and all of its gain
came in Aug–Sep 2025.

What the trials show: before costs the signal is about breakeven. Charges
(₹16k–49k) and slippage decide the result. The filters improved the numbers
mainly by cutting the number of trades.

## NSE stocks, daily, 2005–2022

The stock test is long only. Each month the universe is the 200 most-traded
stocks by the previous month's turnover, with delisted stocks included and
prices adjusted for every corporate action. A trade buys at the next open,
stops at the 2nd low, targets 2R and sells after 20 days otherwise. Costs
are 0.24% round trip plus 10 bps slippage each side.

| Version | Trades | Avg net/trade | PF | t | vs equal-weight universe |
|---|---|---|---|---|---|
| RSI < 30, close above EMA20 within 4 bars | 652 | +0.04% | 1.01 | 0.07 | −1.27% |
| RSI < 40, no confirmation | 5,667 | −0.14% | 0.96 | −1.12 | −0.70% |

Both did worse than holding the liquid universe over the same days. The
confirmed version beat the universe in 3 of 18 years. It lost most in 2008
and 2011, the years when buying the dip should have helped.

## Sources checked

- howtotrade.com cheat sheet and PDF: definitions only, with no entry, stop
  or target rules.
- rupeezy.in/blog/rsi-divergence: names only moving averages as a
  confirmation, and says divergences are noisier on lower timeframes. Trial
  5 and the stock test follow from it.

Don't retune this on the same data. A new variant would need data it hasn't
seen.
