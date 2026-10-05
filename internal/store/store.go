// Package store remembers runs and saves files safely.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// Resolve finds the file a path really names, following links even to a file not yet created.
func Resolve(path string) string {
	for range 40 { // enough hops for any sane chain of links
		dir, base := filepath.Split(path)
		where, err := filepath.EvalSymlinks(dir)
		if err != nil {
			if abs, err := filepath.Abs(path); err == nil {
				return abs // a missing folder cannot be written to, so the path is only made absolute
			}
			return path
		}
		if where, err = filepath.Abs(where); err != nil {
			return path
		}
		path = filepath.Join(where, base)
		target, err := os.Readlink(path)
		if err != nil {
			return path // not a link
		}
		if !filepath.IsAbs(target) {
			target = where + string(filepath.Separator) + target // left untidied, so ".." steps back from where the link leads
		}
		path = target
	}
	return path
}

// cause strips file names from an error, leaving only what went wrong.
func cause(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return linkErr.Err
	}
	return err
}

// beforeCommit lets tests step in just before a file is saved.
var beforeCommit func()

// afterCreate lets tests act on the temp file before the write.
var afterCreate func(*os.File)

// WriteAtomic replaces a file all at once, or leaves it untouched if interrupted.
func WriteAtomic(ctx context.Context, path string, data []byte) (err error) {
	defer func() { err = cause(err) }() // the caller names the file, so only the reason is kept
	path = Resolve(path)                // a link is kept, and the file it points to is replaced
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("too many links")
	}
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
	if afterCreate != nil {
		afterCreate(tmp)
	}
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
	return os.Rename(tmp.Name(), path)
}
