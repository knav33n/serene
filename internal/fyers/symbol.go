package fyers

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"serene/internal/market"
)

// Underlying is something Fyers lists derivatives on.
type Underlying struct {
	Name   string          // serene's name, e.g. NIFTY; also the ticker prefix
	Symbol string          // Fyers spot symbol, e.g. NSE:NIFTY50-INDEX
	Spot   market.Exchange // NSE or BSE: where the spot trades
}

// Indices maps serene's index names to their Fyers spot symbols.
var Indices = map[string]Underlying{
	"NIFTY":     {Name: "NIFTY", Symbol: "NSE:NIFTY50-INDEX", Spot: market.NSE},
	"BANKNIFTY": {Name: "BANKNIFTY", Symbol: "NSE:NIFTYBANK-INDEX", Spot: market.NSE},
	"SENSEX":    {Name: "SENSEX", Symbol: "BSE:SENSEX-INDEX", Spot: market.BSE},
}

// Lookup returns an index from Indices, or treats any other name as an NSE
// stock such as SBIN (NSE:SBIN-EQ).
func Lookup(name string) Underlying {
	if u, ok := Indices[name]; ok {
		return u
	}
	return Underlying{Name: name, Symbol: "NSE:" + name + "-EQ", Spot: market.NSE}
}

// Derivatives is the exchange segment its contracts trade on.
func (u Underlying) Derivatives() market.Exchange {
	if u.Spot == market.BSE {
		return market.BFO
	}
	return market.NFO
}

// SpotKey is the store key of the spot series, which the engine reads as the
// underlying's price.
func (u Underlying) SpotKey() string { return market.Index(u.Spot, u.Name).Key() }

// Earliest is the first date Fyers holds expired contracts for, by exchange.
func (u Underlying) Earliest() market.Date {
	if u.Spot == market.BSE {
		return market.Date{Year: 2023, Month: time.August, Day: 7}
	}
	return market.Date{Year: 2018, Month: time.October, Day: 10}
}

// weeklyMonth is the one-character month in weekly option tickers.
var weeklyMonth = map[byte]time.Month{'1': 1, '2': 2, '3': 3, '4': 4, '5': 5, '6': 6, '7': 7, '8': 8, '9': 9,
	'O': 10, 'N': 11, 'D': 12}

// ParseContract turns a Fyers derivative ticker into an instrument, checking it
// against the expiry it was listed under. Monthly tickers carry only the year
// and month, so the expiry supplies the day. Lot size is left at zero.
//
//	NSE:NIFTY25MAR23000CE    monthly option
//	NSE:NIFTY2510923000CE    weekly option, 2025-01-09 (month 1-9, O, N, D)
//	NSE:NIFTY25MARFUT        future
func ParseContract(ticker string, u Underlying, expiry market.Date) (market.Instrument, error) {
	exch, rest, ok := strings.Cut(ticker, ":")
	if !ok || market.Exchange(exch) != u.Spot || !strings.HasPrefix(rest, u.Name) {
		return market.Instrument{}, fmt.Errorf("%q is not a %s contract", ticker, u.Name)
	}
	rest = rest[len(u.Name):]
	if len(rest) < 8 {
		return market.Instrument{}, fmt.Errorf("%q: too short", ticker)
	}
	yy, err := strconv.Atoi(rest[:2])
	if err != nil {
		return market.Instrument{}, fmt.Errorf("%q: bad year", ticker)
	}
	year := 2000 + yy
	code, tail := rest[2:5], rest[5:]
	var month time.Month
	day := 0
	if m, err := time.Parse("Jan", code[:1]+strings.ToLower(code[1:])); err == nil && isLetters(code) {
		month = m.Month()
	} else {
		wm, ok := weeklyMonth[code[0]]
		d, err := strconv.Atoi(code[1:])
		if !ok || err != nil {
			return market.Instrument{}, fmt.Errorf("%q: unreadable expiry code %q", ticker, code)
		}
		month, day = wm, d
	}
	if expiry.Year != year || expiry.Month != month || (day != 0 && expiry.Day != day) {
		return market.Instrument{}, fmt.Errorf("%q does not expire on %s", ticker, expiry)
	}
	if tail == "FUT" {
		return market.Instrument{Exchange: u.Derivatives(), Kind: market.KindFuture, Underlying: u.Name, Expiry: expiry}, nil
	}
	right := market.Right(tail[max(len(tail)-2, 0):])
	if right != market.Call && right != market.Put {
		return market.Instrument{}, fmt.Errorf("%q: unknown option type", ticker)
	}
	strike, err := strconv.ParseFloat(tail[:len(tail)-2], 64)
	if err != nil || strike <= 0 {
		return market.Instrument{}, fmt.Errorf("%q: bad strike", ticker)
	}
	return market.Option(u.Derivatives(), u.Name, expiry, market.Rupees(strike), right, 0), nil
}

func isLetters(s string) bool {
	for i := range len(s) {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}
