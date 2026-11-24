package manual

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"serene/internal/market"
)

// Since is the first day the page's running total counts.
var Since = market.Date{Year: 2026, Month: 11, Day: 25}

// DayNet is one broker's result from the desk's trades on one day.
type DayNet struct {
	PnL     float64 `json:"pnl"`
	Charges float64 `json:"charges"`
	Net     float64 `json:"net"`
	Source  string  `json:"source"` // "desk": recorded by the desk after the close
	Note    string  `json:"note,omitempty"`
}

// Ledger is the desk's record of each day: each broker's net from its
// manual trades, and whether a trade was taken (one a day). It is a JSON file
// so a hand-entered day can be added with an editor.
type Ledger struct {
	path string
	mu   sync.Mutex
	f    ledgerFile
}

type ledgerFile struct {
	Days   map[string]map[string]DayNet `json:"days"`   // date → broker → net
	Traded map[string]string            `json:"traded"` // date → when the day's trade was placed
}

// OpenLedger reads path, or starts empty if it doesn't exist. An empty path
// keeps the ledger in memory only.
func OpenLedger(path string) (*Ledger, error) {
	l := &Ledger{path: path, f: ledgerFile{Days: map[string]map[string]DayNet{}, Traded: map[string]string{}}}
	if path == "" {
		return l, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &l.f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if l.f.Days == nil {
		l.f.Days = map[string]map[string]DayNet{}
	}
	if l.f.Traded == nil {
		l.f.Traded = map[string]string{}
	}
	return l, nil
}

func (l *Ledger) save() error {
	if l.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(l.f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// Put records broker's net on day, replacing what was there.
func (l *Ledger) Put(day market.Date, broker string, n DayNet) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := day.String()
	if l.f.Days[k] == nil {
		l.f.Days[k] = map[string]DayNet{}
	}
	l.f.Days[k][broker] = n
	return l.save()
}

// Get is broker's net on day, if recorded.
func (l *Ledger) Get(day market.Date, broker string) (DayNet, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.f.Days[day.String()][broker]
	return n, ok
}

// MarkTraded records that day's one trade has been taken.
func (l *Ledger) MarkTraded(day market.Date, at string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.f.Traded[day.String()]; ok {
		return nil
	}
	l.f.Traded[day.String()] = at
	return l.save()
}

// TradedOn says whether day's trade has been taken.
func (l *Ledger) TradedOn(day market.Date) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.f.Traded[day.String()]
	return ok
}

// Tally is the running total the page's card shows.
type Tally struct {
	Since   string             `json:"since"`
	Brokers map[string]float64 `json:"brokers"` // net by broker, recorded days plus today's live figure
	Net     float64            `json:"net"`
	Days    int                `json:"days"` // trading days from Since to today
}

// Tally sums the recorded days from Since up to, not including, today, and
// adds today's live nets (by broker) when today is a trading day.
func (l *Ledger) Tally(today market.Date, live map[string]float64) Tally {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := Tally{Since: Since.String(), Brokers: map[string]float64{}}
	var days []string
	for d := Since; !today.Before(d); d = d.AddDays(1) {
		if market.NSETradingDay(d) {
			t.Days++
			if d.Before(today) {
				days = append(days, d.String())
			}
		}
	}
	for _, d := range days {
		for b, n := range l.f.Days[d] {
			t.Brokers[b] += n.Net
		}
	}
	for b, v := range live {
		if _, done := l.f.Days[today.String()][b]; done {
			continue // today already closed and recorded below
		}
		t.Brokers[b] += v
	}
	for b, n := range l.f.Days[today.String()] {
		t.Brokers[b] += n.Net
	}
	for b := range t.Brokers {
		t.Brokers[b] = round2(t.Brokers[b])
		t.Net += t.Brokers[b]
	}
	t.Net = round2(t.Net)
	return t
}
