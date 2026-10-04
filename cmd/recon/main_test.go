package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mehmetkr/recon-go/internal/domain"
	"github.com/mehmetkr/recon-go/internal/match"
)

var update = flag.Bool("update", false, "rewrite golden files")

// The header rows of the two exports.
const bankHeader, ledgerHeader = "account,date,amount,currency,reference\n", "account,date,debit,credit,currency,reference\n"

// The two sides hold different numbers of rows, so a mix-up would show.
const bankCSV = bankHeader +
	"ACC-1,2026-09-01,-150.00,EUR,INV-1001\n" +
	"ACC-1,2026-09-03,980.00,EUR,Payout\n" +
	"ACC-1,2026-09-02,75.50,EUR,x\n" +
	"ACC-2,2026-09-02,12.00,USD,unmatched\n" +
	"broken row\n"

const ledgerCSV = ledgerHeader +
	"ACC-1,2026-09-01,,150.00,EUR,INV1001\n" +
	"ACC-1,2026-09-01,1000.00,,EUR,Payout\n" +
	"ACC-1,2026-09-04,75.50,,EUR,y\n" +
	"ACC-1,2026-09-05,10.00,5.00,EUR,both sides\n"

// fixture writes the two input files and returns their paths.
func fixture(t *testing.T, bank, ledger string) (dir, bankPath, ledgerPath string) {
	t.Helper()
	dir = t.TempDir()
	bankPath, ledgerPath = filepath.Join(dir, "bank.csv"), filepath.Join(dir, "ledger.csv")
	for path, content := range map[string]string{bankPath: bank, ledgerPath: ledger} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, bankPath, ledgerPath
}

// withState gives a run its own empty state file unless one is named.
func withState(t *testing.T, args []string) []string {
	if slices.Contains(args, "-state") {
		return args
	}
	return append(slices.Clone(args), "-state", filepath.Join(t.TempDir(), "state.json"))
}

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	return runCLIWith(t, match.Run, args...)
}

func runCLIWith(t *testing.T, matcher matchFunc, args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(context.Background(), withState(t, args), matcher, &out, &errOut)
	return code, out.String(), errOut.String()
}

type results struct {
	RunID   string         `json:"run_id"`
	Config  map[string]any `json:"config"`
	Matches []struct {
		MatchID  string `json:"match_id"`
		BankID   string `json:"bank_id"`
		LedgerID string `json:"ledger_id"`
		Rule     string `json:"rule"`
		DayDelta int    `json:"day_delta"`
	} `json:"matches"`
	Exceptions []struct {
		Source     string   `json:"source"`
		ID         string   `json:"id"`
		Reason     string   `json:"reason"`
		Candidates []string `json:"candidates"`
	} `json:"exceptions"`
	Malformed []struct {
		Source string   `json:"source"`
		Line   int      `json:"line"`
		Reason string   `json:"reason"`
		Detail string   `json:"detail"`
		Raw    []string `json:"raw"`
	} `json:"malformed"`
	Summary struct {
		Read       map[string]int            `json:"read"`
		Matches    map[string]int            `json:"matches"`
		Exceptions map[string]map[string]int `json:"exceptions"`
		Malformed  map[string]int            `json:"malformed"`
		Excluded   map[string]int            `json:"excluded"`
	} `json:"summary"`
}

// reportOf runs the command with the report on standard output and reads it back.
func reportOf(t *testing.T, args ...string) results {
	t.Helper()
	r, _ := step(t, filepath.Join(t.TempDir(), "state.json"), args...)
	return r
}

// reportOfWith is reportOf with a custom matcher.
func reportOfWith(t *testing.T, matcher matchFunc, args ...string) results {
	t.Helper()
	r, _ := stepWith(t, matcher, filepath.Join(t.TempDir(), "state.json"), args...)
	return r
}

// goldenFixtures returns paths to the comprehensive fixture pair.
func goldenFixtures(t *testing.T) (bank, ledger string) {
	t.Helper()
	bank = filepath.Join(testdataDir, "bank.csv")
	ledger = filepath.Join(testdataDir, "ledger.csv")
	for _, f := range []string{bank, ledger} {
		if _, err := os.Stat(f); err != nil {
			t.Fatal(err)
		}
	}
	return bank, ledger
}

