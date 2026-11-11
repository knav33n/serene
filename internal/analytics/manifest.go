package analytics

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"serene/internal/market"
	"serene/internal/strategy"
)

// Manifest records what produced a report, so a result can be traced to the
// code, settings and data behind it.
type Manifest struct {
	Created      time.Time       `json:"created"`
	Revision     string          `json:"revision"`
	GoVersion    string          `json:"go_version"`
	Config       string          `json:"config"`
	ConfigSHA256 string          `json:"config_sha256"`
	CostsSHA256  string          `json:"costs_sha256"`
	DataDir      string          `json:"data_dir"`
	Strategy     string          `json:"strategy"`
	Underlying   string          `json:"underlying"`
	Params       strategy.Params `json:"params"`
	From         market.Date     `json:"from"`
	To           market.Date     `json:"to"`
	Days         int             `json:"days"`
	Capital      market.Paise    `json:"capital_paise"`
	Trials       int             `json:"trials"`
	Unsealed     bool            `json:"unsealed"`
	Net          market.Paise    `json:"net_paise"`
	Sharpe       float64         `json:"sharpe"`
	Trades       int             `json:"trades"`
	Rejected     int             `json:"rejected"`
}

// NewManifest describes a finished backtest.
func NewManifest(o Outcome, prov Provenance, config, configSHA, costsSHA, dataDir string, now time.Time) Manifest {
	return Manifest{Created: now, Revision: Revision(), GoVersion: runtime.Version(),
		Config: config, ConfigSHA256: configSHA, CostsSHA256: costsSHA, DataDir: dataDir,
		Strategy: o.Spec.Strategy, Underlying: o.Spec.Underlying, Params: o.Spec.Params,
		From: o.Spec.From, To: o.Spec.To, Days: o.Metrics.Days, Capital: o.Spec.Capital,
		Trials: prov.Trials, Unsealed: prov.Unsealed, Net: o.Metrics.Net,
		Sharpe: o.Metrics.Sharpe, Trades: o.Metrics.Trades, Rejected: o.Metrics.Rejected}
}

// WriteManifest stores a manifest as indented JSON.
func WriteManifest(path string, m Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// FileSHA256 returns a file's hash in hex.
func FileSHA256(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
