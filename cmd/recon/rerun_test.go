package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mehmetkr/recon-go/internal/domain"
	"github.com/mehmetkr/recon-go/internal/ingest"
	"github.com/mehmetkr/recon-go/internal/match"
	"github.com/mehmetkr/recon-go/internal/store"
)

// step runs the command against a shared state file and reads back the report.
func step(t *testing.T, state string, args ...string) (r results, stderr string) {
	t.Helper()
	code, stdout, stderr := runCLI(t, append(args, "-state", state, "-out", "-")...)
	if code != exitOK {
		t.Fatalf("%v: exit %d:\n%s", args, code, stderr)
	}
	if stdout == "" {
		return r, stderr
	}
	if json.Unmarshal([]byte(stdout), &r) != nil {
		t.Fatalf("unreadable report:\n%s", stdout)
	}
	// Every record read is accounted for exactly once.
	for _, side := range []string{"bank", "ledger"} {
		exceptions := 0
		for _, e := range r.Exceptions {
			if e.Source == side {
				exceptions++
			}
		}
		if got := r.Summary.Excluded[side] + len(r.Matches) + exceptions; got != r.Summary.Read[side] {
			t.Errorf("%s: %d records read, but %d accounted for", side, r.Summary.Read[side], got)
		}
	}
	return r, stderr
}

// excluded reports how many records per side were set aside as matched before.
func (r results) excluded() [2]int {
	return [2]int{r.Summary.Excluded["bank"], r.Summary.Excluded["ledger"]}
}

// assertClean checks that no half-written file was left in a folder, and that these files were never created.
func assertClean(t *testing.T, dir string, absent ...string) {
	t.Helper()
	if temps, _ := filepath.Glob(filepath.Join(dir, ".recon-*.tmp")); len(temps) != 0 {
		t.Errorf("left behind: %v", temps)
	}
	for _, name := range absent {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s was written", name)
		}
	}
}

func TestRerun(t *testing.T) {
	dir, bank, ledger := fixture(t, bankCSV, ledgerCSV)
	state := filepath.Join(dir, "state.json")
	var runID string // of the last run that did the work
	var first results
	for _, tt := range []struct {
		name     string
		flags    []string
		skipped  bool
		matches  int
		excluded [2]int
	}{
		{"first run", nil, false, 2, [2]int{}},
		{"the same run again", nil, true, 0, [2]int{}},
		{"another worker count", []string{"-workers", strconv.Itoa(runtime.GOMAXPROCS(0) + 1)}, true, 0, [2]int{}},
		{"another reference window", []string{"-fuzzy-window", "8"}, false, 0, [2]int{2, 2}},
		{"a narrower date window, keeping the pair it can no longer see", []string{"-date-tolerance", "1"}, false, 0, [2]int{2, 2}},
		{"forced", []string{"-force"}, false, 0, [2]int{2, 2}},
	} {
		before, _ := os.Stat(state)
		bytesBefore, _ := os.ReadFile(state)
		r, stderr := step(t, state, append([]string{"-bank", bank, "-ledger", ledger}, tt.flags...)...)
		after, _ := os.Stat(state)
		bytesAfter, _ := os.ReadFile(state)
		if r.RunID != "" {
			runID = r.RunID
		}
		if first.RunID == "" {
			first = r
		}
		logged := "already reconciled; nothing written (use -force to run again)"
		if !tt.skipped {
			logged = fmt.Sprintf("excluded_bank=%d excluded_ledger=%d", tt.excluded[0], tt.excluded[1])
		}
		if !strings.Contains(stderr, logged) || !strings.Contains(stderr, "run_id="+runID) {
			t.Errorf("%s: want %q logged for run %s:\n%s", tt.name, logged, runID, stderr)
		}
		if tt.skipped && (!os.SameFile(before, after) || string(bytesBefore) != string(bytesAfter)) {
			t.Errorf("%s: a skipped run rewrote the state", tt.name)
		}
		if len(r.Matches) != tt.matches || r.excluded() != tt.excluded {
			t.Errorf("%s: %d new matches, excluded %v; want %d and %v", tt.name, len(r.Matches), r.excluded(), tt.matches, tt.excluded)
		}
	}
	var rules []string
	for _, m := range first.Matches {
		rules = append(rules, m.Rule)
	}
	if !slices.Contains(rules, "date_tolerance") {
		t.Errorf("first run rules %v: a date_tolerance pair is needed for the narrower window to lose", rules)
	}
	// Each run is remembered by its inputs and settings; the forced one repeats the first.
	want := store.Run{RunID: first.RunID, BankSHA: store.FileHash([]byte(bankCSV)), LedgerSHA: store.FileHash([]byte(ledgerCSV)),
		ConfigHash: first.Config["config_hash"].(string), NewMatches: 2}
	forced := want
	forced.NewMatches = 0
	s, err := store.Load(state)
	if err != nil || len(s.Matches) != 2 || len(s.Runs) != 4 || s.Runs[0] != want || s.Runs[3] != forced {
		t.Errorf("state: %v; want 2 matches and 4 runs, first %+v and last %+v, got %+v", err, want, forced, s.Runs)
	}
	assertClean(t, dir)
}