// golden returns the reviewed report for the fixture.
func golden(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testdataDir, "fixture.golden.json"))
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	return string(b)
}

// TestOutput compares the full report with a reviewed copy (refresh with -update).
func TestOutput(t *testing.T) {
	bank, ledger := goldenFixtures(t)
	dir := t.TempDir()
	t.Chdir(dir)
	if *update {
		_, stdout, _ := runCLI(t, "-bank", bank, "-ledger", ledger, "-out", "-")
		if err := os.WriteFile(filepath.Join(testdataDir, "fixture.golden.json"), []byte(stdout), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := golden(t)
	for _, tt := range []struct {
		name, out string
		extra     []string
	}{
		{"to standard output", "-", nil},
		{"to a file", "written.json", nil},
		{"to the default file", "", nil},
		{"one worker", "-", []string{"-workers", "1"}},
		{"many workers", "-", []string{"-workers", "16"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"-bank", bank, "-ledger", ledger}, tt.extra...)
			if tt.out != "" {
				args = append(args, "-out", tt.out)
			}
			code, stdout, stderr := runCLI(t, args...)
			got := stdout
			if tt.out != "-" {
				file := tt.out
				if file == "" {
					file = "results.json"
				}
				b, _ := os.ReadFile(file)
				got = string(b)
				if stdout != "" || !strings.Contains(stderr, "out="+file) {
					t.Errorf("a file report must leave standard output empty and log its path:\n%s", stderr)
				}
				os.Remove(file)
			}
			if diff := cmp.Diff(want, got); code != exitOK || diff != "" {
				t.Fatalf("exit %d, report differs from the reviewed copy (-want +got):\n%s", code, diff)
			}
			var r results
			json.Unmarshal([]byte(got), &r)
			for _, field := range []string{
				"msg=reconciled", "run_id=" + r.RunID,
				"bank=12", "ledger=9", "matched=6", "exceptions=9", "malformed=5",
			} {
				if !strings.Contains(stderr, field) {
					t.Errorf("the summary line lacks %q:\n%s", field, stderr)
				}
			}
		})
	}
}

// TestRealMainWritesReportToStdout runs the real command and checks where output goes.
func TestRealMainWritesReportToStdout(t *testing.T) {
	bank, ledger := goldenFixtures(t)
	cmd := exec.Command(testBinary, withState(t, []string{"-bank", bank, "-ledger", ledger, "-out", "-"})...)
	cmd.Env = append(os.Environ(), "RECON_TEST_RUN_MAIN=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v, stderr:\n%s", err, stderr.String())
	}
	if diff := cmp.Diff(golden(t), stdout.String()); diff != "" {
		t.Errorf("standard output differs from the reviewed report (-want +got):\n%s", diff)
	}
	if lines := strings.Split(strings.TrimSpace(stderr.String()), "\n"); len(lines) != 1 || !strings.Contains(lines[0], "msg=reconciled") {
		t.Errorf("standard error should hold only the summary line:\n%s", stderr.String())
	}
}

// TestShuffleDeterminism reorders both input files and checks the result is identical.
func TestShuffleDeterminism(t *testing.T) {
	bank, ledger := goldenFixtures(t)
	bankLines := dataLines(t, bank)
	ledgerLines := dataLines(t, ledger)
	want := golden(t)
	var ref results
	json.Unmarshal([]byte(want), &ref)

	wantCanon := canonicalReport(ref)
	for i := range 50 {
		sb, sl := shuffled(t, bankLines, bankHeader, i), shuffled(t, ledgerLines, ledgerHeader, i+1000)
		r, _ := step(t, filepath.Join(t.TempDir(), "state.json"), "-bank", sb, "-ledger", sl)
		if diff := cmp.Diff(wantCanon, canonicalReport(r)); diff != "" {
			t.Fatalf("permutation %d differs (-want +got):\n%s", i, diff)
		}
	}
}

// dataLines reads a CSV file and returns only the data lines (no header).
func dataLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("%s has no data lines", path)
	}
	return lines[1:]
}

