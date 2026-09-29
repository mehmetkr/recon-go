package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestFingerprints(t *testing.T) {
	base := func() Config {
		return Config{AlgoVersion: 1, TZ: "UTC", BankLayouts: []string{"2006-01-02"}, LedgerLayouts: []string{"2006-01-02"},
			DateTolerance: 3, FuzzyWindow: 7, MinRefLen: 5, ThresholdNum: 4, ThresholdDen: 5}
	}
	a, b, c := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	// Computed independently from the published formulas.
	for got, want := range map[string]string{
		ConfigHash(base()): "205fd73a04fd2b5b9eaecbd8ed6b5d44d91777cd39a2b19ccef9b7e020795af7",
		RunID(a, b, c):     "b20dd8bdd812e185",
		FileHash(nil):      "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	} {
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
	if RunID(b, a, c) == RunID(a, b, c) {
		t.Error("swapping the bank and ledger fingerprints must change the run identity")
	}
	// Every setting must change the fingerprint.
	for name, change := range map[string]func(*Config){
		"algo_version":   func(c *Config) { c.AlgoVersion = 2 },
		"tz":             func(c *Config) { c.TZ = "Europe/Berlin" },
		"bank_layouts":   func(c *Config) { c.BankLayouts = []string{"02/01/2006"} },
		"ledger_layouts": func(c *Config) { c.LedgerLayouts = append(c.LedgerLayouts, "2006-01-02T15:04:05Z07:00") },
		"date_tolerance": func(c *Config) { c.DateTolerance = 2 },
		"fuzzy_window":   func(c *Config) { c.FuzzyWindow = 8 },
		"min_ref_len":    func(c *Config) { c.MinRefLen = 6 },
		"threshold_num":  func(c *Config) { c.ThresholdNum = 3 },
		"threshold_den":  func(c *Config) { c.ThresholdDen = 4 },
	} {
		cfg := base()
		change(&cfg)
		if ConfigHash(cfg) == ConfigHash(base()) {
			t.Errorf("changing %s did not change the fingerprint", name)
		}
	}
}

func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "results.json")
	for _, content := range []string{"first", "second"} {
		if err := WriteAtomic(context.Background(), path, []byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(path)
	if b, _ := os.ReadFile(path); err != nil || string(b) != "second" || info.Mode().Perm() != 0o644 {
		t.Errorf("content %q, mode %v; want the latest content, readable like a normal file", b, info.Mode().Perm())
	}
	assertOnlyFiles(t, dir, "results.json")
	// The longest name most file systems allow must still work.
	if err := WriteAtomic(context.Background(), filepath.Join(dir, strings.Repeat("r", 250)+".json"), []byte("x")); err != nil {
		t.Errorf("long name: %v", err)
	}
	// Point the system temp folder nowhere, to prove it is not used.
	t.Setenv("TMPDIR", filepath.Join(dir, "does-not-exist"))
	if err := WriteAtomic(context.Background(), path, []byte("x")); err != nil {
		t.Errorf("the system temp folder was used: %v", err)
	}
}

func TestWriteAtomicKeepsTheOldFile(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, dir string) context.Context{
		"cancelled before writing": func(t *testing.T, dir string) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		},
		"interrupted while writing": func(t *testing.T, dir string) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			beforeCommit = func() {
				// The new file is fully written at this point.
				if entries, _ := os.ReadDir(dir); len(entries) != 2 {
					t.Errorf("expected the old file and one new one, found %d", len(entries))
				}
				cancel()
			}
			t.Cleanup(func() { beforeCommit = nil })
			return ctx
		},
		"write fails part-way": func(t *testing.T, dir string) context.Context {
			// Limit the file size so the write fails part-way.
			var old syscall.Rlimit
			if syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old) != nil {
				t.Skip("cannot read the file size limit")
			}
			limited := old
			limited.Cur = 4096
			if syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limited) != nil {
				t.Skip("cannot lower the file size limit")
			}
			t.Cleanup(func() { syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old) })
			return context.Background()
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "results.json")
			if err := os.WriteFile(path, []byte("previous"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := WriteAtomic(setup(t, dir), path, make([]byte, 64*1024)); err == nil {
				t.Error("the write was reported as successful")
			}
			if b, _ := os.ReadFile(path); string(b) != "previous" {
				t.Errorf("content = %q, want the old file untouched", b)
			}
			assertOnlyFiles(t, dir, "results.json")
		})
	}
}

func TestWriteAtomicFailures(t *testing.T) {
	dir := t.TempDir()
	occupied := filepath.Join(dir, "occupied")
	if err := os.MkdirAll(filepath.Join(occupied, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A file cannot replace a folder that has contents, nor go where no folder exists.
	for _, path := range []string{occupied, filepath.Join(dir, "missing", "f")} {
		if WriteAtomic(context.Background(), path, []byte("x")) == nil {
			t.Errorf("writing %s succeeded", path)
		}
	}
	assertOnlyFiles(t, dir, "occupied")
}

func assertOnlyFiles(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("folder holds %v, want %v", names, want)
	}
}
