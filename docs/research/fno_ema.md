# Daily EMA crossovers on each F&O stock

Researched 2026-11-15. Code: `serene research stockema --list configs/fno.csv`
(`cmd/serene/stockema.go`). Per-stock numbers for every pair:
`reports/fno_ema_long.csv` (long only) and `reports/fno_ema_ls.csv` (long and
short). The same method run on the whole Nifty 500 is in
[nifty500_ema.md](nifty500_ema.md).

## Short answer

The F&O list gives the same result as the Nifty 500.

- **Long only:** no pair stays best for a stock. On 2019–2022 the
  training-period pick does about as well as a random pair. Buy and hold
  beat it on both return and Sharpe for the typical stock. The crossover's
  one benefit is a smaller drawdown.
- **Long and short:** shorting below the cross, which F&O makes possible,
  is clearly worse. For the typical stock nearly every pair loses money on
  2019–2022 after costs, with deeper drawdowns than buy and hold. Indian
  stocks drifted up over this period, so the shorts mostly paid for the
  whipsaws.

## Method

- Universe: today's 218 F&O stock underlyings (`configs/fno.csv`, from
  NSE's `fo_mktlots.csv`, fetched 2026-11-15, with index underlyings
  removed). 20 renamed stocks are joined to their older symbols by ISIN.
- Stocks tested: 148. Another 59 had less than 3 years of training data,
  and 11 have no data before the 2023 holdout.
- Survivorship bias: using today's list means stocks that were dropped from
  F&O are missing.
- Signals, fills, pairs, the train years (2005–2018), the test years
  (2019–2022) and the 2023+ holdout are all the same as for the Nifty 500.
- **Long only:** delivery costs, 0.24% round trip plus 10 bps slippage on
  each side.
- **Long and short:** long when fast > slow and short when below, so the
  position is always in the market. Costs are 0.10% round trip, a rough
  figure for stock futures, plus 10 bps slippage on each side.
- What's left out: trades use the cash price as a stand-in for the future.
  Futures basis, monthly roll costs and margin are not modelled. Leaving
  them out flatters long and short slightly.

## Long only, 2019–2022 test (medians per stock)

| pair | train CAGR | test CAGR | train Sharpe | test Sharpe | test max DD | trades/yr | time in market | beat B&H Sharpe |
|---|---|---|---|---|---|---|---|---|
| 5/13 | 6.5% | 5.9% | 0.37 | 0.36 | 38% | 9.3 | 55% | 28% |
| 5/20 | 7.7% | 6.8% | 0.42 | 0.39 | 38% | 7.4 | 56% | 37% |
| 8/21 | 7.6% | 8.0% | 0.41 | 0.48 | 36% | 5.7 | 57% | 38% |
| 9/21 | 8.5% | 8.5% | 0.45 | 0.44 | 36% | 5.3 | 57% | 43% |
| 10/30 | 9.4% | 7.5% | 0.47 | 0.44 | 35% | 4.3 | 58% | 39% |
| 12/26 | 9.3% | 8.0% | 0.47 | 0.44 | 35% | 4.3 | 58% | 38% |
| 13/34 | 9.7% | 7.9% | 0.48 | 0.43 | 36% | 3.8 | 59% | 38% |
| 20/50 | 11.1% | 6.8% | 0.52 | 0.38 | 37% | 2.5 | 61% | 32% |
| 21/55 | 10.4% | 6.7% | 0.52 | 0.38 | 37% | 2.3 | 61% | 28% |
| 30/100 | 12.1% | 8.9% | 0.53 | 0.45 | 38% | 1.5 | 61% | 32% |
| 50/100 | 11.8% | 8.3% | 0.55 | 0.45 | 38% | 1.3 | 61% | 29% |
| 50/200 | 12.0% | 8.6% | 0.53 | 0.42 | 40% | 0.8 | 62% | 30% |
| 100/200 | 11.6% | 7.9% | 0.52 | 0.44 | 40% | 0.5 | 61% | 33% |
| B&H | 15.4% | 14.4% | 0.58 | 0.57 | 52% | 0.3 | 100% | 0% |

How each stock's own pick did:

- Median test Sharpe was 0.44; buy and hold's was 0.57. Median test CAGR was
  8.8%; buy and hold's was 14.4%.
- 56 of 148 picks beat buy and hold's Sharpe.
- The pick's average test rank was 0.52, where 0.50 is what chance gives
  (0.7 standard errors from 0.50).
- It was in the test's top 3 for 25% of stocks; chance gives 23%.
- On the earlier split (train 2005–2012, test 2013–2022, 123 stocks), the
  pick averaged 0.52 (0.8 standard errors). Its median test CAGR was 7.4%,
  against 13.2% for buy and hold.

## Long and short, 2019–2022 test (medians per stock)

