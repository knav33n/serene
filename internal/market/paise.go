// Package market holds the domain types shared by every mode.
package market

import (
	"fmt"
	"math"
)

// Paise is an amount of money or a price in paise. Integer arithmetic keeps
// P&L exact; floats are reserved for indicators and Greeks.
type Paise int64

// Rupees converts a rupee amount, rounding to the nearest paisa.
func Rupees(r float64) Paise { return Paise(math.Round(r * 100)) }

// Rupees returns the amount in rupees.
func (p Paise) Rupees() float64 { return float64(p) / 100 }

// String formats as rupees, dropping a zero fraction ("24000", "24000.50").
func (p Paise) String() string {
	sign := ""
	v := int64(p)
	if v < 0 {
		sign, v = "-", -v
	}
	if v%100 == 0 {
		return fmt.Sprintf("%s%d", sign, v/100)
	}
	return fmt.Sprintf("%s%d.%02d", sign, v/100, v%100)
}
