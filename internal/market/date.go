package market

import (
	"fmt"
	"time"
)

// IST is India Standard Time. It is a fixed zone so behaviour does not depend
// on the host's timezone database.
var IST = time.FixedZone("IST", 5*3600+30*60)

// Date is a calendar date in IST. It is comparable, so it works as a map key.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// DateOf returns the IST date of an instant.
func DateOf(t time.Time) Date {
	y, m, d := t.In(IST).Date()
	return Date{y, m, d}
}

// ParseDate parses YYYY-MM-DD.
func ParseDate(s string) (Date, error) {
	t, err := time.ParseInLocation("2006-01-02", s, IST)
	if err != nil {
		return Date{}, fmt.Errorf("parse date %q: %w", s, err)
	}
	return DateOf(t), nil
}

func (d Date) String() string { return fmt.Sprintf("%04d-%02d-%02d", d.Year, int(d.Month), d.Day) }

// IsZero reports whether the date is unset.
func (d Date) IsZero() bool { return d == Date{} }

// At returns the instant at hour:minute IST on this date.
func (d Date) At(hour, minute int) time.Time {
	return time.Date(d.Year, d.Month, d.Day, hour, minute, 0, 0, IST)
}

// Before reports whether d is earlier than o.
func (d Date) Before(o Date) bool { return d.At(0, 0).Before(o.At(0, 0)) }

// AddDays returns the date n days later.
func (d Date) AddDays(n int) Date { return DateOf(d.At(12, 0).AddDate(0, 0, n)) }

// Weekday returns the day of the week.
func (d Date) Weekday() time.Weekday { return d.At(12, 0).Weekday() }
