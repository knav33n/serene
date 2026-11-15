# Daily EMA crossovers on each Nifty 500 stock

Researched 2026-11-15. Code: `serene research stockema` (`cmd/serene/stockema.go`).
Per-stock numbers for every pair: `reports/nifty500_ema.csv`.

## Question

For each Nifty 500 stock, which EMA crossover works?

## Short answer

None of them reliably does. In the training years each stock has a best
crossover. On later data, that pair does about as well as a pair picked at
random. Buy and hold beat almost every crossover on both return and Sharpe,
for the typical stock. What a crossover does buy you is a smaller drawdown:
about 40% for a typical stock against 56% for buy and hold over 2019–2022.
So the "best EMA for this stock" column below is mostly a description of the
past. It is not a forecast, and it shouldn't be traded as one without
further evidence.

## Method

- Universe: today's 501 Nifty 500 members (`configs/nifty500.csv`, fetched
  from niftyindices.com on 2026-11-15). A renamed company's older symbol is
  joined by ISIN (44 stocks). Using today's members means past losers that
  left the index are missing (survivorship bias). That flatters buy and
  hold more than the crossovers.
- Prices: NSE daily bars, adjusted for splits, bonuses, demergers and rights
  (`EquityPanel`, `factor_all`).
- Rule: long only, because cash stocks can't be held short overnight. Hold
  the stock while the fast EMA is above the slow EMA at the close. Every
  change fills at the next day's open. Money out of the market earns
  nothing.
- Costs: 0.24% round trip for delivery charges, plus 10 bps slippage on each
  side.
- Pairs: 5/13, 5/20, 8/21, 9/21, 10/30, 12/26, 13/34, 20/50, 21/55, 30/100,
  50/100, 50/200 and 100/200. Each is compared with buy and hold over the
  same days.
- Train: 2005-01-01 (or the first day a 200-day EMA exists) to 2018-12-31.
  The pick is each stock's pair with the best Sharpe on daily returns.
  Test: 2019-01-01 to 2022-12-31. Nothing was chosen on the test years.
- Holdout: stock data from 2023-01-01 is held back (as for `stockdiv`) and
  was not touched.
- Stocks need at least 3 years of training data and 3 years of test data.
  303 of the 501 qualify. The rest listed too late; they are listed at the
  end.

## Results across stocks, 2019–2022 test (medians per stock)

| pair | train CAGR | test CAGR | train Sharpe | test Sharpe | test max DD | trades/yr | time in market | beat B&H Sharpe | picked as best |
|---|---|---|---|---|---|---|---|---|---|
| 5/13 | 7.8% | 7.2% | 0.40 | 0.39 | 40% | 9.1 | 54% | 34% of stocks | 19 |
| 5/20 | 8.4% | 7.6% | 0.43 | 0.40 | 39% | 7.4 | 55% | 37% of stocks | 16 |
| 8/21 | 9.7% | 8.9% | 0.47 | 0.46 | 38% | 5.8 | 56% | 37% of stocks | 12 |
| 9/21 | 9.8% | 8.6% | 0.49 | 0.44 | 38% | 5.6 | 56% | 38% of stocks | 14 |
| 10/30 | 10.8% | 9.5% | 0.48 | 0.44 | 38% | 4.3 | 57% | 38% of stocks | 10 |
| 12/26 | 10.5% | 10.0% | 0.49 | 0.47 | 37% | 4.3 | 57% | 36% of stocks | 15 |
| 13/34 | 11.2% | 8.8% | 0.51 | 0.43 | 38% | 3.8 | 58% | 38% of stocks | 22 |
| 20/50 | 11.9% | 7.4% | 0.53 | 0.39 | 40% | 2.5 | 59% | 36% of stocks | 21 |
| 21/55 | 11.8% | 7.3% | 0.52 | 0.38 | 40% | 2.3 | 59% | 33% of stocks | 34 |
| 30/100 | 12.2% | 9.6% | 0.54 | 0.47 | 40% | 1.5 | 58% | 33% of stocks | 28 |
| 50/100 | 12.1% | 9.1% | 0.54 | 0.45 | 40% | 1.0 | 58% | 35% of stocks | 41 |
| 50/200 | 12.1% | 10.0% | 0.52 | 0.47 | 41% | 0.8 | 59% | 36% of stocks | 29 |
| 100/200 | 11.3% | 10.0% | 0.51 | 0.48 | 41% | 0.5 | 57% | 40% of stocks | 42 |
| B&H | 14.4% | 14.7% | 0.54 | 0.55 | 56% | 0.3 | 100% | 0% of stocks | 0 |

How each stock's own training pick did on the test years:

- Median test Sharpe was 0.45; buy and hold's was 0.55. Median test CAGR was
  9.7%; buy and hold's was 14.7%.
- 240 of 303 picks made money. 118 of 303 beat buy and hold's Sharpe, which
  is about what the average pair manages anyway (33–40%).
- **Persistence:** among the 13 pairs, the pick's average test rank was 0.52,
  where 0.50 is what chance gives. That is 1.3 standard errors from 0.50, so
  it is not significant. The training winner landed in the test's top 3 for
  27% of stocks; chance gives 23%.

### Check: an earlier split (train 2005–2012, test 2013–2022, 246 stocks)

| | crossover pick | buy and hold |
|---|---|---|
| median test Sharpe | 0.46 | 0.58 |
| median test CAGR | 9.0% | 16.1% |
| stocks where the pick beat B&H's Sharpe | 71 of 246 | |
| pick's average test rank (0.50 = chance) | 0.51 (0.5 standard errors) | |
| pick in the test's top 3 | 26% (chance 23%) | |

On this split the fast pairs (5/13, 5/20) looked best over 2005–2012, with
its 2008 crash. They were the worst over 2013–2022. The slow pairs (50/100,
50/200, 100/200) are the most stable across both splits, and their test
Sharpe comes closest to buy and hold.

## What to take from it

1. Fitting a pair to each stock doesn't carry forward. Using one slow pair
   for everything does as well or better, with less curve-fitting.
2. A long-only crossover on Indian stocks is a drawdown tool, not a
   return tool. Over 2019–2022 a typical stock held through a 56% fall
   under buy and hold, against about 40% with a crossover. In exchange the
   crossover gave up about 4–6 points of CAGR a year.
3. Next ideas worth testing: a crossover as a market-level filter (NIFTY
   500 index above its 50/200, then hold stocks) rather than stock by stock;
   or crossovers ranked across stocks (momentum) instead of timed on each
   stock alone.

## Per stock

Pick = the pair with the best training Sharpe (2005–2018). Rank = its test
Sharpe percentile among the 13 pairs, where 1 is best. Test best = the pair
that did best on 2019–2022, known only in hindsight. Held = the pick made
money on the test years and beat buy and hold's test Sharpe; 112 of 303 did.
A "yes" is mostly luck, given the persistence numbers above.