// shuffled writes a CSV with the header first and data lines in a seeded permutation.
func shuffled(t *testing.T, lines []string, header string, seed int) string {
	t.Helper()
	perm := make([]string, len(lines))
	copy(perm, lines)
	for i := len(perm) - 1; i > 0; i-- {
		j := (seed*31 + i*17) % (i + 1)
		if j < 0 {
			j += i + 1
		}
		perm[i], perm[j] = perm[j], perm[i]
	}
	path := filepath.Join(t.TempDir(), "shuffled.csv")
	if err := os.WriteFile(path, []byte(header+strings.Join(perm, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// canonicalReport zeroes fields that change when inputs are shuffled.
func canonicalReport(r results) results {
	c := r
	c.RunID = ""
	c.Config = nil
	c.Malformed = slices.Clone(c.Malformed)
	for i := range c.Malformed {
		c.Malformed[i].Line = 0
	}
	return c
}

func TestFlagsReachTheEngine(t *testing.T) {
	stamped := "ACC-1,2026-09-01T10:00:00Z,1.00,EUR,INV10001"
	for _, tt := range []struct {
		name, bank, ledger, want string
		flags                    []string
	}{
		{"timestamps parse by default", stamped, "ACC-1,2026-09-01T11:00:00+01:00,1.00,,EUR,INV10001", "exact", nil},
		{"a date format replaces the defaults", "ACC-1,03/04/2026,1.00,EUR,INV10001", "ACC-1,2026-04-03,1.00,,EUR,INV10001", "exact",
			[]string{"-bank-date-layout", "02/01/2006"}},
		// Five days apart with matching references: the windows decide.
		{"default windows", "ACC-1,2026-09-06,1.00,EUR,INV10001", "ACC-1,2026-09-01,1.00,,EUR,INV10001", "fuzzy_reference", nil},
		{"narrow reference window", "ACC-1,2026-09-06,1.00,EUR,INV10001", "ACC-1,2026-09-01,1.00,,EUR,INV10001", "",
			[]string{"-fuzzy-window", "4"}},
		{"wide date window", "ACC-1,2026-09-06,1.00,EUR,INV10001", "ACC-1,2026-09-01,1.00,,EUR,INV10001", "date_tolerance",
			[]string{"-date-tolerance", "5", "-fuzzy-window", "6"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, bank, ledger := fixture(t, bankHeader+tt.bank+"\n",
				ledgerHeader+tt.ledger+"\n")
			var rules []string
			for _, m := range reportOf(t, append([]string{"-bank", bank, "-ledger", ledger}, tt.flags...)...).Matches {
				rules = append(rules, m.Rule)
			}
			if strings.Join(rules, ",") != tt.want {
				t.Errorf("matched by %v, want %q", rules, tt.want)
			}
		})
	}
}

func TestConfigEcho(t *testing.T) {
	_, bank, ledger := fixture(t, bankCSV, ledgerCSV)
	conf := reportOf(t, "-bank", bank, "-ledger", ledger,
		"-tz", "Europe/Berlin", "-bank-date-layout", "02/01/2006", "-date-tolerance", "1", "-fuzzy-window", "9").Config
	delete(conf, "config_hash")
	got, _ := json.Marshal(conf)
	want := `{"algo_version":1,"bank_layouts":["02/01/2006"],"date_tolerance":1,"fuzzy_window":9,` +
		`"ledger_layouts":["2006-01-02","2006-01-02T15:04:05Z07:00"],"min_ref_len":5,"threshold_den":5,"threshold_num":4,"tz":"Europe/Berlin"}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestRunIDInputs(t *testing.T) {
	dir, bank, ledger := fixture(t, bankCSV, ledgerCSV)
	otherBank, otherLedger := filepath.Join(dir, "bank2.csv"), filepath.Join(dir, "ledger2.csv")
	os.WriteFile(otherBank, []byte(bankCSV+"ACC-1,2026-09-09,1.00,EUR,z\n"), 0o644)
	os.WriteFile(otherLedger, []byte(ledgerCSV+"ACC-1,2026-09-09,1.00,,EUR,z\n"), 0o644)
	ids := map[string]string{}
	for name, args := range map[string][]string{
		"base":          {"-bank", bank, "-ledger", ledger},
		"other ledger":  {"-bank", bank, "-ledger", otherLedger},
		"other bank":    {"-bank", otherBank, "-ledger", ledger},
		"tz":            {"-bank", bank, "-ledger", ledger, "-tz", "Europe/Berlin"},
		"N":             {"-bank", bank, "-ledger", ledger, "-date-tolerance", "2"},
		"M":             {"-bank", bank, "-ledger", ledger, "-fuzzy-window", "8"},
		"bank layout":   {"-bank", bank, "-ledger", ledger, "-bank-date-layout", "2006-01-02"},
		"ledger layout": {"-bank", bank, "-ledger", ledger, "-ledger-date-layout", "2006-01-02"},
	} {
		r := reportOf(t, args...)
		if prev, dup := ids[r.RunID]; dup {
			t.Errorf("%s and %s share the run identity %s", name, prev, r.RunID)
		}
		ids[r.RunID] = name
		if name == "base" {
			// The run identity follows the published formula.
			sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
			if want := sum(sum(bankCSV) + sum(ledgerCSV) + r.Config["config_hash"].(string))[:16]; r.RunID != want {
				t.Errorf("run identity %s, want %s", r.RunID, want)
			}
		}
	}
}

func TestWorkersFlagReachesTheRunner(t *testing.T) {
	_, bank, ledger := fixture(t, bankCSV, ledgerCSV)
	var got []int
	spy := func(ctx context.Context, b, l, tk []domain.Transaction, p match.Params, workers int) (match.Result, error) {
		got = append(got, workers)
		return match.Run(ctx, b, l, tk, p, workers)
	}
	reportOfWith(t, spy, "-bank", bank, "-ledger", ledger, "-workers", "1")
	reportOfWith(t, spy, "-bank", bank, "-ledger", ledger, "-workers", "5")
	procs := runtime.NumCPU() + 1
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(procs))
	reportOfWith(t, spy, "-bank", bank, "-ledger", ledger)
	if want := []int{1, 5, procs}; !slices.Equal(got, want) {
		t.Errorf("the runner saw workers %v, want %v", got, want)
	}
}

func TestPathsMustNotClash(t *testing.T) {
	dir, _, _ := fixture(t, bankCSV, ledgerCSV)
	t.Chdir(dir)
	os.Link("bank.csv", "hard.csv")
	os.Symlink("ledger.csv", "sym.csv")
	os.Symlink("bank.csv", "bank_link.csv")
	os.Mkdir("d", 0o755)
	os.Symlink("d", "d_link")
	os.WriteFile("-", []byte(bankCSV), 0o644)
	os.WriteFile("old.json", nil, 0o644)
	abs := filepath.Join(dir, "bank.csv")
	outClash := func(out string) string { return "-out " + out + " is also an input file" }
	for _, tt := range []struct {
		bank, out, state, msg string
		code                  int
	}{
		{"bank.csv", abs, "s.json", outClash(abs), exitUsage},
		{"bank.csv", "ledger.csv", "s.json", outClash("ledger.csv"), exitUsage},
		{"bank.csv", "./bank.csv", "s.json", outClash("./bank.csv"), exitUsage},
		{abs, "bank.csv", "s.json", outClash("bank.csv"), exitUsage},
		{"bank.csv", "./ledger.csv", "s.json", outClash("./ledger.csv"), exitUsage},
		{"bank.csv", "hard.csv", "s.json", outClash("hard.csv"), exitUsage},
		{"bank.csv", "sym.csv", "s.json", outClash("sym.csv"), exitUsage},
		{"bank_link.csv", "bank.csv", "s.json", outClash("bank.csv"), exitUsage},
		// The state file follows the same rule, and must not be the report either.
		{"bank.csv", "r.json", "./ledger.csv", "-state ./ledger.csv is also an input file", exitUsage},
		{"bank.csv", "r.json", "hard.csv", "-state hard.csv is also an input file", exitUsage},
		{"bank.csv", "d_link/x.json", "d/x.json", "-out and -state must be different files", exitUsage},
		// In a missing folder, two files fail only when written, while one file named twice still clashes.
		{"bank.csv", "missing/r.json", "missing/s.json", "no such file or directory", exitFatal},
		{"bank.csv", "missing/r.json", "./missing/r.json", "-out and -state must be different files", exitUsage},
		// An earlier report is simply replaced.
		{"bank.csv", "old.json", "s.json", "", exitOK},
		// An input named "-" is a file, while -out - is standard output.
		{"-", "-", "s.json", "", exitOK},
		// A state named "-" is a file too, here one that holds no state.
		{"bank.csv", "-", "-", "- is unreadable", exitFatal},
	} {
		os.Remove("s.json")
		code, _, stderr := runCLI(t, "-bank", tt.bank, "-ledger", "ledger.csv", "-out", tt.out, "-state", tt.state)
		if code != tt.code || !strings.Contains(stderr, tt.msg) {
			t.Errorf("-bank %s -out %s -state %s: exit %d, want %d explaining %q:\n%s", tt.bank, tt.out, tt.state, code, tt.code, tt.msg, stderr)
		}
	}
	for file, want := range map[string]string{"bank.csv": bankCSV, "ledger.csv": ledgerCSV} {
		if b, _ := os.ReadFile(file); string(b) != want {
			t.Errorf("%s was modified", file)
		}
	}
}

// secondCheckCancels behaves as if Ctrl-C arrives while the report is being saved.
type secondCheckCancels struct {
	context.Context
	calls int
}

func (c *secondCheckCancels) Err() error {
	if c.calls++; c.calls >= 2 {
		return context.Canceled
	}
	return nil
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestNoReportAfterInterrupt(t *testing.T) {
	file := filepath.Join(t.TempDir(), "r.json")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tt := range []struct {
		name, out string
		ctx       func() (context.Context, io.Writer)
	}{
		{"file, before saving", file, func() (context.Context, io.Writer) { return cancelled, io.Discard }},
		{"stdout, before writing", "-", func() (context.Context, io.Writer) { return cancelled, &bytes.Buffer{} }},
		{"file, while saving", file, func() (context.Context, io.Writer) {
			return &secondCheckCancels{Context: context.Background()}, io.Discard
		}},
		{"stdout, while writing", "-", func() (context.Context, io.Writer) {
			ctx, cancel := context.WithCancel(context.Background())
			return ctx, writerFunc(func(p []byte) (int, error) { cancel(); return len(p), nil })
		}},
	} {
		ctx, w := tt.ctx()
		if err := writeReport(ctx, tt.out, []byte("{}\n"), w); !errors.Is(err, context.Canceled) {
			t.Errorf("%s: err = %v, want context.Canceled", tt.name, err)
		}
		if buf, ok := w.(*bytes.Buffer); ok && buf.Len() != 0 {
			t.Errorf("%s: a report was written", tt.name)
		}
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("an interrupted report was saved")
	}
}

func TestInterruptedRunIsFatal(t *testing.T) {
	dir, bank, ledger := fixture(t, bankCSV, ledgerCSV)
	for _, afterMatching := range []bool{false, true} {
		for _, out := range []string{filepath.Join(dir, "results.json"), "-"} {
			ctx, cancel := context.WithCancel(context.Background())
			if !afterMatching {
				cancel()
			}
			interrupter := func(rctx context.Context, b, l, tk []domain.Transaction, p match.Params, w int) (match.Result, error) {
				res, err := match.Run(rctx, b, l, tk, p, w)
				cancel()
				if rctx.Err() == nil {
					t.Error("the runner did not receive the command's context")
				}
				return res, err
			}
			var stdout, stderr bytes.Buffer
			code := run(ctx, withState(t, []string{"-bank", bank, "-ledger", ledger, "-out", out}), interrupter, &stdout, &stderr)
			cancel()
			if _, err := os.Stat(filepath.Join(dir, "results.json")); code != exitFatal || stdout.Len() != 0 || !os.IsNotExist(err) {
				t.Errorf("-out %s (after matching: %v): exit %d; want exit %d and nothing written", out, afterMatching, code, exitFatal)
			}
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

func TestFatalErrors(t *testing.T) {
	dir, bank, ledger := fixture(t, bankCSV, ledgerCSV)
	files := map[string]string{
		"bad_bank.csv":      "account,date\n",
		"bad_ledger.csv":    "no,header,here\n",
		"latin1_bank.csv":   bankHeader + "ACC-1,2026-09-01,1.00,EUR,M\xfcller\n",
		"latin1_ledger.csv": ledgerHeader + "ACC-1,2026-09-01,1.00,,EUR,M\xfcller\n",
	}
	for name, content := range files {
		os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
	}
	in := func(name string) string { return filepath.Join(dir, name) }
	out := in("results.json")
	for _, tt := range []struct {
		bank, ledger, out, want string
		stdout                  io.Writer
	}{
		{in("nope.csv"), ledger, out, in("nope.csv"), nil},
		{in("bad_bank.csv"), ledger, out, in("bad_bank.csv"), nil},
		{bank, in("bad_ledger.csv"), out, in("bad_ledger.csv"), nil},
		{in("latin1_bank.csv"), ledger, "-", in("latin1_bank.csv") + ": not valid UTF-8", nil},
		{bank, in("latin1_ledger.csv"), "-", in("latin1_ledger.csv") + ": not valid UTF-8", nil},
		{bank, ledger, in("missing/r.json"), "no such file or directory", nil},
		{bank, ledger, "-", "file already closed", failingWriter{}},
	} {
		var stdout, stderr bytes.Buffer
		var w io.Writer = &stdout
		if tt.stdout != nil {
			w = tt.stdout
		}
		code := run(context.Background(), withState(t, []string{"-bank", tt.bank, "-ledger", tt.ledger, "-out", tt.out}), match.Run, w, &stderr)
		if _, err := os.Stat(out); code != exitFatal || stdout.Len() != 0 || !os.IsNotExist(err) ||
			!strings.Contains(stderr.String(), "level=ERROR") || !strings.Contains(stderr.String(), tt.want) {
			t.Errorf("want exit %d naming %q and nothing written; got exit %d:\n%s", exitFatal, tt.want, code, stderr.String())
		}
	}
}

func TestUsageErrors(t *testing.T) {
	_, bank, ledger := fixture(t, bankCSV, ledgerCSV)
	both := []string{"-bank", bank, "-ledger", ledger, "-out", "-"}
	with := func(extra ...string) []string { return append(slices.Clone(both), extra...) }
	for _, tt := range []struct {
		args []string
		msg  string // expected explanation
		code int
	}{
		{[]string{"-h"}, "-fuzzy-window", exitOK},
		{nil, "-bank and -ledger are required", exitUsage},
		{[]string{"-bank", bank}, "-bank and -ledger are required", exitUsage},
		{with("-nope"), "flag provided but not defined: -nope", exitUsage},
		{with("extra"), "unexpected arguments: extra", exitUsage},
		{with("-date-tolerance", "7"), "fuzzy window must be larger than the date tolerance", exitUsage},
		{with("-date-tolerance", "-1"), "date tolerance must not be negative", exitUsage},
		{with("-date-tolerance", "x"), `invalid value "x" for flag -date-tolerance`, exitUsage},
		{with("-workers", "0"), "-workers must be at least 1", exitUsage},
		{with("-tz", "Local"), `time zone "Local" is machine-dependent`, exitUsage},
		{with("-tz", "./localtime"), `time zone "./localtime" is machine-dependent`, exitUsage},
		{with("-tz", ""), "time zone must not be empty", exitUsage},
		{with("-tz", "Mars/Olympus"), `unknown time zone "Mars/Olympus"`, exitUsage},
		{with("-ledger-date-layout", "2006-01-02 MST"), "uses a zone abbreviation", exitUsage},
		{with("-bank-date-layout", "Jan _2 15:04:05 MST 2006"), "uses a zone abbreviation", exitUsage},
		{[]string{"-bank", bank, "-ledger", ledger, "-out", ""}, `-out must be a file path or "-"`, exitUsage},
		{with("-state", ""), "-state must be a file path", exitUsage},
	} {
		if code, stdout, stderr := runCLI(t, tt.args...); code != tt.code || stdout != "" || !strings.Contains(stderr, tt.msg) {
			t.Errorf("%v: exit %d, stdout %q, stderr:\n%s\nwant exit %d explaining %q", tt.args, code, stdout, stderr, tt.code, tt.msg)
		}
	}
}
