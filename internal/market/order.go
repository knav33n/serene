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

// Leg is one instrument within an order.
type Leg struct {
	Instrument Instrument
	Side       Side
	Lots       int64
}

// Qty is the leg's quantity in units.
func (l Leg) Qty() int64 { return l.Lots * l.Instrument.LotSize }

// Order is a group of legs that are placed together. Tag names the position
// the order belongs to, so legs can be analysed as one structure.
type Order struct {
	ID     int64
	Tag    string
	Legs   []Leg
	Placed time.Time
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