| stock | industry | train from | pick | train Sharpe | test Sharpe | test CAGR | test max DD | B&H test Sharpe | B&H test CAGR | B&H test max DD | rank | test best | held |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| AARTIIND | Chemicals | 2005 | 30/100 | 0.77 | 0.36 | 6.6% | 45% | 0.55 | 14.0% | 49% | 0.67 | 50/200 | no |
| ABB | Capital Goods | 2005 | 10/30 | 0.95 | 0.91 | 22.6% | 23% | 0.88 | 26.9% | 47% | 0.83 | 5/20 | yes |
| ABBOTINDIA | Healthcare | 2010 | 100/200 | 1.03 | 0.93 | 23.6% | 33% | 1.08 | 30.1% | 33% | 0.75 | 50/100 | no |
| ABREL | Realty | 2005 | 13/34 | 0.77 | -0.02 | -11.2% | 69% | 0.19 | -6.4% | 79% | 0.25 | 5/20 | no |
| ACC | Construction Materials | 2005 | 21/55 | 0.53 | 0.33 | 5.0% | 31% | 0.55 | 12.7% | 48% | 0.83 | 50/100 | no |
| ACE | Capital Goods | 2007 | 50/200 | 0.75 | 1.04 | 39.3% | 36% | 0.82 | 35.2% | 73% | 0.92 | 100/200 | yes |
| ADANIENT | Metals & Mining | 2007 | 5/20 | 1.13 | 1.86 | 89.2% | 36% | 1.92 | 123.7% | 53% | 0.50 | 10/30 | no |
| ADANIPORTS | Services | 2008 | 30/100 | 0.45 | 0.35 | 6.1% | 41% | 0.69 | 20.8% | 51% | 0.50 | 100/200 | no |
| ADANIPOWER | Power | 2010 | 10/30 | 0.37 | 1.05 | 47.4% | 44% | 1.06 | 56.5% | 64% | 0.42 | 8/21 | no |
| AEGISLOG | Oil Gas & Consumable Fuels | 2005 | 20/50 | 1.07 | 0.27 | 3.6% | 42% | 0.51 | 14.3% | 56% | 0.92 | 21/55 | no |
| AIAENG | Capital Goods | 2006 | 10/30 | 1.03 | 0.04 | -2.2% | 30% | 0.48 | 11.1% | 36% | 0.75 | 13/34 | no |
| AJANTPHARM | Healthcare | 2005 | 100/200 | 1.02 | 0.63 | 13.4% | 31% | 0.50 | 11.4% | 31% | 1.00 | 100/200 | yes |
| AMBUJACEM | Construction Materials | 2008 | 13/34 | 0.30 | 0.38 | 6.5% | 38% | 0.81 | 23.9% | 42% | 0.08 | 100/200 | no |
| ANANTRAJ | Realty | 2007 | 8/21 | 0.20 | 0.78 | 27.0% | 33% | 1.03 | 52.4% | 63% | 0.17 | 5/13 | no |
| APARINDS | Capital Goods | 2005 | 21/55 | 0.75 | 1.05 | 36.0% | 34% | 0.78 | 28.7% | 65% | 0.75 | 100/200 | yes |
| APLAPOLLO | Capital Goods | 2012 | 50/100 | 1.35 | 0.93 | 32.4% | 49% | 1.51 | 75.9% | 49% | 0.00 | 8/21 | no |
| APOLLOHOSP | Healthcare | 2005 | 100/200 | 0.65 | 0.77 | 24.1% | 41% | 1.04 | 38.0% | 40% | 0.83 | 13/34 | no |
| APOLLOTYRE | Automobile and Auto Components | 2005 | 12/26 | 0.67 | 0.41 | 7.9% | 37% | 0.40 | 8.2% | 68% | 0.58 | 50/100 | yes |
| ARE&M | Automobile and Auto Components | 2005 | 20/50 | 1.19 | 0.21 | 2.3% | 25% | -0.05 | -6.5% | 56% | 0.75 | 9/21 | yes |
| ASAHIINDIA | Automobile and Auto Components | 2005 | 20/50 | 0.59 | 0.68 | 19.8% | 41% | 0.62 | 19.5% | 51% | 0.92 | 21/55 | yes |
| ASHOKLEY | Capital Goods | 2005 | 50/100 | 0.78 | 0.33 | 5.4% | 38% | 0.41 | 8.7% | 67% | 0.50 | 100/200 | no |
| ASIANPAINT | Consumer Durables | 2005 | 100/200 | 1.09 | 0.63 | 14.5% | 35% | 0.87 | 22.7% | 28% | 1.00 | 100/200 | no |
| ASTRAL | Capital Goods | 2008 | 10/30 | 1.61 | 0.29 | 4.4% | 36% | 0.87 | 30.6% | 39% | 0.00 | 20/50 | no |
| ATUL | Chemicals | 2005 | 5/20 | 0.97 | 0.70 | 14.3% | 33% | 0.87 | 24.6% | 35% | 0.75 | 5/13 | no |
| AUROPHARMA | Healthcare | 2005 | 20/50 | 1.17 | -0.46 | -16.1% | 51% | -0.10 | -12.4% | 65% | 0.08 | 9/21 | no |
| AVANTIFEED | Fast Moving Consumer Goods | 2011 | 20/50 | 1.75 | 0.30 | 4.7% | 35% | 0.21 | -0.1% | 65% | 0.67 | 5/13 | yes |
| AXISBANK | Financial Services | 2008 | 20/50 | 0.62 | 0.51 | 10.2% | 35% | 0.46 | 10.7% | 63% | 0.58 | 9/21 | yes |
| BAJAJ-AUTO | Automobile and Auto Components | 2009 | 20/50 | 0.97 | 0.21 | 2.3% | 31% | 0.39 | 7.2% | 41% | 0.58 | 9/21 | no |
| BAJAJFINSV | Financial Services | 2009 | 9/21 | 1.11 | 1.44 | 40.3% | 28% | 0.76 | 24.4% | 59% | 0.75 | 12/26 | yes |
| BAJAJHLDNG | Financial Services | 2009 | 21/55 | 0.90 | 0.26 | 3.4% | 38% | 0.65 | 18.1% | 61% | 0.00 | 5/20 | no |
| BAJFINANCE | Financial Services | 2011 | 13/34 | 1.75 | 1.29 | 38.6% | 26% | 0.76 | 25.9% | 63% | 0.67 | 5/13 | yes |
| BALKRISIND | Automobile and Auto Components | 2006 | 21/55 | 1.09 | 0.53 | 10.8% | 39% | 0.78 | 23.5% | 45% | 0.25 | 5/20 | no |
| BALRAMCHIN | Fast Moving Consumer Goods | 2005 | 12/26 | 0.68 | 0.90 | 30.3% | 27% | 0.94 | 40.8% | 62% | 0.92 | 5/20 | no |
| BANKBARODA | Financial Services | 2005 | 12/26 | 0.22 | 0.94 | 28.3% | 33% | 0.47 | 11.7% | 74% | 0.83 | 5/13 | yes |
| BANKINDIA | Financial Services | 2005 | 13/34 | 0.21 | 0.34 | 6.0% | 46% | 0.14 | -4.3% | 71% | 0.50 | 5/13 | yes |
| BATAINDIA | Consumer Durables | 2005 | 30/100 | 0.75 | 0.31 | 4.5% | 37% | 0.47 | 9.9% | 43% | 0.25 | 12/26 | no |
| BBOX | Information Technology | 2011 | 50/100 | 0.42 | 1.31 | 56.7% | 52% | 1.26 | 75.1% | 64% | 0.58 | 5/20 | yes |
| BEL | Capital Goods | 2005 | 50/100 | 0.79 | 0.94 | 28.1% | 31% | 0.97 | 36.2% | 51% | 0.83 | 5/20 | no |
| BEML | Capital Goods | 2005 | 5/20 | 1.00 | 0.55 | 14.0% | 41% | 0.49 | 12.9% | 63% | 0.92 | 5/13 | yes |
| BERGEPAINT | Consumer Durables | 2005 | 100/200 | 0.86 | 0.89 | 21.8% | 31% | 0.64 | 15.3% | 35% | 0.92 | 8/21 | yes |
| BHARATFORG | Automobile and Auto Components | 2005 | 50/100 | 0.83 | 0.56 | 12.0% | 37% | 0.54 | 14.7% | 59% | 0.92 | 30/100 | yes |
| BHARTIARTL | Telecommunication | 2007 | 30/100 | 0.07 | 0.72 | 18.9% | 28% | 0.87 | 26.7% | 34% | 1.00 | 30/100 | no |
| BHEL | Capital Goods | 2005 | 50/200 | 0.53 | 0.54 | 12.6% | 41% | 0.29 | 1.7% | 74% | 0.42 | 5/13 | yes |
| BIOCON | Healthcare | 2005 | 100/200 | 0.62 | 0.26 | 3.5% | 31% | 0.02 | -4.7% | 46% | 0.83 | 50/100 | yes |
| BLUESTARCO | Consumer Durables | 2005 | 5/20 | 1.07 | 0.39 | 6.7% | 32% | 0.66 | 17.7% | 49% | 0.33 | 12/26 | no |
| BOSCHLTD | Automobile and Auto Components | 2008 | 13/34 | 1.02 | 0.24 | 2.9% | 29% | 0.07 | -3.4% | 58% | 0.67 | 12/26 | yes |
| BPCL | Oil Gas & Consumable Fuels | 2005 | 30/100 | 0.62 | -0.15 | -6.0% | 45% | 0.13 | -2.3% | 52% | 0.50 | 5/13 | no |
| BRIGADE | Realty | 2008 | 21/55 | 0.66 | 0.87 | 28.8% | 32% | 0.89 | 33.9% | 62% | 0.83 | 13/34 | no |
| BRITANNIA | Fast Moving Consumer Goods | 2005 | 10/30 | 1.18 | 0.30 | 3.8% | 24% | 0.44 | 8.4% | 36% | 0.75 | 5/20 | no |
| BSOFT | Information Technology | 2005 | 8/21 | 0.63 | 0.57 | 15.9% | 54% | 0.70 | 25.1% | 60% | 0.33 | 100/200 | no |
| CANBK | Financial Services | 2005 | 50/100 | 0.33 | 0.50 | 11.6% | 41% | 0.33 | 4.7% | 74% | 0.33 | 5/13 | yes |
| CANFINHOME | Financial Services | 2005 | 30/100 | 0.85 | -0.13 | -9.7% | 61% | 0.58 | 17.1% | 48% | 0.25 | 8/21 | no |
| CAPLIPOINT | Healthcare | 2015 | 30/100 | 0.83 | 0.48 | 11.3% | 34% | 0.56 | 17.0% | 58% | 0.92 | 5/20 | no |
| CARBORUNIV | Capital Goods | 2005 | 20/50 | 0.72 | 0.79 | 20.0% | 33% | 0.80 | 25.6% | 54% | 0.83 | 13/34 | no |
| CASTROLIND | Oil Gas & Consumable Fuels | 2015 | 50/200 | -0.54 | -0.65 | -10.3% | 38% | -0.02 | -5.3% | 47% | 0.17 | 5/13 | no |
| CCL | Fast Moving Consumer Goods | 2005 | 21/55 | 0.86 | 0.34 | 5.8% | 37% | 0.64 | 18.7% | 50% | 0.58 | 100/200 | no |
| CEATLTD | Automobile and Auto Components | 2008 | 5/13 | 1.18 | 0.81 | 19.3% | 28% | 0.34 | 5.9% | 53% | 1.00 | 5/13 | yes |
| CEMPRO | Construction | 2006 | 21/55 | 0.76 | 0.41 | 9.0% | 39% | 0.29 | 1.2% | 80% | 0.75 | 50/200 | yes |
| CENTRALBK | Financial Services | 2008 | 5/13 | 0.82 | -0.00 | -7.3% | 55% | 0.17 | -3.9% | 73% | 0.17 | 30/100 | no |
| CESC | Power | 2005 | 100/200 | 0.62 | -0.04 | -2.4% | 28% | 0.26 | 3.3% | 55% | 0.75 | 5/13 | no |
| CGCL | Financial Services | 2011 | 50/200 | 0.52 | 1.05 | 37.9% | 46% | 1.27 | 56.8% | 48% | 0.42 | 50/100 | no |
| CGPOWER | Capital Goods | 2005 | 30/100 | 0.88 | 2.58 | 134.9% | 25% | 1.08 | 57.5% | 90% | 1.00 | 30/100 | yes |
| CHAMBLFERT | Chemicals | 2005 | 50/100 | 0.37 | 0.21 | 1.6% | 60% | 0.65 | 20.7% | 48% | 0.08 | 5/20 | no |
| CHENNPETRO | Oil Gas & Consumable Fuels | 2005 | 9/21 | 0.70 | 0.81 | 27.1% | 37% | 0.09 | -9.0% | 84% | 0.92 | 8/21 | yes |
| CHOLAFIN | Financial Services | 2011 | 50/100 | 0.93 | 0.55 | 14.0% | 42% | 0.79 | 30.5% | 64% | 0.42 | 13/34 | no |
| CIEINDIA | Automobile and Auto Components | 2008 | 50/200 | 0.50 | 0.71 | 17.6% | 36% | 0.38 | 7.5% | 76% | 0.83 | 50/100 | yes |
| CIPLA | Healthcare | 2005 | 12/26 | 0.39 | 0.40 | 6.3% | 26% | 0.79 | 20.1% | 35% | 0.33 | 50/200 | no |
| COALINDIA | Oil Gas & Consumable Fuels | 2011 | 50/100 | -0.19 | 0.14 | 0.4% | 34% | 0.12 | -1.8% | 59% | 0.67 | 100/200 | yes |
| COFORGE | Information Technology | 2005 | 100/200 | 0.66 | 0.70 | 22.6% | 57% | 0.90 | 36.2% | 57% | 0.17 | 9/21 | no |
| COLPAL | Fast Moving Consumer Goods | 2008 | 21/55 | 0.83 | -0.34 | -7.4% | 38% | 0.25 | 3.2% | 32% | 0.25 | 50/100 | no |
| CONCOR | Services | 2005 | 50/200 | 0.72 | -0.28 | -9.8% | 42% | 0.38 | 7.6% | 56% | 0.08 | 5/13 | no |
| COROMANDEL | Chemicals | 2010 | 21/55 | 0.60 | 0.33 | 5.2% | 33% | 0.71 | 18.5% | 32% | 0.33 | 13/34 | no |
| CRISIL | Financial Services | 2005 | 50/200 | 0.87 | 0.36 | 6.5% | 42% | 0.64 | 17.8% | 42% | 0.33 | 21/55 | no |
| CUB | Financial Services | 2005 | 12/26 | 0.91 | -0.10 | -4.9% | 50% | 0.13 | -2.1% | 55% | 0.25 | 13/34 | no |
| CUMMINSIND | Capital Goods | 2006 | 20/50 | 0.78 | 0.69 | 15.4% | 33% | 0.53 | 13.1% | 67% | 0.42 | 50/100 | yes |
| CYIENT | Information Technology | 2005 | 50/200 | 0.89 | 0.95 | 23.9% | 36% | 0.36 | 6.8% | 73% | 0.58 | 10/30 | yes |
| DABUR | Fast Moving Consumer Goods | 2005 | 50/200 | 0.89 | 0.01 | -2.0% | 28% | 0.39 | 6.8% | 26% | 1.00 | 50/200 | no |
| DEEPAKFERT | Chemicals | 2005 | 5/13 | 0.54 | 1.19 | 50.3% | 35% | 1.01 | 48.8% | 61% | 0.83 | 50/200 | yes |
| DEEPAKNTR | Chemicals | 2011 | 100/200 | 1.18 | 1.24 | 55.0% | 49% | 1.48 | 74.1% | 41% | 0.67 | 50/100 | no |
| DIVISLAB | Healthcare | 2005 | 9/21 | 0.82 | 0.27 | 3.5% | 39% | 0.85 | 23.4% | 39% | 0.42 | 50/100 | no |
| DLF | Realty | 2008 | 8/21 | 0.29 | 0.85 | 23.8% | 29% | 0.65 | 20.8% | 54% | 0.92 | 9/21 | yes |
| DRREDDY | Healthcare | 2005 | 100/200 | 0.52 | 0.71 | 15.3% | 22% | 0.58 | 12.9% | 33% | 0.92 | 5/13 | yes |
| ECLERX | Services | 2008 | 30/100 | 0.91 | 0.60 | 16.0% | 47% | 0.55 | 15.8% | 69% | 0.08 | 13/34 | yes |
| EICHERMOT | Automobile and Auto Components | 2005 | 13/34 | 1.27 | 0.23 | 2.6% | 41% | 0.41 | 8.7% | 45% | 0.50 | 10/30 | no |
| EIDPARRY | Fast Moving Consumer Goods | 2005 | 21/55 | 0.67 | 0.31 | 4.9% | 35% | 0.79 | 29.2% | 55% | 0.17 | 8/21 | no |
| EIHOTEL | Consumer Services | 2005 | 100/200 | 0.49 | 0.18 | 0.9% | 44% | 0.15 | -2.7% | 73% | 0.42 | 50/200 | yes |
| ELECON | Capital Goods | 2007 | 10/30 | 0.41 | 1.23 | 62.3% | 47% | 0.99 | 51.8% | 76% | 0.50 | 100/200 | yes |
| ELGIEQUIP | Capital Goods | 2005 | 50/100 | 0.59 | 0.73 | 23.8% | 43% | 0.86 | 34.2% | 66% | 0.58 | 50/200 | no |
| EMAMILTD | Fast Moving Consumer Goods | 2007 | 21/55 | 0.74 | 0.77 | 15.7% | 29% | 0.19 | 0.3% | 65% | 1.00 | 21/55 | yes |
| ENGINERSIN | Construction | 2005 | 8/21 | 0.64 | -0.36 | -10.6% | 51% | -0.16 | -11.1% | 60% | 0.75 | 50/100 | no |
| ESCORTS | Capital Goods | 2005 | 5/20 | 0.72 | 0.84 | 23.0% | 34% | 0.87 | 32.2% | 47% | 0.75 | 100/200 | no |
| EXIDEIND | Automobile and Auto Components | 2006 | 100/200 | 0.64 | -0.77 | -8.9% | 34% | -0.19 | -9.7% | 53% | 0.00 | 5/13 | no |
| FACT | Chemicals | 2005 | 50/200 | 0.14 | 0.80 | 33.5% | 54% | 1.18 | 74.7% | 54% | 0.17 | 12/26 | no |
| FEDERALBNK | Financial Services | 2005 | 21/55 | 0.61 | 0.37 | 6.8% | 39% | 0.45 | 10.4% | 66% | 0.33 | 12/26 | no |
| FINCABLES | Capital Goods | 2005 | 13/34 | 1.04 | 0.41 | 8.0% | 41% | 0.31 | 4.8% | 66% | 0.92 | 5/13 | yes |
| FORTIS | Healthcare | 2008 | 50/200 | 0.32 | 0.43 | 8.7% | 34% | 0.70 | 19.6% | 31% | 0.75 | 9/21 | no |
| FSL | Services | 2007 | 20/50 | 0.47 | 0.73 | 21.5% | 56% | 0.65 | 21.5% | 59% | 0.67 | 30/100 | yes |
| GABRIEL | Automobile and Auto Components | 2006 | 100/200 | 0.75 | 0.34 | 5.6% | 33% | 0.37 | 7.0% | 72% | 0.42 | 20/50 | no |
| GAIL | Oil Gas & Consumable Fuels | 2005 | 50/200 | 0.45 | -0.43 | -10.1% | 40% | 0.02 | -5.7% | 62% | 0.33 | 100/200 | no |
| GALLANTT | Capital Goods | 2007 | 5/13 | 0.60 | 0.13 | -2.7% | 50% | 0.29 | 1.7% | 66% | 0.08 | 20/50 | no |
| GESHIP | Services | 2007 | 30/100 | 0.19 | 0.40 | 7.9% | 42% | 0.66 | 20.9% | 49% | 0.17 | 20/50 | no |
| GILLETTE | Fast Moving Consumer Goods | 2007 | 5/13 | 0.86 | -0.73 | -10.2% | 42% | -0.26 | -6.5% | 43% | 0.08 | 20/50 | no |
| GLAXO | Healthcare | 2005 | 5/20 | 0.57 | -0.22 | -5.2% | 38% | -0.01 | -3.3% | 35% | 0.50 | 13/34 | no |
| GLENMARK | Healthcare | 2005 | 50/200 | 0.56 | -0.28 | -7.8% | 43% | -0.09 | -11.7% | 73% | 0.17 | 21/55 | no |
| GMDCLTD | Metals & Mining | 2005 | 21/55 | 0.83 | 0.24 | 1.9% | 57% | 0.50 | 13.4% | 67% | 0.00 | 5/13 | no |
| GMRAIRPORT | Services | 2007 | 5/13 | 0.20 | 0.20 | 1.5% | 40% | 0.74 | 24.9% | 38% | 0.33 | 8/21 | no |
| GODFRYPHLP | Fast Moving Consumer Goods | 2005 | 5/20 | 0.65 | 0.45 | 9.7% | 46% | 0.70 | 22.6% | 48% | 0.67 | 21/55 | no |
| GODREJCP | Fast Moving Consumer Goods | 2005 | 100/200 | 0.93 | -0.43 | -11.6% | 48% | 0.21 | 1.7% | 47% | 0.08 | 5/20 | no |
| GODREJIND | Diversified | 2005 | 9/21 | 0.87 | -0.13 | -4.4% | 31% | -0.06 | -6.2% | 57% | 0.17 | 50/100 | no |
| GODREJPROP | Realty | 2010 | 12/26 | 0.77 | 0.46 | 10.0% | 38% | 0.58 | 17.2% | 54% | 0.25 | 5/20 | no |
| GPIL | Capital Goods | 2007 | 20/50 | 0.92 | 1.29 | 57.6% | 61% | 0.99 | 51.1% | 71% | 0.50 | 10/30 | yes |
| GRANULES | Healthcare | 2006 | 13/34 | 0.86 | 0.69 | 19.0% | 40% | 0.95 | 37.7% | 45% | 0.50 | 10/30 | no |
| GRAPHITE | Capital Goods | 2005 | 10/30 | 0.93 | -0.03 | -6.2% | 66% | -0.06 | -16.5% | 86% | 0.17 | 50/100 | no |
| GRASIM | Construction Materials | 2005 | 8/21 | 0.74 | 1.25 | 29.7% | 24% | 0.71 | 20.3% | 58% | 0.83 | 5/20 | yes |
| GRAVITA | Metals & Mining | 2011 | 50/100 | 1.18 | 0.89 | 33.9% | 57% | 1.10 | 57.1% | 65% | 0.00 | 100/200 | no |
| GVT&D | Capital Goods | 2009 | 50/100 | 0.51 | -0.16 | -7.5% | 38% | -0.26 | -21.0% | 82% | 0.25 | 5/13 | no |
| HAVELLS | Consumer Durables | 2005 | 100/200 | 1.13 | 0.44 | 8.5% | 35% | 0.52 | 12.3% | 42% | 0.58 | 20/50 | no |
| HBLENGINE | Capital Goods | 2007 | 13/34 | 0.37 | 0.56 | 16.8% | 51% | 0.84 | 38.7% | 65% | 0.25 | 50/200 | no |
| HCLTECH | Information Technology | 2005 | 50/200 | 0.75 | 0.38 | 6.5% | 37% | 0.81 | 21.5% | 35% | 0.00 | 5/13 | no |
| HDFCBANK | Financial Services | 2005 | 50/200 | 0.89 | 0.27 | 3.5% | 28% | 0.52 | 11.3% | 41% | 0.58 | 30/100 | no |
| HEGAM | Capital Goods | 2005 | 20/50 | 1.16 | 0.21 | 1.5% | 48% | -0.26 | -27.8% | 89% | 0.50 | 50/100 | yes |
| HEROMOTOCO | Automobile and Auto Components | 2005 | 100/200 | 0.36 | 0.04 | -0.5% | 30% | 0.07 | -3.1% | 50% | 0.58 | 13/34 | no |
| HFCL | Telecommunication | 2011 | 50/200 | 0.41 | 0.98 | 40.4% | 40% | 0.83 | 36.9% | 65% | 0.92 | 21/55 | yes |
| HINDALCO | Metals & Mining | 2008 | 30/100 | 0.61 | 1.00 | 28.6% | 35% | 0.65 | 20.6% | 61% | 0.67 | 9/21 | yes |
| HINDCOPPER | Metals & Mining | 2010 | 50/100 | -0.01 | 0.50 | 13.1% | 59% | 0.61 | 20.4% | 65% | 0.00 | 10/30 | no |
| HINDPETRO | Oil Gas & Consumable Fuels | 2005 | 8/21 | 0.67 | 0.30 | 4.4% | 32% | 0.14 | -2.0% | 50% | 1.00 | 8/21 | yes |
| HINDUNILVR | Fast Moving Consumer Goods | 2008 | 100/200 | 0.76 | 0.33 | 5.1% | 24% | 0.46 | 8.9% | 31% | 0.83 | 5/13 | no |
| HINDZINC | Metals & Mining | 2007 | 50/100 | 0.47 | 0.25 | 3.1% | 40% | 0.28 | 3.6% | 57% | 0.75 | 100/200 | no |
| HONAUT | Capital Goods | 2006 | 9/21 | 1.12 | 0.54 | 11.1% | 28% | 0.64 | 17.5% | 47% | 0.83 | 100/200 | no |
| HSCL | Chemicals | 2007 | 50/100 | 0.88 | 0.18 | 0.8% | 48% | 0.14 | -6.8% | 79% | 0.83 | 50/200 | yes |
| ICICIBANK | Financial Services | 2005 | 30/100 | 0.48 | 0.62 | 13.6% | 32% | 0.82 | 25.6% | 48% | 0.50 | 9/21 | no |
| IDBI | Financial Services | 2005 | 5/20 | 0.33 | -0.05 | -8.5% | 63% | 0.21 | -3.2% | 72% | 0.33 | 12/26 | no |
| IDEA | Telecommunication | 2007 | 13/34 | 0.08 | 0.44 | 10.4% | 43% | 0.14 | -23.8% | 87% | 1.00 | 13/34 | yes |
| IFCI | Financial Services | 2005 | 21/55 | 0.47 | 0.05 | -6.1% | 50% | 0.25 | -2.4% | 77% | 0.08 | 8/21 | no |
| IGL | Oil Gas & Consumable Fuels | 2005 | 30/100 | 0.78 | 0.20 | 1.8% | 47% | 0.49 | 11.5% | 44% | 0.58 | 50/200 | no |
| IIFL | Financial Services | 2006 | 8/21 | 0.89 | 0.82 | 30.1% | 43% | 0.31 | -1.3% | 88% | 0.67 | 50/200 | yes |
| INDHOTEL | Consumer Services | 2005 | 21/55 | 0.69 | 0.65 | 16.3% | 28% | 0.69 | 22.3% | 61% | 0.67 | 30/100 | no |
| INDIACEM | Construction Materials | 2005 | 12/26 | 0.34 | 0.09 | -3.3% | 54% | 0.66 | 22.9% | 41% | 0.58 | 50/200 | no |
| INDIANB | Financial Services | 2007 | 30/100 | 0.38 | 0.65 | 18.2% | 46% | 0.33 | 4.0% | 85% | 0.17 | 50/200 | yes |
| INDUSINDBK | Financial Services | 2005 | 21/55 | 0.79 | 0.38 | 7.1% | 37% | 0.15 | -6.8% | 83% | 1.00 | 21/55 | yes |
| INDUSTOWER | Telecommunication | 2013 | 50/200 | 0.64 | -0.58 | -14.9% | 51% | 0.07 | -7.7% | 62% | 0.00 | 9/21 | no |
| INFY | Information Technology | 2005 | 100/200 | 0.47 | 0.44 | 7.8% | 36% | 0.85 | 23.2% | 37% | 0.00 | 5/20 | no |
| INTELLECT | Information Technology | 2015 | 50/200 | 0.51 | 0.77 | 24.5% | 41% | 0.59 | 18.8% | 84% | 0.08 | 8/21 | yes |
| IOB | Financial Services | 2005 | 21/55 | 0.27 | 0.35 | 6.4% | 52% | 0.62 | 21.3% | 56% | 0.08 | 100/200 | no |
| IOC | Oil Gas & Consumable Fuels | 2005 | 50/100 | 0.32 | 0.01 | -1.7% | 28% | -0.00 | -4.5% | 58% | 0.33 | 10/30 | no |
| IPCALAB | Healthcare | 2005 | 13/34 | 0.87 | 0.53 | 11.4% | 32% | 0.75 | 20.7% | 36% | 0.50 | 100/200 | no |
| IRB | Construction | 2008 | 50/200 | 0.13 | 0.62 | 17.9% | 40% | 0.53 | 15.3% | 71% | 0.83 | 5/13 | yes |
| ITC | Fast Moving Consumer Goods | 2005 | 50/100 | 0.62 | 0.26 | 3.1% | 27% | 0.28 | 4.1% | 52% | 0.83 | 100/200 | no |
| ITI | Telecommunication | 2005 | 50/100 | 0.58 | -0.11 | -8.5% | 43% | 0.30 | 2.4% | 57% | 0.42 | 5/20 | no |
| J&KBANK | Financial Services | 2005 | 30/100 | 0.40 | 0.16 | -1.1% | 58% | 0.46 | 10.6% | 83% | 0.08 | 13/34 | no |
| JBMA | Automobile and Auto Components | 2006 | 21/55 | 0.97 | 1.22 | 52.6% | 49% | 0.93 | 43.9% | 67% | 0.83 | 5/13 | yes |
| JINDALSAW | Capital Goods | 2005 | 20/50 | 0.60 | -0.25 | -11.3% | 48% | 0.34 | 5.0% | 59% | 0.00 | 9/21 | no |
| JINDALSTEL | Metals & Mining | 2005 | 20/50 | 0.78 | 1.03 | 37.8% | 42% | 0.85 | 37.3% | 68% | 0.50 | 12/26 | yes |
| JKCEMENT | Construction Materials | 2006 | 8/21 | 1.12 | 1.10 | 30.2% | 39% | 1.19 | 42.8% | 47% | 0.67 | 5/13 | no |
| JKTYRE | Automobile and Auto Components | 2008 | 5/20 | 0.90 | 0.67 | 19.5% | 36% | 0.54 | 15.5% | 67% | 0.58 | 50/200 | yes |
| JMFINANCIL | Financial Services | 2007 | 100/200 | 0.53 | -0.38 | -13.1% | 58% | 0.06 | -6.8% | 53% | 0.17 | 5/20 | no |
| JPPOWER | Power | 2010 | 5/13 | -0.03 | 1.04 | 50.0% | 46% | 0.82 | 39.1% | 88% | 1.00 | 5/13 | yes |
| JSL | Metals & Mining | 2009 | 9/21 | 0.49 | 1.46 | 67.9% | 27% | 1.16 | 63.6% | 55% | 0.50 | 5/20 | yes |
| JSWENERGY | Power | 2010 | 13/34 | 0.18 | 1.06 | 36.4% | 38% | 1.02 | 43.4% | 51% | 0.58 | 8/21 | yes |
| JSWSTEEL | Metals & Mining | 2006 | 30/100 | 0.89 | 0.81 | 19.7% | 44% | 0.77 | 25.9% | 55% | 0.42 | 20/50 | yes |
| JUBLFOOD | Consumer Services | 2010 | 50/100 | 0.62 | 0.53 | 12.4% | 40% | 0.65 | 19.6% | 49% | 0.50 | 13/34 | no |
| JWL | Capital Goods | 2011 | 50/100 | 0.48 | 0.76 | 28.9% | 54% | 0.79 | 35.0% | 75% | 0.83 | 50/200 | no |
| KAJARIACER | Consumer Durables | 2005 | 100/200 | 0.94 | 0.22 | 2.5% | 44% | 0.80 | 23.9% | 53% | 0.00 | 13/34 | no |
| KARURVYSYA | Financial Services | 2005 | 5/13 | 0.75 | 0.79 | 23.4% | 40% | 0.36 | 6.3% | 79% | 0.92 | 5/20 | yes |
| KEC | Construction | 2006 | 9/21 | 1.09 | 0.39 | 7.0% | 36% | 0.52 | 13.4% | 55% | 0.83 | 5/20 | no |
| KEI | Capital Goods | 2007 | 21/55 | 0.75 | 0.87 | 29.5% | 34% | 1.02 | 42.7% | 63% | 1.00 | 21/55 | no |
| KIRLOSBROS | Capital Goods | 2011 | 30/100 | 0.38 | 0.30 | 4.4% | 62% | 0.63 | 21.2% | 60% | 0.17 | 5/13 | no |
| KIRLOSENG | Capital Goods | 2011 | 50/100 | 0.64 | 0.90 | 24.2% | 28% | 0.47 | 11.7% | 61% | 1.00 | 50/100 | yes |
| KOTAKBANK | Financial Services | 2005 | 100/200 | 0.99 | 0.07 | -1.4% | 36% | 0.46 | 9.9% | 36% | 0.75 | 20/50 | no |
| KPIL | Construction | 2005 | 21/55 | 1.25 | 0.54 | 11.5% | 37% | 0.42 | 9.1% | 68% | 0.92 | 50/100 | yes |
| KPRMILL | Textiles | 2008 | 21/55 | 1.17 | 1.29 | 45.3% | 31% | 1.12 | 47.2% | 54% | 0.83 | 10/30 | yes |
| KSB | Capital Goods | 2005 | 12/26 | 0.67 | 0.76 | 17.6% | 25% | 0.79 | 25.4% | 50% | 0.33 | 8/21 | no |
| LICHSGFIN | Financial Services | 2005 | 12/26 | 0.79 | -0.39 | -13.6% | 50% | 0.11 | -4.1% | 66% | 0.08 | 30/100 | no |
| LINDEINDIA | Chemicals | 2005 | 21/55 | 0.55 | 0.85 | 28.9% | 46% | 1.13 | 50.5% | 48% | 0.25 | 5/13 | no |
| LT | Construction | 2005 | 13/34 | 0.91 | 0.62 | 10.5% | 33% | 0.46 | 9.6% | 56% | 0.42 | 8/21 | yes |
| LTF | Financial Services | 2012 | 12/26 | 0.75 | 0.04 | -2.5% | 42% | -0.04 | -11.8% | 68% | 0.50 | 8/21 | no |
| LTFOODS | Fast Moving Consumer Goods | 2007 | 100/200 | 0.66 | 0.91 | 31.0% | 32% | 0.74 | 29.0% | 67% | 0.92 | 50/200 | yes |
| LUPIN | Healthcare | 2005 | 100/200 | 0.85 | 0.27 | 3.3% | 26% | 0.04 | -3.7% | 53% | 1.00 | 100/200 | yes |
| M&M | Automobile and Auto Components | 2005 | 13/34 | 0.64 | 0.52 | 10.3% | 37% | 0.49 | 11.7% | 67% | 0.25 | 100/200 | yes |
| M&MFIN | Financial Services | 2007 | 100/200 | 0.45 | 0.04 | -2.4% | 50% | 0.14 | -5.4% | 73% | 1.00 | 100/200 | no |
| MAHABANK | Financial Services | 2005 | 20/50 | 0.48 | 0.68 | 22.7% | 41% | 0.59 | 19.3% | 56% | 1.00 | 20/50 | yes |
| MANAPPURAM | Financial Services | 2011 | 50/200 | 0.62 | 0.09 | -3.3% | 59% | 0.35 | 5.6% | 63% | 0.17 | 10/30 | no |
| MARICO | Fast Moving Consumer Goods | 2006 | 100/200 | 0.76 | 0.36 | 5.4% | 22% | 0.43 | 8.0% | 39% | 0.92 | 21/55 | no |
| MARUTI | Automobile and Auto Components | 2005 | 21/55 | 1.02 | 0.07 | -0.7% | 31% | 0.26 | 3.0% | 48% | 0.50 | 10/30 | no |
| MCX | Financial Services | 2012 | 50/100 | 0.25 | 0.33 | 5.5% | 38% | 0.67 | 20.7% | 44% | 0.25 | 13/34 | no |
| MFSL | Financial Services | 2005 | 50/100 | 0.55 | 0.04 | -3.7% | 52% | 0.46 | 10.9% | 52% | 0.83 | 50/200 | no |
| MINDACORP | Automobile and Auto Components | 2014 | 21/55 | 0.35 | 0.17 | -0.3% | 53% | 0.41 | 8.5% | 65% | 0.58 | 100/200 | no |
| MMTC | Services | 2010 | 50/200 | 0.03 | 0.58 | 16.4% | 40% | 0.36 | 5.0% | 66% | 1.00 | 50/200 | yes |
| MOTHERSON | Automobile and Auto Components | 2005 | 50/100 | 0.83 | 0.27 | 3.8% | 38% | 0.09 | -7.4% | 69% | 0.50 | 30/100 | yes |
| MOTILALOFS | Financial Services | 2008 | 21/55 | 0.97 | -0.21 | -8.8% | 42% | 0.21 | 0.3% | 49% | 0.33 | 12/26 | no |
| MPHASIS | Information Technology | 2007 | 100/200 | 0.47 | 0.71 | 16.8% | 39% | 0.64 | 18.1% | 46% | 0.33 | 50/100 | yes |
| MRF | Automobile and Auto Components | 2005 | 5/20 | 1.07 | 0.30 | 4.2% | 33% | 0.38 | 7.1% | 34% | 0.83 | 20/50 | no |
| MRPL | Oil Gas & Consumable Fuels | 2005 | 5/20 | 0.46 | 0.22 | 1.4% | 48% | 0.09 | -7.4% | 71% | 0.08 | 5/13 | yes |
| MUTHOOTFIN | Financial Services | 2012 | 13/34 | 0.59 | 0.07 | -2.3% | 38% | 0.66 | 20.0% | 46% | 0.42 | 100/200 | no |
| NATCOPHARM | Healthcare | 2005 | 50/100 | 0.78 | 0.12 | 0.0% | 40% | -0.01 | -5.1% | 53% | 0.42 | 13/34 | yes |
| NATIONALUM | Metals & Mining | 2005 | 5/20 | 0.42 | 0.39 | 7.6% | 37% | 0.34 | 5.1% | 59% | 0.08 | 50/100 | yes |
| NAUKRI | Consumer Services | 2007 | 20/50 | 0.62 | 0.71 | 20.2% | 38% | 0.81 | 29.2% | 52% | 0.42 | 13/34 | no |
| NAVA | Power | 2007 | 30/100 | 0.52 | 0.75 | 26.2% | 38% | 0.60 | 19.8% | 72% | 0.58 | 100/200 | yes |
| NAVINFLUOR | Chemicals | 2008 | 50/100 | 1.01 | 1.04 | 38.6% | 30% | 1.30 | 56.1% | 30% | 0.75 | 100/200 | no |
| NBCC | Construction | 2013 | 100/200 | 1.03 | 0.29 | 4.4% | 40% | 0.05 | -9.8% | 77% | 1.00 | 100/200 | yes |
| NCC | Construction | 2012 | 5/13 | 0.56 | 0.43 | 9.3% | 50% | 0.24 | -1.4% | 86% | 0.58 | 30/100 | yes |
| NESTLEIND | Fast Moving Consumer Goods | 2010 | 50/200 | 0.70 | 0.62 | 12.3% | 23% | 0.71 | 15.3% | 23% | 1.00 | 50/200 | no |
| NEULANDLAB | Healthcare | 2009 | 9/21 | 0.54 | 0.56 | 16.9% | 59% | 0.85 | 38.6% | 67% | 0.08 | 50/100 | no |
| NHPC | Power | 2010 | 50/100 | 0.13 | 0.12 | -0.1% | 37% | 0.48 | 11.1% | 34% | 0.17 | 50/200 | no |
| NLCINDIA | Power | 2005 | 21/55 | 0.31 | -0.57 | -18.9% | 61% | 0.33 | 5.3% | 50% | 0.08 | 100/200 | no |
| NMDC | Metals & Mining | 2008 | 20/50 | 0.36 | 0.78 | 38.7% | 32% | 0.75 | 37.6% | 55% | 0.42 | 9/21 | yes |
| NTPC | Power | 2005 | 100/200 | 0.19 | 0.40 | 5.9% | 30% | 0.40 | 7.6% | 47% | 0.75 | 50/200 | no |
| OBEROIRLTY | Realty | 2011 | 50/200 | 0.31 | 0.43 | 9.5% | 35% | 0.60 | 18.4% | 53% | 0.67 | 50/100 | no |
| OFSS | Information Technology | 2009 | 8/21 | 0.54 | 0.19 | 1.7% | 29% | 0.02 | -4.2% | 59% | 0.33 | 30/100 | yes |
| OIL | Oil Gas & Consumable Fuels | 2010 | 21/55 | 0.12 | 0.17 | 0.9% | 47% | 0.31 | 4.4% | 62% | 0.08 | 10/30 | no |
| OLECTRA | Automobile and Auto Components | 2008 | 100/200 | 0.84 | 1.00 | 35.0% | 41% | 0.62 | 21.1% | 84% | 0.42 | 50/100 | yes |
| ONGC | Oil Gas & Consumable Fuels | 2005 | 20/50 | 0.24 | 0.53 | 10.7% | 29% | 0.19 | -0.7% | 66% | 0.75 | 13/34 | yes |
| PAGEIND | Textiles | 2008 | 50/100 | 1.21 | 0.64 | 13.8% | 31% | 0.56 | 14.3% | 37% | 1.00 | 50/100 | yes |
| PCBL | Chemicals | 2005 | 8/21 | 1.05 | 0.02 | -5.0% | 56% | 0.35 | 5.7% | 73% | 0.08 | 21/55 | no |
| PERSISTENT | Information Technology | 2011 | 30/100 | 0.75 | 1.48 | 49.1% | 29% | 1.42 | 58.5% | 37% | 0.33 | 12/26 | yes |
| PETRONET | Oil Gas & Consumable Fuels | 2005 | 30/100 | 0.82 | -0.58 | -11.4% | 47% | 0.10 | -1.2% | 39% | 0.25 | 5/13 | no |
| PFC | Financial Services | 2007 | 50/100 | 0.27 | 0.06 | -2.1% | 42% | 0.37 | 7.2% | 45% | 0.42 | 13/34 | no |
| PFOCUS | Media Entertainment & Publication | 2007 | 100/200 | 0.54 | 0.45 | 10.9% | 39% | 0.40 | 4.8% | 79% | 1.00 | 100/200 | yes |
| PGEL | Consumer Durables | 2012 | 12/26 | 0.70 | 1.25 | 61.2% | 54% | 1.25 | 84.2% | 79% | 0.25 | 100/200 | no |
| PHOENIXLTD | Realty | 2008 | 50/100 | 0.59 | 0.53 | 13.5% | 48% | 0.77 | 26.5% | 48% | 0.92 | 13/34 | no |
| PIDILITIND | Chemicals | 2005 | 50/200 | 1.13 | 0.69 | 15.7% | 28% | 0.91 | 23.3% | 28% | 0.92 | 100/200 | no |
| PIIND | Chemicals | 2012 | 50/100 | 0.92 | 0.89 | 25.4% | 37% | 1.19 | 41.7% | 37% | 0.67 | 50/200 | no |
| PNB | Financial Services | 2005 | 5/13 | 0.32 | 0.48 | 10.2% | 37% | 0.02 | -8.2% | 73% | 1.00 | 5/13 | yes |
| POLYMED | Healthcare | 2012 | 50/100 | 1.09 | 1.00 | 34.2% | 41% | 1.04 | 41.9% | 41% | 0.67 | 5/20 | no |
| POONAWALLA | Financial Services | 2005 | 50/200 | 0.52 | 1.22 | 51.2% | 39% | 0.74 | 29.8% | 90% | 0.08 | 12/26 | yes |
| POWERGRID | Power | 2008 | 50/100 | 0.36 | -0.00 | -2.5% | 31% | 0.47 | 9.6% | 33% | 0.75 | 100/200 | no |
| PRESTIGE | Realty | 2011 | 50/100 | 0.58 | 0.29 | 3.5% | 55% | 0.63 | 20.5% | 68% | 0.25 | 13/34 | no |
| PVRINOX | Media Entertainment & Publication | 2006 | 21/55 | 0.66 | -0.10 | -7.1% | 35% | 0.25 | 1.7% | 65% | 0.58 | 5/13 | no |
| RADICO | Fast Moving Consumer Goods | 2005 | 13/34 | 0.58 | 0.88 | 24.9% | 35% | 0.78 | 26.3% | 45% | 1.00 | 13/34 | yes |
| RAMCOCEM | Construction Materials | 2005 | 30/100 | 0.82 | 0.16 | 1.2% | 35% | 0.22 | 2.1% | 48% | 0.75 | 9/21 | no |
| RECLTD | Financial Services | 2009 | 13/34 | 0.68 | 0.19 | 1.5% | 36% | 0.35 | 6.3% | 52% | 0.83 | 21/55 | no |
| REDINGTON | Services | 2007 | 12/26 | 0.51 | 0.37 | 7.4% | 50% | 0.94 | 42.9% | 54% | 0.42 | 50/100 | no |
| RELIANCE | Oil Gas & Consumable Fuels | 2005 | 100/200 | 0.70 | 0.58 | 13.9% | 45% | 0.79 | 22.9% | 45% | 0.50 | 5/20 | no |
| RHIM | Capital Goods | 2012 | 50/100 | 1.04 | 0.64 | 18.5% | 58% | 0.94 | 38.1% | 57% | 0.08 | 21/55 | no |
| RKFORGE | Automobile and Auto Components | 2005 | 5/20 | 1.09 | 1.12 | 39.8% | 35% | 0.71 | 25.7% | 74% | 0.75 | 30/100 | yes |
| RPOWER | Power | 2008 | 5/13 | 0.24 | 0.69 | 23.4% | 60% | 0.14 | -16.4% | 97% | 0.33 | 10/30 | yes |
| SAIL | Metals & Mining | 2005 | 21/55 | 0.42 | 0.46 | 10.2% | 45% | 0.44 | 9.9% | 66% | 0.08 | 12/26 | yes |
| SAMMAANCAP | Financial Services | 2014 | 13/34 | 0.78 | 0.03 | -6.2% | 52% | -0.22 | -35.2% | 90% | 0.67 | 5/13 | no |
| SARDAEN | Metals & Mining | 2010 | 20/50 | 0.71 | 1.14 | 47.6% | 31% | 0.92 | 41.3% | 68% | 1.00 | 20/50 | yes |
| SBIN | Financial Services | 2005 | 5/13 | 0.46 | 1.04 | 25.7% | 21% | 0.68 | 20.0% | 59% | 0.58 | 9/21 | yes |
| SCHAEFFLER | Automobile and Auto Components | 2005 | 9/21 | 1.08 | 0.73 | 15.2% | 23% | 0.89 | 24.9% | 46% | 0.17 | 13/34 | no |
| SCHNEIDER | Capital Goods | 2013 | 100/200 | 0.56 | 0.50 | 12.5% | 37% | 0.49 | 12.9% | 46% | 1.00 | 100/200 | yes |
| SCI | Services | 2005 | 50/100 | 0.14 | 0.30 | 4.4% | 48% | 0.76 | 29.7% | 51% | 0.00 | 12/26 | no |
| SHREECEM | Construction Materials | 2005 | 9/21 | 1.53 | 0.22 | 2.4% | 33% | 0.40 | 7.8% | 43% | 0.83 | 5/20 | no |
| SHRIRAMFIN | Financial Services | 2005 | 9/21 | 0.97 | -0.15 | -9.7% | 59% | 0.31 | 2.5% | 66% | 0.42 | 10/30 | no |
| SIEMENS | Capital Goods | 2005 | 50/100 | 0.82 | 1.18 | 32.1% | 23% | 0.97 | 28.4% | 41% | 1.00 | 50/100 | yes |
| SJVN | Power | 2011 | 100/200 | 0.25 | 0.31 | 4.4% | 22% | 0.39 | 7.4% | 32% | 1.00 | 100/200 | no |
| SOBHA | Realty | 2007 | 5/13 | 0.42 | 0.26 | 3.1% | 42% | 0.36 | 5.9% | 78% | 0.17 | 10/30 | no |
| SOLARINDS | Chemicals | 2010 | 10/30 | 1.25 | 1.22 | 33.7% | 22% | 1.24 | 42.3% | 34% | 0.75 | 9/21 | no |
| SRF | Chemicals | 2005 | 5/13 | 0.99 | 0.84 | 20.0% | 35% | 1.40 | 55.5% | 37% | 0.00 | 50/200 | no |
| STLTECH | Telecommunication | 2008 | 100/200 | 0.90 | 0.12 | -0.2% | 45% | 0.01 | -12.5% | 79% | 0.58 | 30/100 | no |
| SUNDARMFIN | Financial Services | 2005 | 21/55 | 1.04 | 0.46 | 8.2% | 32% | 0.52 | 12.7% | 43% | 1.00 | 21/55 | no |
| SUNPHARMA | Healthcare | 2005 | 50/100 | 0.75 | 0.66 | 13.0% | 22% | 0.85 | 23.6% | 32% | 0.83 | 100/200 | no |
| SUNTV | Media Entertainment & Publication | 2007 | 9/21 | 0.24 | 0.16 | 0.9% | 23% | 0.04 | -5.7% | 58% | 0.83 | 12/26 | yes |
| SUPREMEIND | Capital Goods | 2005 | 5/20 | 1.07 | 0.47 | 8.9% | 36% | 0.74 | 20.3% | 43% | 0.50 | 8/21 | no |
| SUZLON | Capital Goods | 2006 | 13/34 | 0.40 | 0.09 | -6.3% | 71% | 0.61 | 20.8% | 78% | 0.00 | 9/21 | no |
| SWANCORP | Chemicals | 2013 | 21/55 | 0.61 | 0.01 | -6.1% | 52% | 0.81 | 32.1% | 45% | 0.08 | 100/200 | no |
| TARIL | Capital Goods | 2008 | 10/30 | 0.50 | 0.36 | 6.2% | 46% | 0.86 | 43.8% | 67% | 0.17 | 100/200 | no |
| TATACHEM | Chemicals | 2005 | 5/13 | 0.66 | 0.71 | 16.4% | 43% | 0.44 | 7.3% | 74% | 0.75 | 12/26 | yes |
| TATACOMM | Telecommunication | 2008 | 12/26 | 0.27 | 0.95 | 27.3% | 30% | 0.73 | 25.1% | 65% | 0.75 | 5/13 | yes |
| TATACONSUM | Fast Moving Consumer Goods | 2011 | 100/200 | 0.37 | 1.04 | 31.3% | 43% | 1.13 | 37.1% | 43% | 0.50 | 13/34 | no |
| TATAELXSI | Information Technology | 2005 | 30/100 | 0.77 | 1.32 | 53.2% | 52% | 1.29 | 58.3% | 50% | 0.17 | 8/21 | yes |
| TATAINVEST | Financial Services | 2006 | 5/13 | 0.53 | 0.79 | 18.8% | 23% | 0.83 | 24.2% | 40% | 0.83 | 8/21 | no |
| TATAPOWER | Power | 2005 | 50/200 | 0.31 | 1.18 | 39.1% | 33% | 0.81 | 28.5% | 65% | 0.75 | 8/21 | yes |
| TATASTEEL | Metals & Mining | 2006 | 21/55 | 0.57 | 0.86 | 22.8% | 49% | 0.67 | 21.3% | 54% | 0.42 | 10/30 | yes |
| TCS | Information Technology | 2005 | 100/200 | 0.93 | 0.27 | 3.5% | 33% | 0.65 | 14.6% | 28% | 0.08 | 30/100 | no |
| TDPOWERSYS | Capital Goods | 2012 | 21/55 | 0.21 | 0.37 | 7.3% | 62% | 1.01 | 47.0% | 57% | 0.17 | 8/21 | no |
| TECHM | Information Technology | 2007 | 30/100 | 0.83 | 0.83 | 17.3% | 28% | 0.44 | 9.2% | 47% | 0.50 | 8/21 | yes |
| THANGAMAYL | Consumer Durables | 2010 | 9/21 | 0.76 | 0.70 | 21.6% | 29% | 0.79 | 30.6% | 51% | 0.75 | 20/50 | no |
| THERMAX | Capital Goods | 2005 | 13/34 | 1.11 | 0.31 | 5.1% | 33% | 0.59 | 15.6% | 41% | 0.33 | 12/26 | no |
| TIMKEN | Capital Goods | 2007 | 50/200 | 0.55 | 1.17 | 46.7% | 37% | 1.23 | 52.8% | 37% | 1.00 | 50/200 | no |
| TITAGARH | Capital Goods | 2009 | 8/21 | 0.86 | 0.85 | 31.1% | 37% | 0.73 | 28.9% | 74% | 0.50 | 100/200 | yes |
| TITAN | Consumer Durables | 2005 | 50/100 | 0.86 | 0.53 | 10.8% | 49% | 0.97 | 29.6% | 42% | 0.00 | 12/26 | no |
| TMPV | Automobile and Auto Components | 2005 | 50/100 | 0.58 | 0.49 | 11.8% | 44% | 0.65 | 22.5% | 72% | 0.00 | 21/55 | no |
| TORNTPHARM | Healthcare | 2005 | 100/200 | 0.84 | 0.49 | 10.2% | 27% | 0.62 | 14.9% | 25% | 1.00 | 100/200 | no |
| TORNTPOWER | Power | 2007 | 30/100 | 0.54 | -0.21 | -8.9% | 48% | 0.66 | 17.2% | 30% | 0.08 | 100/200 | no |
| TRENT | Consumer Services | 2005 | 8/21 | 0.89 | 0.70 | 17.9% | 25% | 1.04 | 39.5% | 49% | 0.67 | 5/13 | no |
| TRIDENT | Textiles | 2012 | 50/200 | 0.87 | 0.90 | 33.3% | 50% | 1.05 | 51.6% | 60% | 0.42 | 30/100 | no |
| TRITURBINE | Capital Goods | 2012 | 100/200 | 0.47 | 0.87 | 27.6% | 32% | 0.68 | 24.3% | 60% | 0.83 | 50/100 | yes |
| TTML | Telecommunication | 2005 | 30/100 | 0.00 | 1.31 | 67.0% | 69% | 1.51 | 121.1% | 71% | 0.25 | 50/200 | no |
| TVSMOTOR | Automobile and Auto Components | 2005 | 10/30 | 1.02 | 0.63 | 13.2% | 30% | 0.63 | 17.6% | 56% | 0.17 | 100/200 | yes |
| UBL | Fast Moving Consumer Goods | 2009 | 100/200 | 0.68 | 0.31 | 4.5% | 30% | 0.33 | 5.5% | 46% | 1.00 | 100/200 | no |
| UCOBANK | Financial Services | 2005 | 12/26 | 0.38 | 0.45 | 10.5% | 52% | 0.43 | 10.1% | 62% | 0.67 | 100/200 | yes |
| ULTRACEMCO | Construction Materials | 2005 | 5/20 | 0.88 | 0.48 | 7.9% | 43% | 0.61 | 15.0% | 38% | 0.58 | 8/21 | no |
| UNIONBANK | Financial Services | 2005 | 21/55 | 0.18 | 0.12 | -1.4% | 60% | 0.19 | -2.0% | 77% | 0.08 | 100/200 | no |
| UNITDSPR | Fast Moving Consumer Goods | 2005 | 30/100 | 0.91 | -0.32 | -9.4% | 46% | 0.41 | 8.5% | 39% | 0.08 | 20/50 | no |
| UNOMINDA | Automobile and Auto Components | 2007 | 20/50 | 1.02 | 0.95 | 28.4% | 24% | 0.90 | 34.8% | 49% | 0.92 | 21/55 | yes |
| UPL | Chemicals | 2005 | 100/200 | 0.58 | 0.11 | -0.6% | 34% | 0.42 | 9.1% | 64% | 0.25 | 8/21 | no |
| USHAMART | Capital Goods | 2005 | 5/13 | 0.74 | 0.93 | 36.0% | 52% | 0.97 | 48.4% | 72% | 0.17 | 8/21 | no |
| VEDL | Metals & Mining | 2005 | 30/100 | 0.90 | 0.73 | 20.7% | 34% | 0.62 | 19.8% | 70% | 0.33 | 9/21 | yes |
| VOLTAS | Consumer Durables | 2005 | 20/50 | 1.20 | 0.21 | 2.1% | 34% | 0.45 | 9.6% | 42% | 0.33 | 30/100 | no |
| VTL | Textiles | 2007 | 50/200 | 0.96 | 0.51 | 11.3% | 45% | 0.44 | 10.2% | 55% | 0.50 | 21/55 | yes |
| WELCORP | Capital Goods | 2011 | 9/21 | 0.66 | 0.29 | 3.7% | 46% | 0.52 | 14.4% | 75% | 0.58 | 5/13 | no |
| WELSPUNLIV | Textiles | 2005 | 5/13 | 0.85 | 0.38 | 7.4% | 41% | 0.38 | 6.5% | 70% | 0.25 | 30/100 | no |
| WHIRLPOOL | Consumer Durables | 2010 | 30/100 | 0.84 | -0.05 | -3.9% | 43% | 0.20 | 1.2% | 47% | 0.25 | 100/200 | no |
| WIPRO | Information Technology | 2005 | 100/200 | 0.46 | 0.64 | 12.4% | 35% | 0.53 | 12.2% | 48% | 0.00 | 30/100 | yes |
| WOCKPHARMA | Healthcare | 2005 | 5/13 | 0.74 | 0.48 | 11.4% | 45% | -0.13 | -17.9% | 72% | 0.92 | 100/200 | yes |
| YESBANK | Financial Services | 2006 | 13/34 | 0.85 | -0.93 | -34.4% | 89% | -0.21 | -42.6% | 96% | 0.00 | 100/200 | no |
| ZEEL | Media Entertainment & Publication | 2007 | 100/200 | 0.57 | -0.67 | -16.1% | 51% | 0.03 | -16.1% | 76% | 0.25 | 5/20 | no |
| ZENSARTECH | Information Technology | 2005 | 5/20 | 0.76 | 0.90 | 28.0% | 45% | 0.17 | -2.3% | 75% | 0.67 | 30/100 | yes |
| ZFCVINDIA | Automobile and Auto Components | 2009 | 50/100 | 1.18 | -0.14 | -4.9% | 37% | 0.36 | 6.4% | 34% | 0.08 | 50/200 | no |
| ZYDUSLIFE | Healthcare | 2005 | 12/26 | 0.62 | 0.35 | 5.4% | 23% | 0.30 | 4.7% | 50% | 0.58 | 50/100 | yes |
| ZYDUSWELL | Fast Moving Consumer Goods | 2010 | 13/34 | 0.48 | 0.36 | 5.6% | 21% | 0.23 | 2.5% | 39% | 0.83 | 50/200 | yes |

