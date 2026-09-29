// Package report builds and writes results.json.
package report

import (
	"cmp"
	"encoding/json"
	"slices"

	"github.com/mehmetkr/recon-go/internal/domain"
	"github.com/mehmetkr/recon-go/internal/match"
	"github.com/mehmetkr/recon-go/internal/store"
)

// Config records the settings that shaped the result.
type Config struct {
	store.Config
	ConfigHash string `json:"config_hash"`
}

// Summary counts every outcome, showing zero where nothing happened.
type Summary struct {
	Read       map[domain.Source]int                   `json:"read"`
	Matches    map[domain.Rule]int                     `json:"matches"`
	Exceptions map[domain.Source]map[domain.Reason]int `json:"exceptions"`
	Malformed  map[domain.Source]int                   `json:"malformed"`
	Excluded   map[domain.Source]int                   `json:"excluded"`
}

// Report is the content of results.json.
type Report struct {
	RunID      string                `json:"run_id"`
	Config     Config                `json:"config"`
	Summary    Summary               `json:"summary"`
	Matches    []domain.Match        `json:"matches"`
	Exceptions []domain.Exception    `json:"exceptions"`
	Malformed  []domain.RowError     `json:"malformed"`
	Excluded   map[domain.Source]int `json:"excluded"`
}

// Input is everything a report is built from.
type Input struct {
	RunID     string
	Config    store.Config
	Read      map[domain.Source]int // records read, including those set aside
	Excluded  map[domain.Source]int
	Result    match.Result
	Malformed []domain.RowError
}

var sources = []domain.Source{domain.Bank, domain.Ledger}

// Build assembles a report in a fixed order, with every list present.
func Build(in Input) Report {
	r := Report{
		RunID:      in.RunID,
		Config:     Config{Config: in.Config, ConfigHash: store.ConfigHash(in.Config)},
		Matches:    nonNil(slices.Clone(in.Result.Matches)),
		Exceptions: nonNil(slices.Clone(in.Result.Exceptions)),
		Malformed:  nonNil(slices.Clone(in.Malformed)),
		Excluded:   countsBySource(in.Excluded),
	}
	for i := range r.Exceptions {
		r.Exceptions[i].Candidates = nonNil(r.Exceptions[i].Candidates)
	}
	for i := range r.Malformed {
		r.Malformed[i].Raw = nonNil(r.Malformed[i].Raw)
	}
	slices.SortFunc(r.Malformed, func(a, b domain.RowError) int {
		return cmp.Or(cmp.Compare(a.Source, b.Source), slices.Compare(a.Raw, b.Raw),
			cmp.Compare(a.Reason, b.Reason), cmp.Compare(a.Detail, b.Detail), cmp.Compare(a.Line, b.Line))
	})

	s := Summary{
		Read:       countsBySource(in.Read),
		Matches:    map[domain.Rule]int{},
		Exceptions: map[domain.Source]map[domain.Reason]int{},
		Malformed:  countsBySource(nil),
		Excluded:   countsBySource(in.Excluded),
	}
	for _, rule := range domain.Rules {
		s.Matches[rule] = 0
	}
	for _, m := range r.Matches {
		s.Matches[m.Rule]++
	}
	for _, src := range sources {
		s.Exceptions[src] = map[domain.Reason]int{}
		for _, reason := range domain.Reasons {
			s.Exceptions[src][reason] = 0
		}
	}
	for _, e := range r.Exceptions {
		s.Exceptions[e.Source][e.Reason]++
	}
	for _, m := range r.Malformed {
		s.Malformed[m.Source]++
	}
	r.Summary = s
	return r
}

// Encode writes the report as readable JSON, identical for identical results.
func Encode(r Report) ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func countsBySource(m map[domain.Source]int) map[domain.Source]int {
	out := map[domain.Source]int{}
	for _, src := range sources {
		out[src] = m[src]
	}
	return out
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
