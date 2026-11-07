package analytics

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"calm/internal/market"
	"calm/internal/strategy"
)

// TrialRecord is one entry in a data directory's trial ledger. Every research
// run is recorded, so a result can be judged against how many tries it took.
type TrialRecord struct {
	Time       time.Time       `json:"time"`
	Command    string          `json:"command"`
	Strategy   string          `json:"strategy"`
	Underlying string          `json:"underlying"`
	Params     strategy.Params `json:"params"`
	From       market.Date     `json:"from"`
	To         market.Date     `json:"to"`
	// Count is how many parameter sets this record stands for: one for a
	// backtest, every combination in every training window for a walk-forward.
	Count    int          `json:"count"`
	Unsealed bool         `json:"unsealed,omitempty"`
	Revision string       `json:"revision,omitempty"`
	Net      market.Paise `json:"net_paise"`
	Sharpe   float64      `json:"sharpe"`
	Trades   int          `json:"trades"`
}

// LedgerPath is where a data directory's trial ledger lives.
func LedgerPath(dataDir string) string { return filepath.Join(dataDir, "trials.jsonl") }

// AppendTrials adds records to the ledger and syncs it to disk.
func AppendTrials(path string, recs []TrialRecord) error {
	if len(recs) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	var buf []byte
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			f.Close()
			return err
		}
		buf = append(append(buf, line...), '\n')
	}
	if _, err := f.Write(buf); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// TrialCount totals the parameter sets tried for a strategy on an underlying,
// and how many of those runs looked at held-back data.
func TrialCount(path, strat, underlying string) (trials, unsealed int, err error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var r TrialRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return 0, 0, err
		}
		if r.Strategy == strat && r.Underlying == underlying {
			trials += r.Count
			if r.Unsealed {
				unsealed++
			}
		}
	}
	return trials, unsealed, sc.Err()
}

// Record builds a ledger entry for one run's outcome.
func Record(command string, s Spec, m Metrics, count int, unsealed bool, revision string, now time.Time) TrialRecord {
	return TrialRecord{Time: now, Command: command, Strategy: s.Strategy, Underlying: s.Underlying,
		Params: s.Params, From: s.From, To: s.To, Count: count, Unsealed: unsealed,
		Revision: revision, Net: m.Net, Sharpe: m.Sharpe, Trades: m.Trades}
}
