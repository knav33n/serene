package main

import (
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
)

// appendCSVRow appends row to the CSV at path, writing header first when the
// file is new.
func appendCSVRow(path string, header, row []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	isNew := !fileExists(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	if isNew {
		_ = w.Write(header)
	}
	_ = w.Write(row)
	w.Flush()
	return errors.Join(w.Error(), f.Close())
}