| pair | train CAGR | test CAGR | train Sharpe | test Sharpe | test max DD | trades/yr | time in market | beat B&H Sharpe |
|---|---|---|---|---|---|---|---|---|
| 5/13 | -5.3% | -6.8% | 0.07 | -0.02 | 58% | 18.5 | 100% | 17% |
| 5/20 | -2.5% | -4.0% | 0.13 | 0.06 | 59% | 14.7 | 100% | 24% |
| 8/21 | -1.4% | -4.7% | 0.16 | 0.08 | 58% | 11.4 | 100% | 24% |
| 9/21 | -0.3% | -2.0% | 0.18 | 0.15 | 57% | 10.9 | 100% | 25% |
| 10/30 | -0.1% | -3.4% | 0.19 | 0.12 | 55% | 8.8 | 100% | 24% |
| 12/26 | -0.4% | -4.0% | 0.17 | 0.11 | 55% | 8.4 | 100% | 24% |
| 13/34 | -0.6% | -4.0% | 0.21 | 0.09 | 56% | 7.4 | 100% | 19% |
| 20/50 | 1.3% | -5.1% | 0.24 | 0.04 | 58% | 4.8 | 100% | 13% |
| 21/55 | 0.9% | -5.4% | 0.23 | 0.06 | 60% | 4.6 | 100% | 14% |
| 30/100 | 2.6% | -3.6% | 0.28 | 0.14 | 60% | 2.8 | 100% | 18% |
| 50/100 | 1.9% | -3.7% | 0.26 | 0.08 | 61% | 2.3 | 100% | 19% |
| 50/200 | 1.7% | -4.3% | 0.24 | 0.10 | 64% | 1.5 | 100% | 18% |
| 100/200 | 2.8% | -0.8% | 0.28 | 0.19 | 65% | 1.0 | 100% | 22% |
| B&H | 15.4% | 14.5% | 0.58 | 0.57 | 52% | 0.3 | 100% | 0% |

How each stock's own pick did:

- Median test Sharpe was 0.19; buy and hold's was 0.57. Median test CAGR was
  −0.6%; buy and hold's was 14.5%.
- 73 of 148 picks made money, and 31 beat buy and hold's Sharpe.
- The pick's average test rank was 0.54 (1.5 standard errors from 0.50).
  That's still not significant.

The slow 100/200 pair loses least, because it is long most of the time.
Even so, its median test CAGR is −0.8%.

## Per stock

Pick = the pair with the best training Sharpe (2005–2018) in each mode. The
test columns are 2019–2022. Held = the pick made money on the test years and
beat buy and hold's test Sharpe; 53 of 148 did long only, and 26 of 148 did
long and short. As on the Nifty 500, a "yes" is mostly luck.