func TestOverlappingExports(t *testing.T) {
	a, b, c := "ACC-1,2026-09-01,-150.00,EUR,INV-1001\n", "ACC-1,2026-09-02,75.50,EUR,INV-1002\n", "ACC-1,2026-09-03,20.00,EUR,INV-1003\n"
	la, lb, lc := "ACC-1,2026-09-01,,150.00,EUR,INV-1001\n", "ACC-1,2026-09-02,75.50,,EUR,INV-1002\n", "ACC-1,2026-09-03,20.00,,EUR,INV-1003\n"
	row, lrow := "ACC-1,2026-09-05,10.00,EUR,R-0001\n", "ACC-1,2026-09-05,10.00,,EUR,R-0001\n"
	dir := t.TempDir()
	export := func(name string, lines ...string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	ledger := export("ledger.csv", ledgerHeader, la, lb, lc, lrow, lrow)

	t.Run("only new rows are matched", func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "state.json")
		step(t, state, "-bank", export("week1.csv", bankHeader, a, b), "-ledger", ledger)
		// The next ledger export no longer holds the first row.
		trimmed := export("trimmed.csv", ledgerHeader, lb, lc, lrow, lrow)
		r, stderr := step(t, state, "-bank", export("week2.csv", bankHeader, a, b, c), "-ledger", trimmed)
		if len(r.Matches) != 1 || r.excluded() != [2]int{2, 1} || !strings.Contains(stderr, "excluded_bank=2 excluded_ledger=1") {
			t.Errorf("%d new matches, excluded %v; want 1 and [2 1], logged per side:\n%s", len(r.Matches), r.excluded(), stderr)
		}
	})
	t.Run("identical rows split across exports", func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "state.json")
		first, _ := step(t, state, "-bank", export("once.csv", bankHeader, row), "-ledger", ledger)
		second, _ := step(t, state, "-bank", export("twice.csv", bankHeader, row, row), "-ledger", ledger)
		if len(first.Matches) != 1 || len(second.Matches) != 1 || second.excluded() != [2]int{1, 1} ||
			second.Matches[0].LedgerID == first.Matches[0].LedgerID {
			t.Errorf("want the second copy paired with the other ledger row: %+v then %+v", first.Matches, second.Matches)
		}
	})
	t.Run("a partner matched earlier explains a newcomer", func(t *testing.T) {
		only := export("b.csv", bankHeader, b)
		later := export("later.csv", ledgerHeader, la, lb, lc, lrow, lrow, "ACC-1,2026-09-04,75.50,,EUR,SOMETHING ELSE\n")
		for _, side := range []struct {
			name, bank, ledger string
			partner            func(results) string
		}{
			{"on the bank side", export("late.csv", bankHeader, b, "ACC-1,2026-09-04,75.50,EUR,SOMETHING ELSE\n"), ledger,
				func(r results) string { return r.Matches[0].LedgerID }},
			{"on the ledger side", only, later,
				func(r results) string { return r.Matches[0].BankID }},
		} {
			state := filepath.Join(t.TempDir(), "state.json")
			first, _ := step(t, state, "-bank", only, "-ledger", ledger)
			r, _ := step(t, state, "-bank", side.bank, "-ledger", side.ledger)
			var taken []string
			for _, e := range r.Exceptions {
				if e.Reason == "counterpart_taken" {
					taken = append(taken, strings.Join(e.Candidates, " "))
				}
			}
			if len(first.Matches) != 1 || !slices.Equal(taken, []string{side.partner(first)}) {
				t.Errorf("%s: counterpart_taken candidates %v, want one exception naming the stored partner", side.name, taken)
			}
		}
	})
}

