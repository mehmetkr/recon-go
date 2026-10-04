package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mehmetkr/recon-go/internal/domain"
)

func match(bank, ledger string) domain.Match {
	return domain.Match{MatchID: domain.MatchID(bank, ledger), BankID: bank, LedgerID: ledger, Rule: domain.RuleExact}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Load(path)
	if err != nil || len(s.Runs) != 0 || len(s.Matches) != 0 {
		t.Fatalf("a missing file must be an empty state: %+v, %v", s, err)
	}
	if err := s.Lock("UTC", []string{"2006-01-02"}, []string{"02.01.2006"}); err != nil {
		t.Fatal(err)
	}
	m := match("b1", "l1")
	m.Rule = domain.RuleDateTolerance
	if err := s.AddRun(Run{RunID: "r1", BankSHA: "b", LedgerSHA: "l", ConfigHash: "c"}, []domain.Match{m}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	// The file format is a promise to future versions, so its exact shape is pinned.
	want := `{
  "version": 1,
  "id_scheme": 1,
  "tz": "UTC",
  "bank_layouts": [
    "2006-01-02"
  ],
  "ledger_layouts": [
    "02.01.2006"
  ],
  "runs": [
    {
      "run_id": "r1",
      "bank_sha256": "b",
      "ledger_sha256": "l",
      "config_hash": "c",
      "new_matches": 1
    }
  ],
  "matches": {
    "` + match("b1", "l1").MatchID + `": {
      "bank_id": "b1",
      "ledger_id": "l1",
      "rule": "date_tolerance",
      "run_id": "r1"
    }
  }
}
`
	if got, _ := os.ReadFile(path); string(got) != want {
		t.Errorf("saved:\n%s\nwant:\n%s", got, want)
	}
	back, err := Load(path)
	if diff := cmp.Diff(s, back); err != nil || diff != "" {
		t.Fatalf("%v (-saved +loaded):\n%s", err, diff)
	}
	// A file with no matches is still usable.
	os.WriteFile(path, []byte(`{"version": 1, "id_scheme": 1, "matches": null}`), 0o644)
	empty, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.AddRun(Run{RunID: "r2"}, []domain.Match{match("b2", "l2")}); err != nil {
		t.Errorf("a file with no matches: %v", err)
	}
	if err := s.Save(context.Background(), filepath.Join(path, "nested")); err == nil || !strings.Contains(err.Error(), "saving state "+filepath.Join(path, "nested")) {
		t.Errorf("a failed save must say so: %v", err)
	}
}

func TestLoadRejectsBadState(t *testing.T) {
	const twice = "pairs a record more than once"
	for name, tt := range map[string]struct{ content, want string }{
		"not JSON":         {"hello", "unreadable"},
		"cut short":        {`{"version": 1, "id_scheme": 1, "matches": {`, "unreadable"},
		"empty file":       {"", "unreadable"},
		"null":             {"null", "is not a recon state file"},
		"some other JSON":  {`{"id_scheme": 1, "matches": {}}`, "is not a recon state file"},
		"no identities":    {`{"version": 1, "matches": {}}`, "version 1 and identity scheme 0"},
		"newer format":     {`{"version": 2, "id_scheme": 1}`, "version 2 and identity scheme 1; this program reads 1 and 1"},
		"newer identities": {`{"version": 1, "id_scheme": 2}`, "version 1 and identity scheme 2"},
		"bank record twice": {`{"version": 1, "id_scheme": 1, "matches": {"m1": {"bank_id": "b", "ledger_id": "l1"}, ` +
			`"m2": {"bank_id": "b", "ledger_id": "l2"}}}`, twice},
		"ledger record twice": {`{"version": 1, "id_scheme": 1, "matches": {"m1": {"bank_id": "b1", "ledger_id": "l"}, ` +
			`"m2": {"bank_id": "b2", "ledger_id": "l"}}}`, twice},
	} {
		path := filepath.Join(t.TempDir(), "state.json")
		os.WriteFile(path, []byte(tt.content), 0o644)
		// Every refusal names the state file it refers to.
		if _, err := Load(path); err == nil || !strings.HasPrefix(err.Error(), path+" ") || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error about %s saying %q", name, err, path, tt.want)
		}
	}
	// A state that exists but cannot be read must never pass for a fresh one.
	if _, err := Load(t.TempDir()); err == nil || !strings.Contains(err.Error(), "reading state") {
		t.Errorf("a folder loaded as a state: %v", err)
	}
}

