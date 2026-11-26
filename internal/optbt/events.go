package optbt

import (
	"encoding/csv"
	"fmt"
	"os"
	"strings"

	"serene/internal/market"
)

// Event kinds, as bits of Day.Events.
const (
	evBudget uint8 = 1 << iota
	evRBI
	evElection
	evFOMC
)

var eventKinds = map[string]uint8{"budget": evBudget, "rbi": evRBI, "election": evElection, "fomc": evFOMC}

// skipSets are the skip_events values: a cell does not trade a day when the
// entry day, or for STBT the exit day, has one of these kinds.
var skipSets = map[string]uint8{
	"none":  0,
	"india": evBudget | evRBI | evElection,
	"all":   evBudget | evRBI | evElection | evFOMC,
}

// loadEvents reads date,kind[,note] rows (# comments allowed). A date that is
// not a stored trading day moves to the next one that is, within 5 days.
func loadEvents(path string, all []market.Date) (map[market.Date]uint8, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
			lines = append(lines, l)
		}
	}
	r := csv.NewReader(strings.NewReader(strings.Join(lines, "\n")))
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := map[market.Date]uint8{}
	for i, rec := range recs {
		if i == 0 && rec[0] == "date" {
			continue
		}
		if len(rec) < 2 {
			return nil, fmt.Errorf("%s: row %d: want date,kind", path, i+1)
		}
		d, err := market.ParseDate(strings.TrimSpace(rec[0]))
		if err != nil {
			return nil, fmt.Errorf("%s: row %d: %w", path, i+1, err)
		}
		k, ok := eventKinds[strings.TrimSpace(rec[1])]
		if !ok {
			return nil, fmt.Errorf("%s: row %d: unknown kind %q", path, i+1, rec[1])
		}
		for _, t := range all {
			if !t.Before(d) {
				if t.Before(d.AddDays(6)) {
					out[t] |= k
				}
				break
			}
		}
	}
	return out, nil
}
