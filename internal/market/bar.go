package market

import (
	"fmt"
	"time"
)

// Timeframe is a bar length.
type Timeframe int

const (
	Minute1 Timeframe = 1
	Minute5 Timeframe = 5
	Hour1   Timeframe = 60
	Daily   Timeframe = 1440 // one whole session
)

func (tf Timeframe) String() string {
	if tf == Daily {
		return "1d"
	}
	return fmt.Sprintf("%dm", int(tf))
}

// Bar is an OHLC candle. Time is the bar's start; Close is when it finished,
// which is when it may first be shown to a strategy.
type Bar struct {
	Key       string
	Timeframe Timeframe
	Time      time.Time
	End       time.Time
	Open      Paise
	High      Paise
	Low       Paise
	Close     Paise
	Volume    int64
	OI        int64
}

// Session is an exchange's continuous trading window in IST.
type Session struct {
	OpenHour, OpenMinute   int
	CloseHour, CloseMinute int
}

// NSESession is the NSE and BSE equity derivatives session.
var NSESession = Session{9, 15, 15, 30}

// Open returns the session start on d.
func (s Session) Open(d Date) time.Time { return d.At(s.OpenHour, s.OpenMinute) }

// Close returns the session end on d.
func (s Session) Close(d Date) time.Time { return d.At(s.CloseHour, s.CloseMinute) }

// Contains reports whether t falls inside the session.
func (s Session) Contains(t time.Time) bool {
	d := DateOf(t)
	return !t.Before(s.Open(d)) && t.Before(s.Close(d))
}

// Bucket returns the start and end of the tf bar holding t. Buckets align to
// the session open, and the last one is cut short at the session close.
func (s Session) Bucket(t time.Time, tf Timeframe) (start, end time.Time) {
	d := DateOf(t)
	open, closeAt := s.Open(d), s.Close(d)
	if tf == Daily {
		return open, closeAt
	}
	size := time.Duration(tf) * time.Minute
	start = open.Add(t.Sub(open).Truncate(size))
	end = start.Add(size)
	if end.After(closeAt) {
		end = closeAt
	}
	return start, end
}
