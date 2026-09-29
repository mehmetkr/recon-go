// Package store remembers runs and saves files safely.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// AlgoVersion changes whenever matching changes, so old results are never reused.
const AlgoVersion = 1

// Config is every setting that can change a result.
type Config struct {
	AlgoVersion   int      `json:"algo_version"`
	TZ            string   `json:"tz"`
	BankLayouts   []string `json:"bank_layouts"`
	LedgerLayouts []string `json:"ledger_layouts"`
	DateTolerance int      `json:"date_tolerance"`
	FuzzyWindow   int      `json:"fuzzy_window"`
	MinRefLen     int      `json:"min_ref_len"`
	ThresholdNum  int      `json:"threshold_num"`
	ThresholdDen  int      `json:"threshold_den"`
}

// ConfigHash fingerprints the settings.
func ConfigHash(c Config) string {
	b, err := json.Marshal(c)
	if err != nil {
		panic(err) // cannot fail for plain values
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// FileHash fingerprints an input file.
func FileHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// RunID names a run by its inputs and settings.
func RunID(bankSHA, ledgerSHA, configHash string) string {
	sum := sha256.Sum256([]byte(bankSHA + ledgerSHA + configHash))
	return hex.EncodeToString(sum[:])[:16]
}

// beforeCommit lets tests step in just before a file is saved.
var beforeCommit func()

// WriteAtomic replaces a file all at once, or leaves it untouched if interrupted.
func WriteAtomic(ctx context.Context, path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".recon-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Chmod(0o644); err != nil { // readable by others, like a normal file
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if beforeCommit != nil {
		beforeCommit()
	}
	if err = ctx.Err(); err != nil {
		return err // interrupted: keep the old file
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	return nil
}