func TestStateGuards(t *testing.T) {
	dir, bank, ledger := fixture(t, bankCSV, ledgerCSV)
	state := filepath.Join(dir, "state.json")
	step(t, state, "-bank", bank, "-ledger", ledger)
	os.WriteFile(filepath.Join(dir, "corrupt.json"), []byte(`{"version": 1,`), 0o644)
	// A missing bank file proves a guard fires before the inputs are even read.
	absent := filepath.Join(dir, "absent.csv")
	for _, tt := range []struct {
		name, bank, state, out, want string
		flags                        []string
	}{
		{"another time zone", absent, "state.json", "results.json", state + ": this state was built with -tz UTC", []string{"-tz", "Europe/Berlin"}},
		{"another date format", absent, "state.json", "results.json", "use the same settings", []string{"-ledger-date-layout", "2006-01-02"}},
		{"an unreadable state", absent, "corrupt.json", "results.json", "unreadable", nil},
		{"an unwritable report", bank, "state.json", "missing/r.json", "writing report " + filepath.Join(dir, "missing", "r.json"), []string{"-force"}},
	} {
		os.WriteFile(filepath.Join(dir, "results.json"), []byte("earlier report"), 0o644)
		path, out := filepath.Join(dir, tt.state), filepath.Join(dir, tt.out)
		before, _ := os.ReadFile(path)
		reportBefore, _ := os.ReadFile(out)
		code, _, stderr := runCLI(t, append([]string{"-bank", tt.bank, "-ledger", ledger, "-state", path, "-out", out}, tt.flags...)...)
		after, _ := os.ReadFile(path)
		reportAfter, _ := os.ReadFile(out)
		if code != exitFatal || !strings.Contains(stderr, tt.want) || string(before) != string(after) || string(reportBefore) != string(reportAfter) {
			t.Errorf("%s: exit %d, want %d naming %q with the state and report untouched:\n%s", tt.name, code, exitFatal, tt.want, stderr)
		}
	}
	assertClean(t, dir)
}

// cancelledOnceExists behaves as if Ctrl-C arrives the moment a file appears.
type cancelledOnceExists struct {
	context.Context
	path string
}

func (c cancelledOnceExists) Err() error {
	if _, err := os.Stat(c.path); err == nil {
		return context.Canceled
	}
	return nil
}

func TestInterruptBetweenWrites(t *testing.T) {
	dir, bank, ledger := fixture(t, bankCSV, ledgerCSV)
	out, state := filepath.Join(dir, "results.json"), filepath.Join(dir, "state.json")
	var stderr bytes.Buffer
	ctx := cancelledOnceExists{context.Background(), out}
	code := run(ctx, []string{"-bank", bank, "-ledger", ledger, "-out", out, "-state", state}, io.Discard, &stderr)
	if code != exitFatal || !strings.Contains(stderr.String(), "context canceled") {
		t.Errorf("exit %d, want %d:\n%s", code, exitFatal, stderr.String())
	}
	// The report stays; the state is not saved, so the next run does the work again.
	if b, _ := os.ReadFile(out); !strings.Contains(string(b), `"run_id"`) {
		t.Error("the report was not kept")
	}
	assertClean(t, dir, "state.json")
}

func TestRefusedCommitWritesNothing(t *testing.T) {
	dir, bank, ledger := fixture(t, bankCSV, ledgerCSV)
	// A faulty matcher pairs one bank record twice.
	runMatch = func(context.Context, []domain.Transaction, []domain.Transaction, []domain.Transaction, match.Params, int) (match.Result, error) {
		return match.Result{Matches: []domain.Match{{MatchID: "m1", BankID: "b", LedgerID: "l1"}, {MatchID: "m2", BankID: "b", LedgerID: "l2"}}}, nil
	}
	t.Cleanup(func() { runMatch = match.Run })
	code, _, stderr := runCLI(t, "-bank", bank, "-ledger", ledger, "-out", filepath.Join(dir, "results.json"), "-state", filepath.Join(dir, "state.json"))
	if code != exitFatal || !strings.Contains(stderr, "matched twice") {
		t.Errorf("exit %d, want %d refusing the commit:\n%s", code, exitFatal, stderr)
	}
	assertClean(t, dir, "results.json", "state.json")
}

func TestStateDefaultsToStateJSON(t *testing.T) {
	dir, bank, ledger := fixture(t, bankCSV, ledgerCSV)
	t.Chdir(dir)
	var stderr bytes.Buffer
	args := []string{"-bank", bank, "-ledger", ledger, "-out", "-", "-bank-date-layout", "2006-01-02"}
	if code := run(context.Background(), args, io.Discard, &stderr); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, stderr.String())
	}
	// The run lands in the default file, with each side's date formats kept apart.
	s, err := store.Load("state.json")
	if err != nil || len(s.Runs) != 1 || s.TZ != "UTC" ||
		!slices.Equal(s.BankLayouts, []string{"2006-01-02"}) || !slices.Equal(s.LedgerLayouts, ingest.DefaultLayouts()) {
		t.Errorf("state %+v, %v; want one run, UTC, and the bank's own date format", s, err)
	}
}
