package market

import "time"

// Side is a trade direction. Its value is the sign of the quantity.
type Side int64

const (
	Buy  Side = 1
	Sell Side = -1
)

func (s Side) String() string {
	if s == Buy {
		return "BUY"
	}
	return "SELL"
}

// Leg is one instrument within an order. Each leg becomes its own order at a
// broker, so legs of one order can fill at different times or not at all.
type Leg struct {
	Instrument Instrument
	Side       Side
	Lots       int64
	Limit      Paise // the worst acceptable price for a limit order; zero for market
}

// Qty is the leg's quantity in units.
func (l Leg) Qty() int64 { return l.Lots * l.Instrument.LotSize }

// OrderKind is how an order is priced.
type OrderKind string

const (
	MarketOrder OrderKind = "market"
	LimitOrder  OrderKind = "limit"
)

// Product is how long a position may be held. The names are calm's own; each
// broker adapter maps them to its codes (Kite NRML and MIS, for example).
type Product string

const (
	Carry    Product = "carry"    // may be held past the day, to expiry
	Intraday Product = "intraday" // squared off by the broker before the close
)

// Order is a group of legs that are placed together. Tag names the position
// the order belongs to, so legs can be analysed as one structure.
type Order struct {
	ID int64
	// ClientID is calm's own identifier, sent to the broker as the order tag so
	// an order can be found again after a timeout or a restart.
	ClientID string
	Tag      string
	Kind     OrderKind
	Product  Product
	Legs     []Leg
	Placed   time.Time
}

// FillKind says how a fill came about.
type FillKind string

const (
	Traded  FillKind = "traded"
	Settled FillKind = "settled" // cash settlement at expiry
)

// Fill is an executed quantity of one instrument.
type Fill struct {
	OrderID    int64
	Tag        string
	Instrument Instrument
	Side       Side
	Qty        int64
	Price      Paise
	Time       time.Time
	Kind       FillKind
	Charges    Paise
}
