# ema_cross_first: forward test

Frozen 2026-11-08. The rules and parameters below must not change while
the forward test runs. Any change is a new strategy and needs fresh data of its own.

## Rules

- **Day:** trade only the day before the nearest NIFTY expiry (1DTE). Days are
  counted on NSE trading days, using the holiday list in
  `internal/market/holidays.go`. That list ends at 2026-12-31. Add NSE's 2027
  list when it's published, or the run stops with an error on the first 2027
  expiry.
- **Strike:** the NIFTY open rounded to 50, in the nearest expiry. Watch both
  the call and the put at that strike.
- **Signal:** each option is read on its own 1-minute chart, with EMA 3 and
  EMA 8 of closes. The EMAs are warmed on the previous afternoon's bars, so a
  gap can cross on the first bar.
- **Entry:** the first option of the day whose EMA 3 closes above its EMA 8
  is bought in the next minute. One trade a day. No entries from 15:00.
- **Exit:** sell in the next minute when that option's EMA 3 closes back below
  its EMA 8, or at 15:15.
- **Size:** one lot. The simulator adds 30 bps slippage a side, at least 2 ticks.

Configs:
- `configs/ema_cross_first.yaml`: the research window.
- `configs/ema_cross_first_forward.yaml`: the forward test.

## Where it came from

The rules came from `calm research emacross` on 2024-12-27 to 2026-04-30:
1DTE, option chart, 1-minute bars, 3/8, first trade a day. After slippage it
made +₹12,030 on 70 trades, with a profit factor of 1.41 in Jan–Aug 2025 and
1.44 in Sep 2025 – Apr 2026.

It was the best of about 270 combinations and filters tried on that window.
Finding 5 that worked in both halves, as this search did, is roughly what
chance alone would give. The edge is about ₹170 a trade on one lot, close to
the costs. So treat it as a lead to check, not a result.

## Pass criteria (set before the data exists)

Judge it only on expiries after 2026-11-03, once there are at least 30 trades.
It passes if all of these hold:

1. Net positive at 30 bps slippage.
2. Profit factor of at least 1.2.
3. Not negative at 2× slippage (the backtest report's slippage table).

There is one 1DTE day a week, so 30 trades takes about seven months, to around
June 2027. Look at it once at that point, not week by week. Checking it
repeatedly and stopping when it looks good is another way of fitting to the data.

## How to run it

1. Fetch new expiries: `./calm research fetch`.
2. Set `to` in `configs/ema_cross_first_forward.yaml` to the last fetched
   expiry. That is the only line allowed to change.
3. Run `./calm research backtest --config configs/ema_cross_first_forward.yaml --unseal`.

The date is past the 2026-05-01 seal, so the run needs `--unseal`, and the
trial ledger records it.

## Holdout result (2026-11-08)

`configs/ema_cross_first_holdout.yaml` was run once on 2026-05-01 to 2026-11-03.
No EMA research had looked at that data. The trial ledger records the run.

| Slippage | Net | Profit factor |
|---|---|---|
| 0× | +₹3,629 | |
| 1× (30 bps) | +₹2,428 | 1.27 |
| 2× | +₹1,307 | |
| 3× | +₹170 | |

It made +₹2,428 on 27 trades, with a 37% win rate and a 6.35% max drawdown.
That meets all three criteria: net positive, profit factor of at least 1.2, and
not negative at 2×. But the pass is provisional:

- 27 trades is under the 30 the criteria need.
- Two days, 2026-09-28 (+₹4,250) and 2026-11-02 (+₹2,203), made more than
  the whole net. Without them it loses.
- Monthly results alternate: +718, −1,620, +1,296, −886, +4,239, −3,174, +2,203.

The forward test on expiries after 2026-11-03 still decides it.
