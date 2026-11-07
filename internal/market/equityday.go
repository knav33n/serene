package market

// EquityDay is one security's end-of-day record from NSE's cash market
// bhavcopy. Prices are as traded that day, not adjusted for splits, bonuses or
// dividends. A security that didn't trade has no row.
type EquityDay struct {
	Date      Date
	Symbol    string
	Series    string // EQ, BE, BZ, SM, ST and so on; bonds and other series share the file
	ISIN      string // empty in files before 2011
	Open      Paise
	High      Paise
	Low       Paise
	Close     Paise // official closing price
	Last      Paise // last traded price
	PrevClose Paise // 0 where NSE published none, as in the first file
	Volume    int64 // shares traded
	Turnover  float64
	Trades    int64 // 0 in files before 2011, which don't carry it
}
