// Package recon orchestrates a reconciliation run.
package recon

import (
	"context"

	"github.com/mehmetkr/recon-go/internal/domain"
	"github.com/mehmetkr/recon-go/internal/match"
	"github.com/mehmetkr/recon-go/internal/report"
	"github.com/mehmetkr/recon-go/internal/store"
)

// MatchFunc is the signature of the concurrent matcher.
type MatchFunc func(ctx context.Context, bank, ledger, taken []domain.Transaction, p match.Params, workers int) (match.Result, error)

// Input holds everything the caller must provide for a reconciliation run.
type Input struct {
	BankRaw       []byte
	LedgerRaw     []byte
	Bank          []domain.Transaction
	Ledger        []domain.Transaction
	BankErrs      []domain.RowError
	LedgerErrs    []domain.RowError
	TZ            string
	BankLayouts   []string
	LedgerLayouts []string
	Params        match.Params
	Workers       int
	Force         bool
}

// Output is the result of a successful reconciliation.
type Output struct {
	Report  report.Report
	Encoded []byte
	Skipped bool
}

// Service runs reconciliation.
type Service struct {
	Matcher MatchFunc
}

// Reconcile matches bank and ledger records, commits the run to state in
// memory, and returns the encoded report. The caller loaded and locked the
// state before this call, and is responsible for persisting the report and
// the mutated state afterward.
func (s *Service) Reconcile(ctx context.Context, state *store.State, in Input) (*Output, error) {
	cfg := store.Config{
		AlgoVersion:   store.AlgoVersion,
		TZ:            in.TZ,
		BankLayouts:   in.BankLayouts,
		LedgerLayouts: in.LedgerLayouts,
		DateTolerance: in.Params.DateTolerance,
		FuzzyWindow:   in.Params.FuzzyWindow,
		MinRefLen:     match.MinRefLen,
		ThresholdNum:  match.ThresholdNum,
		ThresholdDen:  match.ThresholdDen,
	}
	entry := store.Run{
		BankSHA:    store.FileHash(in.BankRaw),
		LedgerSHA:  store.FileHash(in.LedgerRaw),
		ConfigHash: store.ConfigHash(cfg),
	}
	entry.RunID = store.RunID(entry.BankSHA, entry.LedgerSHA, entry.ConfigHash)

	if state.HasRun(entry.RunID) && !in.Force {
		return &Output{Skipped: true, Report: report.Report{RunID: entry.RunID}}, nil
	}

	matchedBank, matchedLedger := state.MatchedIDs()
	freeBank, takenBank := setAside(in.Bank, matchedBank)
	freeLedger, takenLedger := setAside(in.Ledger, matchedLedger)

	res, err := s.Matcher(ctx, freeBank, freeLedger, append(takenBank, takenLedger...), in.Params, in.Workers)
	if err != nil {
		return nil, err
	}

	rep := report.Build(report.Input{
		RunID:     entry.RunID,
		Config:    cfg,
		Read:      map[domain.Source]int{domain.Bank: len(in.Bank), domain.Ledger: len(in.Ledger)},
		Excluded:  map[domain.Source]int{domain.Bank: len(takenBank), domain.Ledger: len(takenLedger)},
		Result:    res,
		Malformed: append(in.BankErrs, in.LedgerErrs...),
	})
	encoded, err := report.Encode(rep)
	if err != nil {
		return nil, err
	}
	if err := state.AddRun(entry, res.Matches); err != nil {
		return nil, err
	}

	return &Output{Report: rep, Encoded: encoded}, nil
}

// setAside splits records into those still free and those paired in an earlier run.
func setAside(records []domain.Transaction, matched map[string]bool) (free, taken []domain.Transaction) {
	for _, t := range records {
		if matched[t.ID] {
			taken = append(taken, t)
		} else {
			free = append(free, t)
		}
	}
	return free, taken
}
