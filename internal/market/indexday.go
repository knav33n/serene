package market

// IndexDay is one index's end-of-day record. A float field is NaN and a price
// or volume is 0 when the source has no value: India VIX has no volume, and
// bond indices have no P/E.
type IndexDay struct {
	Date       Date
	Name       string // canonical name, stable across renames
	Source     string // name as published that day
	Open       Paise
	High       Paise
	Low        Paise
	Close      Paise
	PrevClose  Paise // close minus the published points change
	Volume     int64
	TurnoverCr float64 // ₹ crore
	PE         float64
	PB         float64
	Yield      float64
}
