package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"

	"github.com/mehmetkr/recon-go/internal/domain"
)

// Store loads and saves reconciliation state.
type Store interface {
	Load(path string) (*State, error)
	Save(ctx context.Context, state *State, path string) error
}

// FileStore implements Store with JSON files.
type FileStore struct{}

// Load reads the state file; a missing file is an empty state.
func (FileStore) Load(path string) (*State, error) { return Load(path) }

// Save writes the state file all at once.
func (FileStore) Save(ctx context.Context, state *State, path string) error {
	return state.Save(ctx, path)
}

// The versions of the state format and of the record identity scheme.
const (
	stateVersion = 1
	idScheme     = 1
)

// Run records one reconciliation that was committed.
type Run struct {
	RunID      string `json:"run_id"`
	BankSHA    string `json:"bank_sha256"`
	LedgerSHA  string `json:"ledger_sha256"`
	ConfigHash string `json:"config_hash"`
	NewMatches int    `json:"new_matches"`
}

// Committed is a stored match.
type Committed struct {
	BankID   string      `json:"bank_id"`
	LedgerID string      `json:"ledger_id"`
	Rule     domain.Rule `json:"rule"`
	RunID    string      `json:"run_id"`
}

// State is everything remembered between runs.
type State struct {
	Version       int                  `json:"version"`
	IDScheme      int                  `json:"id_scheme"`
	TZ            string               `json:"tz"`
	BankLayouts   []string             `json:"bank_layouts"`
	LedgerLayouts []string             `json:"ledger_layouts"`
	Runs          []Run                `json:"runs"`
	Matches       map[string]Committed `json:"matches"`
}

// Load reads the state file; a missing file is an empty state.
func Load(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &State{Version: stateVersion, IDScheme: idScheme, Matches: map[string]Committed{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading state: %w", err)
	}
	s := &State{}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("%s is unreadable: %w", path, err)
	}
	if s.Version == 0 {
		return nil, fmt.Errorf("%s is not a recon state file", path)
	}
	if s.Version != stateVersion || s.IDScheme != idScheme {
		return nil, fmt.Errorf("%s has version %d and identity scheme %d; this program reads %d and %d",
			path, s.Version, s.IDScheme, stateVersion, idScheme)
	}
	if s.Matches == nil {
		s.Matches = map[string]Committed{}
	}
	if bank, ledger := s.MatchedIDs(); len(bank) != len(s.Matches) || len(ledger) != len(s.Matches) {
		return nil, fmt.Errorf("%s pairs a record more than once", path)
	}
	return s, nil
}

// Lock pins the time zone and date formats on first use; later runs must repeat them, since they shape record identities.
func (s *State) Lock(tz string, bankLayouts, ledgerLayouts []string) error {
	if s.TZ == "" {
		s.TZ, s.BankLayouts, s.LedgerLayouts = tz, bankLayouts, ledgerLayouts
		return nil
	}
	if s.TZ != tz || !slices.Equal(s.BankLayouts, bankLayouts) || !slices.Equal(s.LedgerLayouts, ledgerLayouts) {
		return fmt.Errorf("this state was built with -tz %s, bank date formats %q and ledger date formats %q; "+
			"use the same settings or a new state file", s.TZ, s.BankLayouts, s.LedgerLayouts)
	}
	return nil
}

// HasRun reports whether a run with this identity was already committed.
func (s *State) HasRun(runID string) bool {
	return slices.ContainsFunc(s.Runs, func(r Run) bool { return r.RunID == runID })
}

// MatchedIDs returns the identities already paired, per side.
func (s *State) MatchedIDs() (bank, ledger map[string]bool) {
	bank, ledger = map[string]bool{}, map[string]bool{}
	for _, m := range s.Matches {
		bank[m.BankID], ledger[m.LedgerID] = true, true
	}
	return bank, ledger
}

// AddRun records a run and its new matches, refusing any record matched twice and changing nothing if it refuses.
func (s *State) AddRun(run Run, matches []domain.Match) error {
	bank, ledger := s.MatchedIDs()
	var added []domain.Match
	seen := map[string]bool{}
	for _, m := range matches {
		if _, ok := s.Matches[m.MatchID]; ok || seen[m.MatchID] {
			continue
		}
		seen[m.MatchID] = true
		if bank[m.BankID] || ledger[m.LedgerID] {
			return fmt.Errorf("record matched twice: %s or %s is already paired", m.BankID, m.LedgerID)
		}
		bank[m.BankID], ledger[m.LedgerID] = true, true
		added = append(added, m)
	}
	for _, m := range added {
		s.Matches[m.MatchID] = Committed{BankID: m.BankID, LedgerID: m.LedgerID, Rule: m.Rule, RunID: run.RunID}
	}
	run.NewMatches = len(added)
	s.Runs = append(s.Runs, run)
	return nil
}

// Save writes the state file all at once.
func (s *State) Save(ctx context.Context, path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := WriteAtomic(ctx, path, append(b, '\n')); err != nil {
		return fmt.Errorf("saving state %s: %w", path, err)
	}
	return nil
}
