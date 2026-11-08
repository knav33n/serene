package market

import "time"

// NSEHolidaysThrough is the last date the NSE holiday list covers. Add each
// year's list when NSE publishes it, usually in December.
var NSEHolidaysThrough = Date{2026, time.December, 31}

// nseHolidays are the weekdays NSE equity derivatives did not trade. Up to
// 2026-11-06 they are the weekdays missing from NSE's index history in
// data/market, which agree with NSE's published 2026 list; the rest of 2026
// comes from that list (nseindia.com/api/holiday-master?type=trading, FO).
var nseHolidays = map[Date]bool{
	{2024, time.January, 22}:   true,
	{2024, time.January, 26}:   true,
	{2024, time.March, 8}:      true,
	{2024, time.March, 25}:     true,
	{2024, time.March, 29}:     true,
	{2024, time.April, 11}:     true,
	{2024, time.April, 17}:     true,
	{2024, time.May, 1}:        true,
	{2024, time.May, 20}:       true,
	{2024, time.June, 17}:      true,
	{2024, time.July, 17}:      true,
	{2024, time.August, 15}:    true,
	{2024, time.October, 2}:    true,
	{2024, time.November, 15}:  true,
	{2024, time.November, 20}:  true,
	{2024, time.December, 25}:  true,
	{2025, time.February, 26}:  true,
	{2025, time.March, 14}:     true,
	{2025, time.March, 31}:     true,
	{2025, time.April, 10}:     true,
	{2025, time.April, 14}:     true,
	{2025, time.April, 18}:     true,
	{2025, time.May, 1}:        true,
	{2025, time.August, 15}:    true,
	{2025, time.August, 27}:    true,
	{2025, time.October, 2}:    true,
	{2025, time.October, 22}:   true,
	{2025, time.November, 5}:   true,
	{2025, time.December, 25}:  true,
	{2026, time.January, 15}:   true,
	{2026, time.January, 26}:   true,
	{2026, time.March, 3}:      true,
	{2026, time.March, 26}:     true,
	{2026, time.March, 31}:     true,
	{2026, time.April, 3}:      true,
	{2026, time.April, 14}:     true,
	{2026, time.May, 1}:        true,
	{2026, time.May, 28}:       true,
	{2026, time.June, 26}:      true,
	{2026, time.September, 14}: true,
	{2026, time.October, 2}:    true,
	{2026, time.October, 20}:   true,
	{2026, time.November, 10}:  true,
	{2026, time.November, 24}:  true,
	{2026, time.December, 25}:  true,
}

// NSETradingDay reports whether d is a weekday NSE is open. Weekend special
// sessions, such as a Sunday budget or Muhurat trading, are not counted.
func NSETradingDay(d Date) bool {
	w := d.Weekday()
	return w != time.Saturday && w != time.Sunday && !nseHolidays[d]
}
