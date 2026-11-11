# pd_break_trail: holdout and forward test

Frozen 2026-11-09. The rules and parameters below must not change. Any change
is a new strategy and needs fresh data of its own.

## Rules

- **Strike:** the one nearest the previous day's NIFTY low plus 1%, on 50-point
  steps, in the nearest expiry. Watch both the call and the put at that strike.
- **Call entry:** the first 1-minute close where NIFTY closes above its previous
  day high *and* the call closes above the call's own previous day high.
- **Put entry:** the first 1-minute close where NIFTY closes below its previous
  day low *and* the put closes above the put's own previous day high.
- Buy in the next minute. Each side at most once a day. No entries from 15:00.
- **Exit:** no stop. Once NIFTY has closed 200 points in the trade's favour,
  measured from NIFTY's open in the entry minute, sell on the first NIFTY close
  50 points short of the best close. Otherwise sell at 15:15.
- **Size:** one lot. The simulator adds 30 bps slippage a side, at least 2 ticks.

The option's previous day high needs that day's bars of a strike that's only
known at the close, so the strategy records every contract of the two nearest
expiries within 40 strikes of spot all day. It can't trade on a run's first
day, so the holdout and forward configs start one day early.

Configs:
- `configs/pd_break_trail.yaml`: the research window, to check the engine.
- `configs/pd_break_trail_holdout.yaml`: the one-time holdout check.
- `configs/pd_break_trail_forward.yaml`: the forward test.

## Where it came from

The user gave the strike and entry rules. `serene research pdbreak` tested them
on 2024-12-27 to 2026-04-30 with 11 exits. The trailing exit was the user's
idea; the 50-point trail distance was mine. The best exit, idx200, made
+₹114,384 on 236 trades after charges and 30 bps slippage: profit factor 1.19,
+₹98k before September 2025 and +₹16k after, max drawdown ₹57k on one lot.

Treat it as a lead, not a result:

- Only 28 of the 236 trades reached +200 and trailed out. They made +₹323k;
  the other 208, held to 15:15 with no stop, lost ₹209k.
- The top five days made +₹97k of the ₹114k.
- Puts made +₹103k; calls lost ₹33k after September 2025.
- Six stop-losses were tried on top; all did worse than none.
- It was the best of 11 exits on the same data.

## Engine replay of the research window

`configs/pd_break_trail.yaml` reproduces the research: +₹118,287 on 235 trades,
profit factor 1.19 and 48.9% wins, with no unfilled or rejected orders. The
research had 236 trades and +₹114,384; the extra trade is 2024-12-27 (−₹4,701),
which the engine can't take on a run's first day. Without that day the two
agree within ₹800.

| Slippage | Net | Sharpe | Max drawdown on ₹1L |
|---|---|---|---|
| 0× | +₹147,099 | 1.47 | 54.9% |
| 1× (30 bps) | +₹118,287 | 1.27 | 60.9% |
| 2× | +₹90,247 | 1.07 | 67.4% |
| 3× | +₹62,190 | 0.87 | 80.1% |

A 61% drawdown on ₹1 lakh with one lot means the account needs more capital
than that to sit through a bad run.

## Pass criteria (set before the data is seen)

**Holdout** (2026-05-01 to 2026-11-03, run once): all of

1. at least 30 trades,
2. net positive at 30 bps slippage,
3. profit factor of at least 1.2,
4. not negative at 2× slippage (the backtest report's slippage table).

**Forward test** (days after 2026-11-03): the same, with at least 60 trades.
At about 14 trades a month that's around early 2027. Look at it once at that
point, not week by week.

## How to run it

Holdout, once: `./serene research backtest --config configs/pd_break_trail_holdout.yaml --unseal`

Forward:
1. Fetch new expiries: `./serene research fetch`.
2. Set `to` in `configs/pd_break_trail_forward.yaml` to the last fetched expiry.
   That is the only line allowed to change.
3. Run `./serene research backtest --config configs/pd_break_trail_forward.yaml --unseal`.

Both runs are past the 2026-05-01 seal, so they need `--unseal`, and the trial
ledger records them.