| stock | industry | train from | long pick | test Sharpe | test CAGR | test DD | held | L+S pick | test Sharpe | test CAGR | test DD | held | B&H Sharpe | B&H CAGR | B&H DD |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| ABB | Capital Goods | 2005 | 10/30 | 0.91 | 22.6% | 23% | yes | 10/30 | 0.59 | 14.9% | 36% | no | 0.88 | 26.9% | 47% |
| ADANIENT | Metals & Mining | 2007 | 5/20 | 1.86 | 89.2% | 36% | no | 5/20 | 1.06 | 48.8% | 60% | no | 1.92 | 123.7% | 53% |
| ADANIPORTS | Services | 2008 | 30/100 | 0.35 | 6.1% | 41% | no | 30/100 | -0.12 | -11.4% | 62% | no | 0.69 | 20.8% | 51% |
| ADANIPOWER | Power | 2010 | 10/30 | 1.05 | 47.4% | 44% | no | 10/30 | 0.65 | 23.5% | 69% | no | 1.06 | 56.5% | 64% |
| AMBUJACEM | Construction Materials | 2008 | 13/34 | 0.38 | 6.5% | 38% | no | 13/34 | -0.23 | -12.2% | 56% | no | 0.81 | 23.9% | 42% |
| APLAPOLLO | Capital Goods | 2012 | 50/100 | 0.93 | 32.4% | 49% | no | 50/100 | 0.11 | -5.0% | 77% | no | 1.51 | 75.9% | 49% |
| APOLLOHOSP | Healthcare | 2005 | 100/200 | 0.77 | 24.1% | 41% | no | 100/200 | 0.46 | 10.9% | 54% | no | 1.04 | 38.0% | 40% |
| ASHOKLEY | Capital Goods | 2005 | 50/100 | 0.33 | 5.4% | 38% | no | 50/100 | -0.01 | -11.3% | 64% | no | 0.41 | 8.7% | 67% |
| ASIANPAINT | Consumer Durables | 2005 | 100/200 | 0.63 | 14.5% | 35% | no | 100/200 | 0.37 | 6.7% | 51% | no | 0.87 | 22.7% | 28% |
| ASTRAL | Capital Goods | 2008 | 10/30 | 0.29 | 4.4% | 36% | no | 10/30 | -0.38 | -20.8% | 67% | no | 0.87 | 30.6% | 39% |
| AUROPHARMA | Healthcare | 2005 | 20/50 | -0.46 | -16.1% | 51% | no | 20/50 | -0.48 | -27.6% | 75% | no | -0.10 | -12.4% | 65% |
| AXISBANK | Financial Services | 2008 | 20/50 | 0.51 | 10.2% | 35% | yes | 20/50 | 0.20 | -0.1% | 65% | no | 0.46 | 10.7% | 63% |
| BAJAJ-AUTO | Automobile and Auto Components | 2009 | 20/50 | 0.21 | 2.3% | 31% | no | 100/200 | -1.02 | -27.7% | 76% | no | 0.39 | 7.2% | 41% |
| BAJAJFINSV | Financial Services | 2009 | 9/21 | 1.44 | 40.3% | 28% | yes | 9/21 | 1.15 | 46.0% | 40% | yes | 0.76 | 24.4% | 59% |
| BAJFINANCE | Financial Services | 2011 | 13/34 | 1.29 | 38.6% | 26% | yes | 100/200 | -0.00 | -8.7% | 74% | no | 0.76 | 25.9% | 63% |
| BANKBARODA | Financial Services | 2005 | 12/26 | 0.94 | 28.3% | 33% | yes | 12/26 | 0.88 | 34.4% | 43% | yes | 0.47 | 11.7% | 74% |
| BANKINDIA | Financial Services | 2005 | 13/34 | 0.34 | 6.0% | 46% | yes | 13/34 | 0.37 | 6.8% | 57% | yes | 0.14 | -4.3% | 71% |
| BEL | Capital Goods | 2005 | 50/100 | 0.94 | 28.1% | 31% | no | 50/100 | 0.51 | 13.3% | 60% | no | 0.97 | 36.2% | 51% |
| BHARATFORG | Automobile and Auto Components | 2005 | 50/100 | 0.56 | 12.0% | 37% | yes | 50/100 | 0.18 | -1.2% | 63% | no | 0.54 | 14.7% | 59% |
| BHARTIARTL | Telecommunication | 2007 | 30/100 | 0.72 | 18.9% | 28% | no | 30/100 | 0.43 | 9.4% | 30% | no | 0.87 | 26.7% | 34% |
| BHEL | Capital Goods | 2005 | 50/200 | 0.54 | 12.6% | 41% | yes | 50/200 | 0.35 | 4.8% | 69% | yes | 0.29 | 1.7% | 74% |
| BIOCON | Healthcare | 2005 | 100/200 | 0.26 | 3.5% | 31% | yes | 100/200 | 0.36 | 6.6% | 41% | yes | 0.02 | -4.7% | 46% |
| BLUESTARCO | Consumer Durables | 2005 | 5/20 | 0.39 | 6.7% | 32% | no | 5/20 | -0.03 | -6.4% | 56% | no | 0.66 | 17.7% | 49% |
| BOSCHLTD | Automobile and Auto Components | 2008 | 13/34 | 0.24 | 2.9% | 29% | yes | 13/34 | 0.25 | 2.8% | 47% | yes | 0.07 | -3.4% | 58% |
| BPCL | Oil Gas & Consumable Fuels | 2005 | 30/100 | -0.15 | -6.0% | 45% | no | 30/100 | -0.30 | -16.8% | 66% | no | 0.13 | -2.3% | 52% |
| BRITANNIA | Fast Moving Consumer Goods | 2005 | 10/30 | 0.30 | 3.8% | 24% | no | 10/30 | 0.00 | -3.4% | 39% | no | 0.44 | 8.4% | 36% |
| CANBK | Financial Services | 2005 | 50/100 | 0.50 | 11.6% | 41% | yes | 12/26 | 0.56 | 16.5% | 48% | yes | 0.33 | 4.7% | 74% |
| CGPOWER | Capital Goods | 2005 | 30/100 | 2.58 | 134.9% | 25% | yes | 50/200 | 1.31 | 79.9% | 83% | yes | 1.08 | 57.5% | 90% |
| CHOLAFIN | Financial Services | 2011 | 50/100 | 0.55 | 14.0% | 42% | no | 50/200 | -0.15 | -18.0% | 87% | no | 0.79 | 30.5% | 64% |
| CIPLA | Healthcare | 2005 | 12/26 | 0.40 | 6.3% | 26% | no | 12/26 | -0.17 | -8.5% | 56% | no | 0.79 | 20.1% | 35% |
| COALINDIA | Oil Gas & Consumable Fuels | 2011 | 50/100 | 0.14 | 0.4% | 34% | yes | 50/100 | 0.07 | -3.5% | 54% | no | 0.12 | -1.8% | 59% |
| COFORGE | Information Technology | 2005 | 100/200 | 0.70 | 22.6% | 57% | no | 100/200 | 0.36 | 5.9% | 65% | no | 0.90 | 36.2% | 57% |
| COLPAL | Fast Moving Consumer Goods | 2008 | 21/55 | -0.34 | -7.4% | 38% | no | 100/200 | -0.74 | -17.9% | 54% | no | 0.25 | 3.2% | 32% |
| CONCOR | Services | 2005 | 50/200 | -0.28 | -9.8% | 42% | no | 50/200 | -0.75 | -29.6% | 78% | no | 0.38 | 7.6% | 56% |
| COROMANDEL | Chemicals | 2010 | 21/55 | 0.33 | 5.2% | 33% | no | 21/55 | -0.16 | -9.3% | 66% | no | 0.71 | 18.5% | 32% |
| CUB | Financial Services | 2005 | 12/26 | -0.10 | -4.9% | 50% | no | 12/26 | -0.22 | -13.7% | 66% | no | 0.13 | -2.1% | 55% |
| CUMMINSIND | Capital Goods | 2006 | 20/50 | 0.69 | 15.4% | 33% | yes | 20/50 | 0.52 | 12.6% | 43% | no | 0.53 | 13.1% | 67% |
| DABUR | Fast Moving Consumer Goods | 2005 | 50/200 | 0.01 | -2.0% | 28% | no | 50/200 | -0.36 | -10.9% | 40% | no | 0.39 | 6.8% | 26% |
| DIVISLAB | Healthcare | 2005 | 9/21 | 0.27 | 3.5% | 39% | no | 9/21 | -0.41 | -15.3% | 53% | no | 0.85 | 23.4% | 39% |
| DLF | Realty | 2008 | 8/21 | 0.85 | 23.8% | 29% | yes | 8/21 | 0.53 | 14.9% | 45% | no | 0.65 | 20.8% | 54% |
| DRREDDY | Healthcare | 2005 | 100/200 | 0.71 | 15.3% | 22% | yes | 100/200 | 0.68 | 16.0% | 23% | yes | 0.58 | 12.9% | 33% |
| EICHERMOT | Automobile and Auto Components | 2005 | 13/34 | 0.23 | 2.6% | 41% | no | 13/34 | -0.11 | -9.7% | 65% | no | 0.41 | 8.7% | 45% |
| FEDERALBNK | Financial Services | 2005 | 21/55 | 0.37 | 6.8% | 39% | no | 21/55 | 0.07 | -5.3% | 57% | no | 0.45 | 10.4% | 66% |
| FORTIS | Healthcare | 2008 | 50/200 | 0.43 | 8.7% | 34% | no | 50/200 | 0.06 | -3.8% | 53% | no | 0.70 | 19.6% | 31% |
| GAIL | Oil Gas & Consumable Fuels | 2005 | 50/200 | -0.43 | -10.1% | 40% | no | 50/200 | -0.49 | -21.3% | 73% | no | 0.02 | -5.7% | 62% |
| GLENMARK | Healthcare | 2005 | 50/200 | -0.28 | -7.8% | 43% | no | 50/200 | -0.19 | -16.0% | 79% | no | -0.09 | -11.7% | 73% |
| GMRAIRPORT | Services | 2007 | 5/13 | 0.20 | 1.5% | 40% | no | 5/13 | -0.35 | -21.0% | 74% | no | 0.74 | 24.9% | 38% |
| GODFRYPHLP | Fast Moving Consumer Goods | 2005 | 5/20 | 0.45 | 9.7% | 46% | no | 5/20 | 0.04 | -7.2% | 69% | no | 0.70 | 22.6% | 48% |
| GODREJCP | Fast Moving Consumer Goods | 2005 | 100/200 | -0.43 | -11.6% | 48% | no | 100/200 | -0.81 | -27.0% | 73% | no | 0.21 | 1.7% | 47% |
| GODREJPROP | Realty | 2010 | 12/26 | 0.46 | 10.0% | 38% | no | 12/26 | 0.09 | -6.2% | 54% | no | 0.58 | 17.2% | 54% |
| GRASIM | Construction Materials | 2005 | 8/21 | 1.25 | 29.7% | 24% | yes | 8/21 | 0.96 | 31.7% | 36% | yes | 0.71 | 20.3% | 58% |
| GVT&D | Capital Goods | 2009 | 50/100 | -0.16 | -7.5% | 38% | no | 50/100 | 0.08 | -7.3% | 69% | no | -0.26 | -21.0% | 82% |
| HAVELLS | Consumer Durables | 2005 | 100/200 | 0.44 | 8.5% | 35% | no | 100/200 | 0.21 | 1.6% | 45% | no | 0.52 | 12.3% | 42% |
| HCLTECH | Information Technology | 2005 | 50/200 | 0.38 | 6.5% | 37% | no | 50/200 | -0.16 | -8.6% | 57% | no | 0.81 | 21.5% | 35% |
| HDFCBANK | Financial Services | 2005 | 50/200 | 0.27 | 3.5% | 28% | no | 50/200 | -0.12 | -7.4% | 49% | no | 0.52 | 11.3% | 41% |
| HEROMOTOCO | Automobile and Auto Components | 2005 | 100/200 | 0.04 | -0.5% | 30% | no | 100/200 | -0.03 | -6.1% | 57% | no | 0.07 | -3.1% | 50% |
| HINDALCO | Metals & Mining | 2008 | 30/100 | 1.00 | 28.6% | 35% | yes | 30/100 | 0.71 | 23.8% | 52% | yes | 0.65 | 20.6% | 61% |
| HINDPETRO | Oil Gas & Consumable Fuels | 2005 | 8/21 | 0.30 | 4.4% | 32% | yes | 8/21 | 0.28 | 3.5% | 37% | yes | 0.14 | -2.0% | 50% |
| HINDUNILVR | Fast Moving Consumer Goods | 2008 | 100/200 | 0.33 | 5.1% | 24% | no | 100/200 | 0.14 | 0.5% | 36% | no | 0.46 | 8.9% | 31% |
| HINDZINC | Metals & Mining | 2007 | 50/100 | 0.25 | 3.1% | 40% | no | 50/100 | 0.10 | -2.9% | 56% | no | 0.28 | 3.6% | 57% |
| ICICIBANK | Financial Services | 2005 | 30/100 | 0.62 | 13.6% | 32% | no | 100/200 | 0.14 | -1.6% | 67% | no | 0.82 | 25.6% | 48% |
| IDEA | Telecommunication | 2007 | 13/34 | 0.44 | 10.4% | 43% | yes | 13/34 | 0.38 | -7.4% | 82% | no | 0.14 | -23.8% | 87% |
| INDHOTEL | Consumer Services | 2005 | 21/55 | 0.65 | 16.3% | 28% | no | 21/55 | 0.27 | 2.5% | 47% | no | 0.69 | 22.3% | 61% |
| INDIANB | Financial Services | 2007 | 30/100 | 0.65 | 18.2% | 46% | yes | 30/100 | 0.60 | 19.1% | 65% | yes | 0.33 | 4.0% | 85% |
| INDUSINDBK | Financial Services | 2005 | 21/55 | 0.38 | 7.1% | 37% | yes | 21/55 | 0.26 | -2.0% | 72% | no | 0.15 | -6.8% | 83% |
| INDUSTOWER | Telecommunication | 2013 | 50/200 | -0.58 | -14.9% | 51% | no | 50/200 | -0.63 | -34.2% | 85% | no | 0.07 | -7.7% | 62% |
| INFY | Information Technology | 2005 | 100/200 | 0.44 | 7.8% | 36% | no | 100/200 | -0.15 | -8.8% | 65% | no | 0.85 | 23.2% | 37% |
| IOC | Oil Gas & Consumable Fuels | 2005 | 50/100 | 0.01 | -1.7% | 28% | no | 50/100 | 0.02 | -3.8% | 45% | no | -0.00 | -4.5% | 58% |
| ITC | Fast Moving Consumer Goods | 2005 | 50/100 | 0.26 | 3.1% | 27% | no | 100/200 | 0.50 | 10.7% | 47% | yes | 0.28 | 4.1% | 52% |
| JINDALSTEL | Metals & Mining | 2005 | 20/50 | 1.03 | 37.8% | 42% | yes | 20/50 | 0.59 | 18.6% | 59% | no | 0.85 | 37.3% | 68% |
| JSWENERGY | Power | 2010 | 13/34 | 1.06 | 36.4% | 38% | yes | 13/34 | 0.63 | 20.4% | 57% | no | 1.02 | 43.4% | 51% |
| JSWSTEEL | Metals & Mining | 2006 | 30/100 | 0.81 | 19.7% | 44% | yes | 30/100 | 0.30 | 4.2% | 64% | no | 0.77 | 25.9% | 55% |
| JUBLFOOD | Consumer Services | 2010 | 50/100 | 0.53 | 12.4% | 40% | no | 100/200 | 0.48 | 11.7% | 53% | no | 0.65 | 19.6% | 49% |
| KARURVYSYA | Financial Services | 2005 | 5/13 | 0.79 | 23.4% | 40% | yes | 5/13 | 0.83 | 32.3% | 49% | yes | 0.36 | 6.3% | 79% |
| KEI | Capital Goods | 2007 | 21/55 | 0.87 | 29.5% | 34% | no | 21/55 | 0.47 | 11.7% | 55% | no | 1.02 | 42.7% | 63% |
| KOTAKBANK | Financial Services | 2005 | 100/200 | 0.07 | -1.4% | 36% | no | 100/200 | -0.33 | -14.3% | 62% | no | 0.46 | 9.9% | 36% |
| LICHSGFIN | Financial Services | 2005 | 12/26 | -0.39 | -13.6% | 50% | no | 12/26 | -0.58 | -28.7% | 82% | no | 0.11 | -4.1% | 66% |
| LT | Construction | 2005 | 13/34 | 0.62 | 10.5% | 33% | yes | 13/34 | 0.36 | 6.6% | 45% | no | 0.46 | 9.6% | 56% |
| LTF | Financial Services | 2012 | 12/26 | 0.04 | -2.5% | 42% | no | 12/26 | 0.11 | -5.7% | 58% | no | -0.04 | -11.8% | 68% |
| LUPIN | Healthcare | 2005 | 100/200 | 0.27 | 3.3% | 26% | yes | 100/200 | 0.28 | 3.9% | 42% | yes | 0.04 | -3.7% | 53% |
| M&M | Automobile and Auto Components | 2005 | 13/34 | 0.52 | 10.3% | 37% | yes | 13/34 | 0.25 | 2.5% | 46% | no | 0.49 | 11.7% | 67% |
| MAHABANK | Financial Services | 2005 | 20/50 | 0.68 | 22.7% | 41% | yes | 20/50 | 0.58 | 18.6% | 53% | no | 0.59 | 19.3% | 56% |
| MANAPPURAM | Financial Services | 2011 | 50/200 | 0.09 | -3.3% | 59% | no | 50/200 | -0.20 | -17.8% | 78% | no | 0.35 | 5.6% | 63% |
| MARICO | Fast Moving Consumer Goods | 2006 | 100/200 | 0.36 | 5.4% | 22% | no | 100/200 | 0.17 | 1.1% | 43% | no | 0.43 | 8.0% | 39% |
| MARUTI | Automobile and Auto Components | 2005 | 21/55 | 0.07 | -0.7% | 31% | no | 21/55 | -0.15 | -10.5% | 52% | no | 0.26 | 3.0% | 48% |
| MCX | Financial Services | 2012 | 50/100 | 0.33 | 5.5% | 38% | no | 50/100 | -0.14 | -12.6% | 64% | no | 0.67 | 20.7% | 44% |
| MFSL | Financial Services | 2005 | 50/100 | 0.04 | -3.7% | 52% | no | 50/100 | -0.39 | -23.2% | 81% | no | 0.46 | 10.9% | 52% |
| MOTHERSON | Automobile and Auto Components | 2005 | 50/100 | 0.27 | 3.8% | 38% | yes | 50/200 | 0.30 | 2.6% | 74% | yes | 0.09 | -7.4% | 69% |
| MOTILALOFS | Financial Services | 2008 | 21/55 | -0.21 | -8.8% | 42% | no | 21/55 | -0.47 | -24.4% | 78% | no | 0.21 | 0.3% | 49% |
| MPHASIS | Information Technology | 2007 | 100/200 | 0.71 | 16.8% | 39% | yes | 100/200 | 0.43 | 9.3% | 48% | no | 0.64 | 18.1% | 46% |
| MUTHOOTFIN | Financial Services | 2012 | 13/34 | 0.07 | -2.3% | 38% | no | 13/34 | -0.51 | -24.9% | 77% | no | 0.66 | 20.0% | 46% |
| NATIONALUM | Metals & Mining | 2005 | 5/20 | 0.39 | 7.6% | 37% | yes | 5/20 | 0.28 | 2.3% | 53% | no | 0.34 | 5.1% | 59% |
| NAUKRI | Consumer Services | 2007 | 20/50 | 0.71 | 20.2% | 38% | no | 20/50 | 0.31 | 3.9% | 59% | no | 0.81 | 29.2% | 52% |
| NAVINFLUOR | Chemicals | 2008 | 50/100 | 1.04 | 38.6% | 30% | no | 50/200 | 0.84 | 29.7% | 53% | no | 1.30 | 56.1% | 30% |
| NBCC | Construction | 2013 | 100/200 | 0.29 | 4.4% | 40% | yes | 100/200 | 0.26 | 0.4% | 66% | yes | 0.05 | -9.8% | 77% |
| NESTLEIND | Fast Moving Consumer Goods | 2010 | 50/200 | 0.62 | 12.3% | 23% | no | 50/200 | 0.47 | 8.9% | 28% | no | 0.71 | 15.3% | 23% |
| NHPC | Power | 2010 | 50/100 | 0.12 | -0.1% | 37% | no | 50/100 | -0.28 | -14.2% | 61% | no | 0.48 | 11.1% | 34% |
| NMDC | Metals & Mining | 2008 | 20/50 | 0.78 | 38.7% | 32% | yes | 20/50 | 0.63 | 27.4% | 59% | no | 0.75 | 37.6% | 55% |
| NTPC | Power | 2005 | 100/200 | 0.40 | 5.9% | 30% | no | 100/200 | 0.11 | -1.1% | 50% | no | 0.40 | 7.6% | 47% |
| OBEROIRLTY | Realty | 2011 | 50/200 | 0.43 | 9.5% | 35% | no | 50/200 | 0.10 | -4.6% | 52% | no | 0.60 | 18.4% | 53% |
| OFSS | Information Technology | 2009 | 8/21 | 0.19 | 1.7% | 29% | yes | 8/21 | 0.26 | 3.4% | 39% | yes | 0.02 | -4.2% | 59% |
| OIL | Oil Gas & Consumable Fuels | 2010 | 21/55 | 0.17 | 0.9% | 47% | no | 21/55 | -0.06 | -10.9% | 65% | no | 0.31 | 4.4% | 62% |
| ONGC | Oil Gas & Consumable Fuels | 2005 | 20/50 | 0.53 | 10.7% | 29% | yes | 20/50 | 0.47 | 11.5% | 48% | yes | 0.19 | -0.7% | 66% |
| PAGEIND | Textiles | 2008 | 50/100 | 0.64 | 13.8% | 31% | yes | 50/200 | 0.26 | 3.2% | 56% | no | 0.56 | 14.3% | 37% |
| PERSISTENT | Information Technology | 2011 | 30/100 | 1.48 | 49.1% | 29% | yes | 30/100 | 0.96 | 33.7% | 53% | no | 1.42 | 58.5% | 37% |
| PETRONET | Oil Gas & Consumable Fuels | 2005 | 30/100 | -0.58 | -11.4% | 47% | no | 30/100 | -0.83 | -23.7% | 73% | no | 0.10 | -1.2% | 39% |
| PFC | Financial Services | 2007 | 50/100 | 0.06 | -2.1% | 42% | no | 50/100 | -0.27 | -14.9% | 65% | no | 0.37 | 7.2% | 45% |
| PGEL | Consumer Durables | 2012 | 12/26 | 1.25 | 61.2% | 54% | no | 12/26 | 0.55 | 15.3% | 74% | no | 1.25 | 84.2% | 79% |
| PHOENIXLTD | Realty | 2008 | 50/100 | 0.53 | 13.5% | 48% | no | 50/100 | 0.15 | -2.4% | 68% | no | 0.77 | 26.5% | 48% |
| PIDILITIND | Chemicals | 2005 | 50/200 | 0.69 | 15.7% | 28% | no | 50/200 | 0.43 | 8.3% | 34% | no | 0.91 | 23.3% | 28% |
| PIIND | Chemicals | 2012 | 50/100 | 0.89 | 25.4% | 37% | no | 50/100 | 0.41 | 8.5% | 57% | no | 1.19 | 41.7% | 37% |
| PNB | Financial Services | 2005 | 5/13 | 0.48 | 10.2% | 37% | yes | 5/13 | 0.67 | 21.9% | 44% | yes | 0.02 | -8.2% | 73% |
| POWERGRID | Power | 2008 | 50/100 | -0.00 | -2.5% | 31% | no | 50/100 | -0.46 | -15.7% | 51% | no | 0.47 | 9.6% | 33% |
| PRESTIGE | Realty | 2011 | 50/100 | 0.29 | 3.5% | 55% | no | 50/100 | -0.14 | -18.2% | 73% | no | 0.63 | 20.5% | 68% |
| RADICO | Fast Moving Consumer Goods | 2005 | 13/34 | 0.88 | 24.9% | 35% | yes | 13/34 | 0.55 | 15.4% | 49% | no | 0.78 | 26.3% | 45% |
| RECLTD | Financial Services | 2009 | 13/34 | 0.19 | 1.5% | 36% | no | 13/34 | -0.04 | -7.8% | 56% | no | 0.35 | 6.3% | 52% |
| RELIANCE | Oil Gas & Consumable Fuels | 2005 | 100/200 | 0.58 | 13.9% | 45% | no | 100/200 | 0.30 | 4.5% | 55% | no | 0.79 | 22.9% | 45% |
| SAIL | Metals & Mining | 2005 | 21/55 | 0.46 | 10.2% | 45% | yes | 5/13 | 0.18 | -3.8% | 60% | no | 0.44 | 9.9% | 66% |
| SBIN | Financial Services | 2005 | 5/13 | 1.04 | 25.7% | 21% | yes | 5/13 | 0.80 | 25.3% | 30% | yes | 0.68 | 20.0% | 59% |
| SHREECEM | Construction Materials | 2005 | 9/21 | 0.22 | 2.4% | 33% | no | 10/30 | -0.34 | -14.2% | 64% | no | 0.40 | 7.8% | 43% |
| SHRIRAMFIN | Financial Services | 2005 | 9/21 | -0.15 | -9.7% | 59% | no | 9/21 | -0.46 | -31.2% | 84% | no | 0.31 | 2.5% | 66% |
| SIEMENS | Capital Goods | 2005 | 50/100 | 1.18 | 32.1% | 23% | yes | 50/100 | 1.08 | 32.9% | 24% | yes | 0.97 | 28.4% | 41% |
| SOLARINDS | Chemicals | 2010 | 10/30 | 1.22 | 33.7% | 22% | no | 10/30 | 0.78 | 22.3% | 34% | no | 1.24 | 42.3% | 34% |
| SRF | Chemicals | 2005 | 5/13 | 0.84 | 20.0% | 35% | no | 100/200 | 1.41 | 55.6% | 37% | no | 1.40 | 55.5% | 37% |
| SUNPHARMA | Healthcare | 2005 | 50/100 | 0.66 | 13.0% | 22% | no | 50/100 | 0.11 | -1.2% | 55% | no | 0.85 | 23.6% | 32% |
| SUPREMEIND | Capital Goods | 2005 | 5/20 | 0.47 | 8.9% | 36% | no | 100/200 | -0.25 | -12.3% | 68% | no | 0.74 | 20.3% | 43% |
| SUZLON | Capital Goods | 2006 | 13/34 | 0.09 | -6.3% | 71% | no | 13/34 | -0.48 | -46.3% | 96% | no | 0.61 | 20.8% | 78% |
| TATACONSUM | Fast Moving Consumer Goods | 2011 | 100/200 | 1.04 | 31.3% | 43% | no | 100/200 | 0.83 | 24.1% | 43% | no | 1.13 | 37.1% | 43% |
| TATAELXSI | Information Technology | 2005 | 30/100 | 1.32 | 53.2% | 52% | yes | 50/100 | 1.18 | 51.3% | 66% | no | 1.29 | 58.3% | 50% |
| TATAPOWER | Power | 2005 | 50/200 | 1.18 | 39.1% | 33% | yes | 100/200 | 0.97 | 37.6% | 62% | yes | 0.81 | 28.5% | 65% |
| TATASTEEL | Metals & Mining | 2006 | 21/55 | 0.86 | 22.8% | 49% | yes | 21/55 | 0.52 | 13.7% | 66% | no | 0.67 | 21.3% | 54% |
| TCS | Information Technology | 2005 | 100/200 | 0.27 | 3.5% | 33% | no | 100/200 | -0.20 | -8.6% | 56% | no | 0.65 | 14.6% | 28% |
| TECHM | Information Technology | 2007 | 30/100 | 0.83 | 17.3% | 28% | yes | 12/26 | 0.93 | 27.8% | 31% | yes | 0.44 | 9.2% | 47% |
| TITAN | Consumer Durables | 2005 | 50/100 | 0.53 | 10.8% | 49% | no | 100/200 | 0.51 | 11.9% | 55% | no | 0.97 | 29.6% | 42% |
| TMPV | Automobile and Auto Components | 2005 | 50/100 | 0.49 | 11.8% | 44% | no | 50/100 | 0.03 | -11.4% | 75% | no | 0.65 | 22.5% | 72% |
| TORNTPHARM | Healthcare | 2005 | 100/200 | 0.49 | 10.2% | 27% | no | 100/200 | 0.30 | 4.6% | 38% | no | 0.62 | 14.9% | 25% |
| TRENT | Consumer Services | 2005 | 8/21 | 0.70 | 17.9% | 25% | no | 8/21 | 0.05 | -5.5% | 42% | no | 1.04 | 39.5% | 49% |
| TVSMOTOR | Automobile and Auto Components | 2005 | 10/30 | 0.63 | 13.2% | 30% | yes | 10/30 | 0.26 | 2.8% | 44% | no | 0.63 | 17.6% | 56% |
| ULTRACEMCO | Construction Materials | 2005 | 5/20 | 0.48 | 7.9% | 43% | no | 5/13 | 0.22 | 2.0% | 54% | no | 0.61 | 15.0% | 38% |
| UNIONBANK | Financial Services | 2005 | 21/55 | 0.12 | -1.4% | 60% | no | 21/55 | 0.01 | -10.1% | 74% | no | 0.19 | -2.0% | 77% |
| UNITDSPR | Fast Moving Consumer Goods | 2005 | 30/100 | -0.32 | -9.4% | 46% | no | 30/100 | -0.84 | -27.9% | 76% | no | 0.41 | 8.5% | 39% |
| UNOMINDA | Automobile and Auto Components | 2007 | 20/50 | 0.95 | 28.4% | 24% | yes | 20/50 | 0.48 | 11.9% | 49% | no | 0.90 | 34.8% | 49% |
| UPL | Chemicals | 2005 | 100/200 | 0.11 | -0.6% | 34% | no | 100/200 | -0.27 | -16.0% | 73% | no | 0.42 | 9.1% | 64% |
| VEDL | Metals & Mining | 2005 | 30/100 | 0.73 | 20.7% | 34% | yes | 30/100 | 0.38 | 6.6% | 72% | no | 0.62 | 19.8% | 70% |
| VOLTAS | Consumer Durables | 2005 | 20/50 | 0.21 | 2.1% | 34% | no | 20/50 | -0.13 | -8.8% | 56% | no | 0.45 | 9.6% | 42% |
| WIPRO | Information Technology | 2005 | 100/200 | 0.64 | 12.4% | 35% | yes | 10/30 | 0.87 | 24.3% | 28% | yes | 0.53 | 12.2% | 48% |
| YESBANK | Financial Services | 2006 | 13/34 | -0.93 | -34.4% | 89% | no | 13/34 | -0.58 | -60.8% | 99% | no | -0.21 | -42.6% | 96% |
| ZYDUSLIFE | Healthcare | 2005 | 12/26 | 0.35 | 5.4% | 23% | yes | 12/26 | 0.20 | 1.2% | 55% | no | 0.30 | 4.7% | 50% |

