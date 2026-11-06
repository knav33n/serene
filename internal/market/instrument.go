package market

import (
	"fmt"
	"strconv"
	"strings"
)

// Exchange identifies a trading venue segment.
type Exchange string

const (
	NSE Exchange = "NSE" // NSE cash and indices
	BSE Exchange = "BSE" // BSE cash and indices
	NFO Exchange = "NFO" // NSE derivatives
	BFO Exchange = "BFO" // BSE derivatives
	MCX Exchange = "MCX" // commodity derivatives
)

// Kind is the instrument class.
type Kind string

const (
	KindIndex  Kind = "index"
	KindEquity Kind = "equity"
	KindFuture Kind = "future"
	KindOption Kind = "option"
)

// Right is an option's type.
type Right string

const (
	Call Right = "CE"
	Put  Right = "PE"
)

// Instrument identifies something tradable or observable. Expiry and LotSize
// come from stored contract data: both change over time, so neither may be
// derived from the calendar or hardcoded.
type Instrument struct {
	Exchange   Exchange
	Kind       Kind
	Underlying string
	Expiry     Date  // options and futures
	Strike     Paise // options
	Right      Right // options
	LotSize    int64 // quantity per lot; 1 for indices
}

// Index returns the instrument for an index such as NIFTY.
func Index(exchange Exchange, name string) Instrument {
	return Instrument{Exchange: exchange, Kind: KindIndex, Underlying: name, LotSize: 1}
}

// Option returns an option contract.
func Option(exchange Exchange, underlying string, expiry Date, strike Paise, right Right, lotSize int64) Instrument {
	return Instrument{Exchange: exchange, Kind: KindOption, Underlying: underlying,
		Expiry: expiry, Strike: strike, Right: right, LotSize: lotSize}
}

// Key is the stable identity of an instrument. Broker tokens are never used
// as identity because exchanges reuse them after expiry. LotSize is not part
// of the key.
func (i Instrument) Key() string {
	switch i.Kind {
	case KindOption:
		return fmt.Sprintf("%s:%s:%s:%s:%s", i.Exchange, i.Underlying, i.Expiry, i.Strike, i.Right)
	case KindFuture:
		return fmt.Sprintf("%s:%s:%s:FUT", i.Exchange, i.Underlying, i.Expiry)
	default:
		return fmt.Sprintf("%s:%s", i.Exchange, i.Underlying)
	}
}

// ParseKey reverses Key. LotSize is unknown from a key and is left at zero,
// except for indices, where it is 1.
func ParseKey(key string) (Instrument, error) {
	p := strings.Split(key, ":")
	switch {
	case len(p) == 2:
		return Index(Exchange(p[0]), p[1]), nil
	case len(p) == 4 && p[3] == "FUT":
		exp, err := ParseDate(p[2])
		if err != nil {
			return Instrument{}, err
		}
		return Instrument{Exchange: Exchange(p[0]), Kind: KindFuture, Underlying: p[1], Expiry: exp}, nil
	case len(p) == 5 && (Right(p[4]) == Call || Right(p[4]) == Put):
		exp, err := ParseDate(p[2])
		if err != nil {
			return Instrument{}, err
		}
		strike, err := strconv.ParseFloat(p[3], 64)
		if err != nil {
			return Instrument{}, fmt.Errorf("parse strike in %q: %w", key, err)
		}
		return Option(Exchange(p[0]), p[1], exp, Rupees(strike), Right(p[4]), 0), nil
	}
	return Instrument{}, fmt.Errorf("invalid instrument key %q", key)
}

// IndexKey returns the key of the index a derivative settles against.
func (i Instrument) IndexKey() string {
	switch i.Exchange {
	case BFO:
		return Index(BSE, i.Underlying).Key()
	default:
		return Index(NSE, i.Underlying).Key()
	}
}

// Intrinsic returns an option's intrinsic value at the given index level.
func (i Instrument) Intrinsic(index Paise) Paise {
	v := index - i.Strike
	if i.Right == Put {
		v = -v
	}
	return max(v, 0)
}
