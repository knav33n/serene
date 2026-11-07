package analytics

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"calm/internal/market"
)

// Holdouts maps a data directory to the first date held back from research.
type Holdouts map[string]market.Date

// LoadHoldouts reads the holdout file. A missing file is an error, because
// running without it would quietly unseal every data directory.
func LoadHoldouts(path string) (Holdouts, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("holdout file: %w", err)
	}
	var raw map[string]string
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	h := Holdouts{}
	for dir, s := range raw {
		d, err := market.ParseDate(s)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", path, dir, err)
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, err
		}
		h[abs] = d
	}
	return h, nil
}

// Check reports whether [from, to] on dataDir reaches held-back data. It returns
// an error unless unseal is set, and says whether the range was sealed.
func (h Holdouts) Check(dataDir string, to market.Date, unseal bool) (sealed bool, err error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return false, err
	}
	start, ok := h[abs]
	if !ok || to.Before(start) {
		return false, nil
	}
	if !unseal {
		return true, fmt.Errorf("%s is held back from %s and this run ends %s: end before it, "+
			"or pass --unseal for a final check of a frozen design (it is recorded)", dataDir, start, to)
	}
	return true, nil
}