## Not tested (70)

Where a stock had some training days, the count is in brackets.

360ONE, ABCAPITAL (129 train days), ADANIENSOL (644 train days), ADANIGREEN, ALKEM (547 train days), AMBER (26 train days), ANANDRATHI, ANGELONE, ATHERENERG, AUBANK (166 train days), BANDHANBNK, BDL, BSE (271 train days), CAMS, CDSL (172 train days), COCHINSHIP (142 train days), CROMPTON (453 train days), DATAPATTNS, DELHIVERY, DIXON (118 train days), DMART (241 train days), ENRIN, ETERNAL, FORCEMOT, HAL, HDFCAMC, HDFCLIFE (76 train days), HYUNDAI, ICICIGI (111 train days), ICICILIFE (358 train days), IDFCFIRSTB (578 train days), IEX (95 train days), INDIGO (576 train days), INOXWIND (723 train days), IREDA, IRFC, JIOFIN, KALYANKJIL, KAYNES, KFINTECH, KPITTECH, LAURUSLABS (304 train days), LICI, LODHA, LTM (405 train days), MANKIND, MAXHEALTH, MAZDOCK, NAM-INDIA (85 train days), NYKAA, PATANJALI, PAYTM, PNBHOUSING (333 train days), POLICYBZR, POLYCAB, POWERINDIA, PREMIERENE, RBLBANK (377 train days), RVNL, SAGILITY, SBICARD, SBILIFE (108 train days), SONACOMS, SWIGGY, SYRMA, TIINDIA (87 train days), UJJIVANSFB, VBL (332 train days), VMM, WAAREEENER