## Not tested (198)

Most of these listed in 2016 or later, so they don't have 3 years of
training data before 2019. Those after 2022 have no data before the
holdout. Where a stock had some training days, the count is in brackets.

360ONE, AADHARHFC, AAVAS, ABCAPITAL (129 train days), ABDL, ABSLAMC, ACMESOLAR, ACUTAAS, ADANIENSOL (644 train days), ADANIGREEN, AEGISVOPAK, AETHER, AFCONS, AFFLE, AIIL, ALKEM (547 train days), AMBER (26 train days), ANANDRATHI, ANGELONE, ANTHEM, ANURAS, APTUS, ASTERDM (8 train days), ATGL, ATHERENERG, AUBANK (166 train days), AWL, AZAD, BAGMANE, BAJAJHFL, BANDHANBNK, BDL, BELRISE, BHARATCOAL, BHARTIHEXA, BIRET, BLS (430 train days), BSE (271 train days), CAMS, CANHLIFE, CARTRADE, CDSL (172 train days), CHOICEIN, CHOLAHLDNG (113 train days), CLEAN, CLEANMAX, CMPDI, COCHINSHIP (142 train days), COHANCE, CONCORDBIO, CPPLUS, CRAFTSMAN, CREDITACC, CROMPTON (453 train days), CUPID (367 train days), DALBHARAT, DATAPATTNS, DELHIVERY, DEVYANI, DIXON (118 train days), DMART (241 train days), DOMS, DUMMYHEG, EMBASSY, EMCURE, EMMVEE, ENDURANCE (346 train days), ENRIN, ETERNAL, FIRSTCRY, FIVESTAR, FLUOROCHEM, FORCEMOT, FRACTAL, GICRE (93 train days), GLAND, GROWW, GRSE, HAL, HDBFS, HDFCAMC, HDFCLIFE (76 train days), HEXT, HOMEFIRST, HONASA, HUDCO (201 train days), HYUNDAI, ICICIAMC, ICICIGI (111 train days), ICICILIFE (358 train days), IDFCFIRSTB (578 train days), IEX (95 train days), IGIL, IKS, INDIAMART, INDIGO (576 train days), INOXINDIA, INOXWIND (723 train days), IRCON, IRCTC, IREDA, IRFC, ITCHOTELS, JAINREC, JIOFIN, JSWCEMENT, JSWINFRA, JUBLINGREA, JYOTICNC, KALYANKJIL, KAYNES, KFINTECH, KIMS, KPITTECH, LALPATHLAB (547 train days), LAURUSLABS (304 train days), LEMONTREE, LENSKART, LGEINDIA, LICI, LLOYDSME, LODHA, LTM (405 train days), LTTS (362 train days), MANKIND, MAXHEALTH, MAZDOCK, MEDANTA, MEESHO, MGL (418 train days), MSUMI, MTARTECH, NAM-INDIA (85 train days), NETWEB, NH (538 train days), NIACL (80 train days), NSLNISP, NTPCGREEN, NUVAMA, NUVOCO, NYKAA, OLAELEC, ONESOURCE, PARADEEP, PATANJALI, PAYTM, PINELABS, PIRAMALFIN, PNBHOUSING (333 train days), POLICYBZR, POLYCAB, POWERINDIA, PPLPHARMA, PREMIERENE, PRIVISCL (704 train days), PTCIL, PWL, RAILTEL, RAINBOW, RBLBANK (377 train days), RITES, RRKABEL, RUBICON, RVNL, SAGILITY, SAILIFE, SANSERA, SBICARD, SBILIFE (108 train days), SHRIPISTON (432 train days), SHYAMMETL, SIGNATURE, SONACOMS, STARHEALTH, SUMICHEM, SWIGGY, SYNGENE (637 train days), SYRMA, TATACAP, TATATECH, TBOTEK, TECHNOE, TEGA, TEJASNET (175 train days), TENNIND, TIINDIA (87 train days), TMCV, URBANCO, UTIAMC, VAML, VBL (332 train days), VEDPOWER, VIJAYA, VISL, VMM, VOGL, WAAREEENER, ZENTEC (729 train days)