func TestLock(t *testing.T) {
	day, stamp := []string{"2006-01-02"}, []string{"2006-01-02T15:04:05Z07:00"}
	var s State
	if err := s.Lock("UTC", day, stamp); err != nil || s.TZ != "UTC" {
		t.Fatalf("the first run must set the lock: %v", err)
	}
	// A refusal names the settings the state was built with.
	stored := fmt.Sprintf("-tz UTC, bank date formats %q and ledger date formats %q", day, stamp)
	for _, tt := range []struct {
		tz           string
		bank, ledger []string
		ok           bool
	}{
		{"UTC", day, stamp, true},
		{"Europe/Berlin", day, stamp, false},
		{"UTC", stamp, stamp, false},
		{"UTC", day, day, false},
	} {
		if err := s.Lock(tt.tz, tt.bank, tt.ledger); (err == nil) != tt.ok || err != nil && !strings.Contains(err.Error(), stored) {
			t.Errorf("Lock(%s, %v, %v) = %v, want accepted %v", tt.tz, tt.bank, tt.ledger, err, tt.ok)
		}
	}
}

func TestAddRun(t *testing.T) {
	s, _ := Load(filepath.Join(t.TempDir(), "state.json"))
	if err := s.AddRun(Run{RunID: "r1"}, []domain.Match{match("b1", "l1"), match("b2", "l2")}); err != nil {
		t.Fatal(err)
	}
	// A pairing stored before, or repeated, is skipped; a new one is added.
	if err := s.AddRun(Run{RunID: "r2"}, []domain.Match{match("b1", "l1"), match("b3", "l3"), match("b3", "l3")}); err != nil {
		t.Fatal(err)
	}
	committed := func(bank, ledger, run string) Committed {
		return Committed{BankID: bank, LedgerID: ledger, Rule: domain.RuleExact, RunID: run}
	}
	want := State{
		Runs: []Run{{RunID: "r1", NewMatches: 2}, {RunID: "r2", NewMatches: 1}},
		Matches: map[string]Committed{
			match("b1", "l1").MatchID: committed("b1", "l1", "r1"),
			match("b2", "l2").MatchID: committed("b2", "l2", "r1"),
			match("b3", "l3").MatchID: committed("b3", "l3", "r2"),
		},
	}
	if diff := cmp.Diff(want, State{Runs: s.Runs, Matches: s.Matches}); diff != "" {
		t.Errorf("state (-want +got):\n%s", diff)
	}
	bank, ledger := s.MatchedIDs()
	if len(bank) != 3 || !bank["b3"] || len(ledger) != 3 || !ledger["l3"] {
		t.Errorf("matched identities = %v, %v", bank, ledger)
	}
	// No record may be paired twice, whether with the stored state or within one run.
	for name, batch := range map[string][]domain.Match{
		"bank record again":     {match("b1", "l9")},
		"ledger record again":   {match("b9", "l1")},
		"bank twice in a run":   {match("b8", "l8"), match("b8", "l7")},
		"ledger twice in a run": {match("b8", "l8"), match("b7", "l8")},
	} {
		last := batch[len(batch)-1]
		err := s.AddRun(Run{RunID: "bad"}, batch)
		if err == nil || !strings.Contains(err.Error(), last.BankID+" or "+last.LedgerID) || len(s.Matches) != 3 || len(s.Runs) != 2 {
			t.Errorf("%s: accepted or left partly applied (%v)", name, err)
		}
		if !errors.Is(err, domain.ErrConflict) {
			t.Errorf("%s: error is not ErrConflict: %v", name, err)
		}
	}
}
